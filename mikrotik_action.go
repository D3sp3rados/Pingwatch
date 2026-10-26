package main

// MikroTik-Aktionen über einen eigenen Benutzer mit Schreibrechten (getrennt vom Lese-Benutzer):
// Neustart des Geräts, PoE-Neustart eines Ports, Port kurz aus- und wieder einschalten.
// Erlaubt nur für ausdrücklich freigegebene Ports (MtActPorts) bzw. wenn Neustart freigegeben ist.
// Ablauf: Dashboard → POST /api/mikrotik/action → Auftrag „mtaction“ an den Agenten → Ergebnis über /api/jobs.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var mtActions = map[string]string{"reboot": "Neustart", "poe": "PoE-Neustart", "bounce": "Port aus/an"}

// mtActPortOK: Port für Aktionen freigegeben? („*“ = alle)
func mtActPortOK(hc HostCfg, port string) bool {
	port = strings.TrimSpace(port)
	if port == "" {
		return false
	}
	for _, p := range strings.Split(hc.MtActPorts, ",") {
		p = strings.TrimSpace(p)
		if p == "*" || strings.EqualFold(p, port) {
			return true
		}
	}
	return false
}

// ---------- Agent ----------

func mtRunAction(hc HostCfg, action, port string) (string, error) {
	if hc.MtActUser == "" || hc.MtActPass == "" {
		return "", errors.New("kein Aktions-Benutzer hinterlegt")
	}
	c := newMtClient(hc.Addr, hc.MtPort, hc.MtActUser, hc.MtActPass)
	var out any
	switch action {
	case "reboot":
		err := c.do("POST", "/system/reboot", map[string]any{}, &out)
		if err != nil && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") {
			return "", err
		}
		return "Neustart ausgelöst", nil
	case "poe":
		if err := c.do("POST", "/interface/ethernet/poe/power-cycle", map[string]any{"numbers": port, "duration": "5s"}, &out); err != nil {
			return "", err
		}
		return "PoE an " + port + " für 5 s ausgeschaltet", nil
	case "bounce":
		if err := c.do("POST", "/interface/disable", map[string]any{"numbers": port}, &out); err != nil {
			return "", err
		}
		time.Sleep(5 * time.Second)
		var err error
		for i := 0; i < 3; i++ { // Einschalten auf jeden Fall nochmal versuchen
			if err = c.do("POST", "/interface/enable", map[string]any{"numbers": port}, &out); err == nil {
				return port + " für 5 s aus- und wieder eingeschaltet", nil
			}
			time.Sleep(3 * time.Second)
		}
		return "", fmt.Errorf("%s wurde ausgeschaltet, Einschalten fehlgeschlagen: %v – bitte sofort prüfen", port, err)
	}
	return "", errors.New("unbekannte Aktion")
}

func (a *Agent) mtActionJob(j agentJob) (any, error) {
	a.mu.Lock()
	ah := a.hosts[j.Params["host"]]
	a.mu.Unlock()
	if ah == nil || !ah.cfg.Mt {
		return nil, errors.New("Host ist am Agenten nicht (mehr) als MikroTik eingerichtet")
	}
	hc := ah.cfg
	if j.Params["action"] == "reboot" && !hc.MtActReboot {
		return nil, errors.New("Neustart ist für diesen Host nicht freigegeben")
	}
	if j.Params["action"] != "reboot" && !mtActPortOK(hc, j.Params["port"]) {
		return nil, errors.New("Port ist für Aktionen nicht freigegeben")
	}
	msg, err := mtRunAction(hc, j.Params["action"], j.Params["port"])
	if err != nil {
		return nil, err
	}
	return map[string]string{"msg": msg}, nil
}

// ---------- Server ----------

// POST /api/mikrotik/action {host, action, port}
func (s *Server) handleMikrotikAction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req struct {
		Host, Action, Port string
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !p.canEdit() {
		http.Error(w, "keine Berechtigung", 403)
		return
	}
	h := s.hostFor(r, req.Host)
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	h.mu.Lock()
	hc := h.cfg
	h.mu.Unlock()
	label, ok := mtActions[req.Action]
	switch {
	case !ok:
		http.Error(w, "unbekannte Aktion", 400)
		return
	case !hc.Mt || hc.MtActUser == "" || hc.MtActPass == "":
		http.Error(w, "Für diesen Host ist kein Aktions-Benutzer eingerichtet", 400)
		return
	case req.Action == "reboot" && !hc.MtActReboot:
		http.Error(w, "Neustart ist für diesen Host nicht freigegeben", 403)
		return
	case req.Action != "reboot" && !mtActPortOK(hc, req.Port):
		http.Error(w, "Dieser Port ist für Aktionen nicht freigegeben", 403)
		return
	}
	if on, _ := s.m.siteOnline(hc.Site); !on {
		http.Error(w, "Der Agent des Standorts ist gerade nicht verbunden", 409)
		return
	}
	who := "Betreiber"
	if p.User != nil {
		who = p.User.Name
		if who == "" {
			who = p.User.Email
		}
	}
	what := hc.Name + " · " + label
	if req.Action != "reboot" {
		what += " " + req.Port
	}
	now := time.Now().UnixMilli()
	s.m.closeSiteEvent(s.m.newHostEvent(hc, "action", what+" (von "+who+")", now, true), now)
	j := agentJob{ID: randHex(8), Kind: "mtaction", Params: map[string]string{"host": hc.ID, "action": req.Action, "port": strings.TrimSpace(req.Port)}}
	jobs.Lock()
	jobs.byID[j.ID] = &jobState{agentJob: j, Org: hc.Org, Site: hc.Site, Status: "queued", Created: now}
	jobs.queue[hc.Site] = append(jobs.queue[hc.Site], j)
	jobs.Unlock()
	writeJSON(w, map[string]string{"id": j.ID})
}

package main

// Fritzbox-Aktionen per TR-064 (nur wenn am Host „Aktionen erlauben“ gesetzt ist):
// Neustart, Neu einwählen, WLAN ein/aus, Wake-on-LAN, Firmware-Update, Rufumleitung ein/aus.
// Ablauf wie bei MikroTik: Dashboard → POST /api/fritz/action → Auftrag „fritzaction“ an den
// Agenten → Ergebnis über /api/jobs. Werksreset gibt es bewusst nicht.
// Bei Neustart/Neueinwahl wird der Host samt abhängigen Hosts kurz stummgeschaltet.

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var fritzActions = map[string]string{
	"reboot":    "Neustart",
	"reconnect": "Neu einwählen",
	"wlan":      "WLAN",
	"wol":       "Wake-on-LAN",
	"update":    "Firmware-Update",
	"defl":      "Rufumleitung",
}

// Stummschaltung nach der Aktion (Box bzw. Leitung kurz weg)
var fritzActMute = map[string]time.Duration{"reboot": 8 * time.Minute, "reconnect": 3 * time.Minute, "update": 15 * time.Minute}

var macRe = regexp.MustCompile(`^[0-9A-Fa-f]{2}([:-][0-9A-Fa-f]{2}){5}$`)

// fritzActionCheck prüft die Parameter (Server und Agent).
func fritzActionCheck(action string, p map[string]string) error {
	switch action {
	case "reboot", "reconnect", "update":
		return nil
	case "wlan":
		if n, err := strconv.Atoi(p["idx"]); err != nil || n < 1 || n > 4 {
			return errors.New("ungültiges WLAN")
		}
	case "wol":
		if !macRe.MatchString(p["mac"]) {
			return errors.New("ungültige MAC-Adresse")
		}
	case "defl":
		if _, err := strconv.Atoi(p["id"]); err != nil {
			return errors.New("ungültige Rufumleitung")
		}
	default:
		return errors.New("unbekannte Aktion")
	}
	if action != "wol" && p["on"] != "1" && p["on"] != "0" {
		return errors.New("an oder aus?")
	}
	return nil
}

// netGone: Verbindung brach während der Aktion ab (bei Neustart/Neueinwahl erwartet).
func netGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "EOF") || strings.Contains(s, "reset") || strings.Contains(s, "timeout") || strings.Contains(s, "Timeout")
}

// ---------- Agent ----------

func fritzRunAction(c *tr064Client, action string, p map[string]string) (string, error) {
	onTxt := map[string]string{"1": "eingeschaltet", "0": "ausgeschaltet"}[p["on"]]
	switch action {
	case "reboot":
		if _, err := c.call("/upnp/control/deviceconfig", "urn:dslforum-org:service:DeviceConfig:1", "Reboot"); err != nil && !netGone(err) {
			return "", err
		}
		return "Neustart ausgelöst – die Box ist 2–3 Minuten nicht erreichbar", nil
	case "reconnect":
		// aktive Verbindung (PPP bei DSL, sonst IP) trennen – die Box wählt sofort neu ein
		for _, x := range [][2]string{{"/upnp/control/wanpppconn1", svcPPP}, {"/upnp/control/wanipconnection1", svcIP}} {
			v, err := c.call(x[0], x[1], "GetStatusInfo")
			if err != nil || v["NewConnectionStatus"] != "Connected" {
				continue
			}
			if _, err := c.call(x[0], x[1], "ForceTermination"); err != nil && !netGone(err) && !strings.Contains(err.Error(), "DisconnectInProgress") {
				return "", err
			}
			return "Verbindung getrennt – die Box wählt neu ein (ca. 10–30 s)", nil
		}
		return "", errors.New("keine aktive Internetverbindung gefunden")
	case "wlan":
		i := p["idx"]
		if _, err := c.callArgs("/upnp/control/wlanconfig"+i, "urn:dslforum-org:service:WLANConfiguration:"+i, "SetEnable", map[string]string{"NewEnable": p["on"]}); err != nil {
			return "", err
		}
		return "WLAN " + i + " " + onTxt, nil
	case "wol":
		if _, err := c.callArgs("/upnp/control/hosts", "urn:dslforum-org:service:Hosts:1", "X_AVM-DE_WakeOnLANByMACAddress", map[string]string{"NewMACAddress": strings.ToUpper(strings.ReplaceAll(p["mac"], "-", ":"))}); err != nil {
			return "", err
		}
		return "Weckpaket an " + p["mac"] + " gesendet", nil
	case "update":
		v, err := c.call("/upnp/control/userif", "urn:dslforum-org:service:UserInterface:1", "GetInfo")
		if err == nil && v["NewUpgradeAvailable"] == "0" {
			return "", errors.New("die Box meldet kein verfügbares Update")
		}
		if _, err := c.call("/upnp/control/userif", "urn:dslforum-org:service:UserInterface:1", "X_AVM-DE_DoUpdate"); err != nil && !netGone(err) {
			return "", err
		}
		return "Update gestartet – die Box startet danach neu (5–10 Minuten)", nil
	case "defl":
		if _, err := c.callArgs("/upnp/control/x_contact", "urn:dslforum-org:service:X_AVM-DE_OnTel:1", "SetDeflectionEnable", map[string]string{"NewDeflectionId": p["id"], "NewEnable": p["on"]}); err != nil {
			return "", err
		}
		return "Rufumleitung " + onTxt, nil
	}
	return "", errors.New("unbekannte Aktion")
}

func (a *Agent) fritzActionJob(j agentJob) (any, error) {
	a.mu.Lock()
	ah := a.hosts[j.Params["host"]]
	a.mu.Unlock()
	if ah == nil || !ah.cfg.Fritz {
		return nil, errors.New("Host ist am Agenten nicht (mehr) als Fritzbox eingerichtet")
	}
	if !ah.cfg.FritzAct {
		return nil, errors.New("Aktionen sind für diese Fritzbox nicht freigegeben")
	}
	if err := fritzActionCheck(j.Params["action"], j.Params); err != nil {
		return nil, err
	}
	c := newTR064(fritzBase(ah.cfg.Addr, ah.cfg.FritzPort), ah.cfg.FritzUser, ah.cfg.FritzPass)
	msg, err := fritzRunAction(c, j.Params["action"], j.Params)
	if err != nil {
		if errors.Is(err, errFritzAuth) || strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "606") {
			return nil, fmt.Errorf("%v – der Fritzbox-Benutzer braucht das Recht „FRITZ!Box Einstellungen“", err)
		}
		return nil, err
	}
	return map[string]string{"msg": msg}, nil
}

// ---------- Server ----------

// fritzDependents: Host und alle (auch indirekt) abhängigen Hosts.
func (m *Monitor) fritzDependents(org, id string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out, seen := []string{id}, map[string]bool{id: true}
	for i := 0; i < len(out); i++ {
		for _, h := range m.cfg.Hosts {
			if h.Org == org && h.Parent == out[i] && !seen[h.ID] {
				seen[h.ID] = true
				out = append(out, h.ID)
			}
		}
	}
	return out
}

// POST /api/fritz/action {host, action, idx, on, mac, id}
func (s *Server) handleFritzAction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req struct {
		Host, Action, Idx, On, Mac, ID string
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
	params := map[string]string{"host": hc.ID, "action": req.Action, "idx": strings.TrimSpace(req.Idx), "on": strings.TrimSpace(req.On), "mac": strings.TrimSpace(req.Mac), "id": strings.TrimSpace(req.ID)}
	label, ok := fritzActions[req.Action]
	switch {
	case !ok:
		http.Error(w, "unbekannte Aktion", 400)
		return
	case !hc.Fritz:
		http.Error(w, "Dieser Host ist keine Fritzbox", 400)
		return
	case !hc.FritzAct:
		http.Error(w, "Aktionen sind für diese Fritzbox nicht freigegeben (Host bearbeiten → Fritzbox → „Aktionen erlauben“)", 403)
		return
	}
	if err := fritzActionCheck(req.Action, params); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if on, _ := s.m.siteOnline(hc.Site); !on {
		http.Error(w, "Der Agent des Standorts ist gerade nicht verbunden", 409)
		return
	}
	who := whoOf(p)
	what := hc.Name + " · " + label
	switch req.Action {
	case "wlan":
		what += " " + req.Idx + map[string]string{"1": " ein", "0": " aus"}[req.On]
	case "defl":
		what += map[string]string{"1": " ein", "0": " aus"}[req.On]
	case "wol":
		what += " " + strings.ToUpper(req.Mac)
	}
	now := time.Now()
	nowMs := now.UnixMilli()
	// Box bzw. Leitung ist gleich kurz weg: Host und abhängige Hosts stummschalten
	if d := fritzActMute[req.Action]; d > 0 {
		ids := s.m.fritzDependents(hc.Org, hc.ID)
		if hc.FritzPort == 49000 || hc.FritzPort == 49443 {
			// Box im LAN des Agenten = meist sein Internetzugang: auch „Standort offline“ stummschalten
			ids = append(ids, "s:"+hc.Site)
		}
		x := Maint{ID: randHex(6), Org: hc.Org, Name: "🔕 " + hc.Name + " · " + label, Hosts: ids, Start: nowMs - 1000, End: now.Add(d).UnixMilli(), Created: nowMs}
		s.m.mu.Lock()
		s.m.cfg.Maints = append(s.m.cfg.Maints, x)
		s.m.saveConfigLocked()
		s.m.mu.Unlock()
	}
	s.m.closeSiteEvent(s.m.newHostEvent(hc, "action", what+" (von "+who+")", nowMs, true), nowMs)
	s.m.audit(hc.Org, who, "Fritzbox-Aktion", "Host", hc.Name, what)
	j := agentJob{ID: randHex(8), Kind: "fritzaction", Params: params}
	jobs.Lock()
	jobs.byID[j.ID] = &jobState{agentJob: j, Org: hc.Org, Site: hc.Site, Status: "queued", Created: nowMs}
	jobs.queue[hc.Site] = append(jobs.queue[hc.Site], j)
	jobs.Unlock()
	writeJSON(w, map[string]any{"id": j.ID, "muteMin": int(fritzActMute[req.Action] / time.Minute)})
}

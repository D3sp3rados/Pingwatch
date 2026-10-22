package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// UniFi über die Cloud (ohne Agent vor Ort): Der PingWacht-Server fragt die Konsole über den
// Cloud-Proxy von Ubiquiti ab (api.ui.com/v1/connector/consoles/<id>/proxy/network/…) – mit einem
// API-Key aus unifi.ui.com (Site Manager). Die Daten laufen in dieselbe Verarbeitung wie beim
// Agenten (ingestUniFi): Livestatus, Verlauf, Meldungen, Wartung und Stummschaltung gelten gleich.

const (
	unifiModeCloud    = "cloud" // UniFiCfg.Mode
	siteKindUniFi     = "unifi" // SiteCfg.Kind: Standort ohne Agent, nur UniFi-Cloud
	unifiCloudEvery   = 60 * time.Second
	unifiCloudWorkers = 4
	unifiCloudMaxAll  = 3               // mehr Sites auf einer Konsole → Site muss gewählt werden (sonst Drosselung)
	unifiCloudStatAge = 5 * time.Minute // Statistik je Gerät über die Cloud bei großen Standorten nur alle 5 Minuten neu
	unifiCloudStatFew = 20              // bis zu so vielen Geräten am Standort: Statistik jede Minute
)

func (c *UniFiCfg) isCloud() bool { return c != nil && c.Mode == unifiModeCloud }

// unifiCloudKey liefert den Key für einen Standort: eigener Key am Standort, sonst der Key des Kunden.
func (m *Monitor) unifiCloudKey(site SiteCfg) string {
	if site.UniFi != nil && strings.TrimSpace(site.UniFi.Key) != "" {
		return strings.TrimSpace(site.UniFi.Key)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.UniFiKeys[site.Org]
}

// ---------- Konsolenliste (Site Manager API) ----------

type unifiConsole struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	Type     string `json:"type,omitempty"`
	Firmware string `json:"fw,omitempty"`
	IP       string `json:"ip,omitempty"`
	Online   bool   `json:"online"`
}

func unifiConsoles(key string) ([]unifiConsole, error) {
	u := &unifiAPI{base: unifiCloudBase, key: strings.TrimSpace(key), cl: unifiCloudClient, cloud: true}
	if u.key == "" {
		return nil, errors.New("Kein UniFi-Cloud-Key hinterlegt")
	}
	out := []unifiConsole{}
	next := ""
	for page := 0; page < 20; page++ {
		path := "/v1/hosts?pageSize=100"
		if next != "" {
			path += "&nextToken=" + url.QueryEscape(next)
		}
		code, b, err := u.raw(path)
		if err != nil {
			return nil, err
		}
		if code >= 300 {
			if code == 404 {
				return nil, errors.New("Site-Manager-API nicht gefunden (HTTP 404)")
			}
			return nil, u.httpErr(code)
		}
		var res struct {
			Data      []map[string]any `json:"data"`
			NextToken string           `json:"nextToken"`
		}
		if err := json.Unmarshal(b, &res); err != nil {
			return nil, errors.New("Antwort der UniFi-Cloud nicht lesbar")
		}
		for _, h := range res.Data {
			out = append(out, parseUniFiHost(h))
		}
		if res.NextToken == "" || res.NextToken == next {
			break
		}
		next = res.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func anyStr(v any) string {
	s, _ := v.(string)
	return s
}

func parseUniFiHost(h map[string]any) unifiConsole {
	c := unifiConsole{ID: anyStr(h["id"]), Type: anyStr(h["type"]), IP: anyStr(h["ipAddress"])}
	rs, _ := h["reportedState"].(map[string]any)
	if rs != nil {
		c.Name = anyStr(rs["name"])
		if c.Name == "" {
			c.Name = anyStr(rs["hostname"])
		}
		c.Firmware = anyStr(rs["version"])
		if c.Firmware == "" {
			c.Firmware = anyStr(rs["firmwareVersion"])
		}
		if hw, ok := rs["hardware"].(map[string]any); ok {
			c.Model = anyStr(hw["name"])
			if c.Model == "" {
				c.Model = anyStr(hw["shortname"])
			}
		}
		if c.IP == "" {
			c.IP = anyStr(rs["ip"])
		}
		st := strings.ToLower(anyStr(rs["state"]))
		c.Online = st == "connected" || st == "online"
	}
	if c.Name == "" {
		if ud, ok := h["userData"].(map[string]any); ok {
			c.Name = anyStr(ud["name"])
		}
	}
	if c.Name == "" {
		c.Name = c.ID
	}
	return c
}

// ---------- Server-Poller ----------

var unifiCloudPoll = struct {
	sync.Mutex
	next map[string]time.Time // Standort → nächste Abfrage (Drosselung nach HTTP 429)
	busy map[string]bool
}{next: map[string]time.Time{}, busy: map[string]bool{}}

// unifiCloudLoop fragt alle Standorte mit „UniFi über Cloud“ jede Minute ab.
func (m *Monitor) unifiCloudLoop() {
	sem := make(chan struct{}, unifiCloudWorkers)
	for {
		m.unifiCloudTick(sem, time.Now())
		time.Sleep(unifiCloudEvery)
	}
}

func (m *Monitor) unifiCloudTick(sem chan struct{}, now time.Time) {
	m.mu.RLock()
	var sites []SiteCfg
	for _, s := range m.cfg.Sites {
		if s.UniFi != nil && s.UniFi.On && s.UniFi.isCloud() {
			sites = append(sites, s)
		}
	}
	m.mu.RUnlock()
	for _, s := range sites {
		unifiCloudPoll.Lock()
		if unifiCloudPoll.busy[s.ID] || now.Before(unifiCloudPoll.next[s.ID]) {
			unifiCloudPoll.Unlock()
			continue
		}
		unifiCloudPoll.busy[s.ID] = true
		unifiCloudPoll.Unlock()
		sem <- struct{}{}
		go func(s SiteCfg) {
			defer func() {
				<-sem
				unifiCloudPoll.Lock()
				delete(unifiCloudPoll.busy, s.ID)
				unifiCloudPoll.Unlock()
			}()
			m.unifiCloudPollSite(s)
		}(s)
	}
}

// unifiCloudPollSite: eine Abfrage über die Cloud, Ergebnis wie vom Agenten verarbeiten.
func (m *Monitor) unifiCloudPollSite(s SiteCfg) {
	key := m.unifiCloudKey(s)
	if key == "" || s.UniFi.Console == "" {
		m.ingestUniFi(s, nil, "Kein UniFi-Cloud-Key oder keine Konsole gewählt")
		return
	}
	kk := "key:" + key
	unifiCloudPoll.Lock()
	paused := time.Now().Before(unifiCloudPoll.next[kk])
	unifiCloudPoll.Unlock()
	if paused {
		return // derselbe Key wird gerade von Ubiquiti gedrosselt
	}
	devs, err := newUniFiCloudAPI(s.UniFi.Console, key).fetch(s.UniFi.Site, true)
	var re *unifiRateErr
	if errors.As(err, &re) {
		wait := re.retry
		if wait < 2*time.Minute {
			wait = 2 * time.Minute
		}
		unifiCloudPoll.Lock()
		unifiCloudPoll.next[s.ID] = time.Now().Add(wait)
		unifiCloudPoll.next[kk] = time.Now().Add(wait) // alle Standorte mit diesem Key pausieren
		unifiCloudPoll.Unlock()
		log.Printf("UniFi-Cloud %s: gedrosselt, Pause %s", s.Name, wait)
		return // Drosselung ist kein Ausfall der Konsole
	}
	if err != nil {
		m.ingestUniFi(s, nil, err.Error())
		return
	}
	m.ingestUniFi(s, devs, "")
}

// ---------- Diagnose ----------

type unifiStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Warn bool   `json:"warn,omitempty"` // nicht schlimm (z. B. WAN-Details fehlen)
	Msg  string `json:"msg"`
}

// unifiCloudDiagnose prüft Schritt für Schritt, was über die Cloud geht.
func unifiCloudDiagnose(key, console, siteFilter string) (steps []unifiStep, siteNames []string) {
	add := func(name string, ok bool, msg string) { steps = append(steps, unifiStep{Name: name, OK: ok, Msg: msg}) }
	cons, err := unifiConsoles(key)
	if err != nil {
		add("UniFi-Cloud-Key", false, err.Error())
		return
	}
	add("UniFi-Cloud-Key", true, fmt.Sprintf("gültig – %d Konsole(n) im Konto", len(cons)))
	if console == "" {
		add("Konsole", false, "Keine Konsole gewählt")
		return
	}
	var c *unifiConsole
	for i := range cons {
		if cons[i].ID == console {
			c = &cons[i]
		}
	}
	switch {
	case c == nil:
		add("Konsole", false, "Konsole nicht in diesem Konto gefunden")
		return
	case !c.Online:
		add("Konsole", false, c.Name+" ist laut UniFi-Cloud offline")
		return
	default:
		add("Konsole", true, strings.TrimSpace(c.Name+" · "+c.Model+" · Firmware "+c.Firmware))
	}
	u := newUniFiCloudAPI(console, key)
	var sites struct {
		Data []struct {
			ID, Name, InternalReference string
		} `json:"data"`
	}
	if err := u.get(unifiIntAPI+"/sites?limit=200", &sites); err != nil {
		add("Zugriff über den Cloud-Proxy", false, err.Error())
		return
	}
	found := siteFilter == ""
	for _, s := range sites.Data {
		siteNames = append(siteNames, s.Name)
		if siteFilter != "" && (strings.EqualFold(siteFilter, s.Name) || siteFilter == s.ID || siteFilter == s.InternalReference) {
			found = true
		}
	}
	sort.Strings(siteNames)
	shown := siteNames
	if len(shown) > 8 {
		shown = append(append([]string{}, shown[:8]...), "…")
	}
	add("Zugriff über den Cloud-Proxy", true, fmt.Sprintf("%d UniFi-Site(s): %s", len(sites.Data), strings.Join(shown, ", ")))
	switch {
	case !found:
		add("UniFi-Site", false, "„"+siteFilter+"“ gibt es auf dieser Konsole nicht – bitte aus der Liste wählen")
		return
	case siteFilter == "" && len(sites.Data) > unifiCloudMaxAll:
		add("UniFi-Site", false, fmt.Sprintf("Die Konsole verwaltet %d Sites – bitte unten die UniFi-Site dieses Kunden wählen", len(sites.Data)))
		return
	}
	devs, err := u.fetch(siteFilter, false)
	if err != nil {
		add("Geräte", false, err.Error())
		return
	}
	add("Geräte", true, fmt.Sprintf("%d Geräte gefunden", len(devs)))
	// Statistik und WAN für ein Gerät bzw. das Gateway
	var stat, gw *unifiDevice
	for i := range devs {
		if stat == nil && strings.EqualFold(devs[i].State, "ONLINE") {
			stat = &devs[i]
		}
		if gw == nil && isGateway(devs[i]) {
			gw = &devs[i]
		}
	}
	if stat != nil {
		siteID := ""
		for _, s := range sites.Data {
			if s.Name == stat.Site {
				siteID = s.ID
			}
		}
		var st map[string]any
		if err := u.get(fmt.Sprintf("%s/sites/%s/devices/%s/statistics/latest", unifiIntAPI, siteID, stat.ID), &st); err != nil {
			add("Statistik (Laufzeit, CPU, RAM)", false, err.Error())
		} else {
			add("Statistik (Laufzeit, CPU, RAM)", true, "verfügbar")
		}
	}
	if gw != nil {
		legacy := "default"
		for _, s := range sites.Data {
			if s.Name == gw.Site && s.InternalReference != "" {
				legacy = s.InternalReference
			}
		}
		code, _, err := u.raw("/proxy/network/api/s/" + legacy + "/stat/device/" + gw.MAC)
		if err == nil && code == 200 {
			add("WAN-Details des Gateways", true, "verfügbar")
		} else {
			steps = append(steps, unifiStep{Name: "WAN-Details des Gateways", OK: true, Warn: true,
				Msg: "über die UniFi-Cloud nicht verfügbar – Geräte, Status und Statistik funktionieren trotzdem"})
		}
	}
	return
}

// ---------- API ----------

// GET /api/unifi/cloud → {keySet, consoles?} · POST {key} speichert den Key des Kunden (leer + remove = löschen)
func (s *Server) handleUniFiCloudKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		m.mu.RLock()
		set := m.cfg.UniFiKeys[p.Org] != ""
		m.mu.RUnlock()
		writeJSON(w, map[string]any{"keySet": set})
	case http.MethodPost:
		if !p.canEdit() {
			http.Error(w, "Keine Berechtigung", 403)
			return
		}
		var req struct {
			Key    string `json:"key"`
			Remove bool   `json:"remove"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		key := strings.TrimSpace(req.Key)
		if !req.Remove && key == "" {
			http.Error(w, "Bitte den API-Key einfügen", 400)
			return
		}
		if len(key) > 500 {
			http.Error(w, "Key zu lang", 400)
			return
		}
		m.mu.Lock()
		if m.cfg.UniFiKeys == nil {
			m.cfg.UniFiKeys = map[string]string{}
		}
		if req.Remove {
			delete(m.cfg.UniFiKeys, p.Org)
		} else {
			m.cfg.UniFiKeys[p.Org] = key
		}
		m.cfg.Version++
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "keySet": !req.Remove})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

// POST /api/unifi/consoles {key?, site?} → Konsolen des Kontos (Key: eingegeben, sonst Standort-, sonst Kunden-Key)
func (s *Server) handleUniFiConsoles(w http.ResponseWriter, r *http.Request) {
	key, ok := s.unifiReqKey(w, r)
	if !ok {
		return
	}
	cons, err := unifiConsoles(key)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, cons)
}

// POST /api/unifi/test {key?, site?, console, usite} → Diagnose Schritt für Schritt
func (s *Server) handleUniFiCloudTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key, Site, Console, USite string
	}
	if !readJSON(w, r, &req) {
		return
	}
	key, ok := s.keyFor(w, r, req.Key, req.Site)
	if !ok {
		return
	}
	steps, sites := unifiCloudDiagnose(key, strings.TrimSpace(req.Console), strings.TrimSpace(req.USite))
	writeJSON(w, map[string]any{"steps": steps, "sites": sites})
}

// POST /api/unifi/sites {key?, site?, console} → Namen der UniFi-Sites auf der Konsole
func (s *Server) handleUniFiCloudSites(w http.ResponseWriter, r *http.Request) {
	var req struct{ Key, Site, Console string }
	if !readJSON(w, r, &req) {
		return
	}
	key, ok := s.keyFor(w, r, req.Key, req.Site)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Console) == "" {
		http.Error(w, "Bitte zuerst eine Konsole wählen", 400)
		return
	}
	var sites struct {
		Data []struct{ Name string } `json:"data"`
	}
	if err := newUniFiCloudAPI(req.Console, key).get(unifiIntAPI+"/sites?limit=200", &sites); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	names := []string{}
	for _, x := range sites.Data {
		names = append(names, x.Name)
	}
	sort.Strings(names)
	writeJSON(w, names)
}

func (s *Server) unifiReqKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct{ Key, Site string }
	if !readJSON(w, r, &req) {
		return "", false
	}
	return s.keyFor(w, r, req.Key, req.Site)
}

func (s *Server) keyFor(w http.ResponseWriter, r *http.Request, key, siteID string) (string, bool) {
	p := principal(r)
	if !p.canEdit() {
		http.Error(w, "Keine Berechtigung", 403)
		return "", false
	}
	if k := strings.TrimSpace(key); k != "" {
		return k, true
	}
	if siteID != "" {
		if site := s.siteFor(r, siteID); site != nil && site.UniFi != nil && site.UniFi.Key != "" && site.UniFi.isCloud() {
			return site.UniFi.Key, true
		}
	}
	s.m.mu.RLock()
	k := s.m.cfg.UniFiKeys[p.Org]
	s.m.mu.RUnlock()
	if k == "" {
		http.Error(w, "Kein UniFi-Cloud-Key hinterlegt – bitte unter Einstellungen → Standorte eintragen", 400)
		return "", false
	}
	return k, true
}

// sanitizeSite: Geheimnisse nicht an den Browser geben.
func sanitizeSite(sc SiteCfg) SiteCfg {
	if sc.UniFi != nil {
		u := *sc.UniFi
		u.KeySet = u.Key != ""
		u.Key = ""
		sc.UniFi = &u
	}
	return sc
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// UniFi-Livestatus: Der Agent fragt die UniFi-Konsole am Standort jede Minute ab (Integration-API)
// und meldet alle Geräte. Der Server erkennt Geräte, die offline gehen, und alarmiert.

type UniFiCfg struct {
	On   bool   `json:"on"`
	URL  string `json:"url"`
	Key  string `json:"key,omitempty"`
	Site string `json:"site,omitempty"` // UniFi-Site (leer = alle)
}

type unifiSiteState struct {
	At      int64                 `json:"at"`
	Err     string                `json:"err,omitempty"`
	ErrN    int                   `json:"-"`
	Devices map[string]*unifiLive `json:"-"`
	open    map[string]*Event     // MAC → offenes Ereignis
	errEv   *Event                // Konsole nicht erreichbar
}

type unifiLive struct {
	unifiDevice
	Since int64 `json:"since"` // Status seit
}

var unifiLiveState = struct {
	sync.Mutex
	bySite map[string]*unifiSiteState
}{bySite: map[string]*unifiSiteState{}}

func unifiOffline(state string) bool {
	switch strings.ToUpper(state) {
	case "OFFLINE", "CONNECTION_INTERRUPTED", "ISOLATED", "DISCONNECTED":
		return true
	}
	return false
}

// ingestUniFi verarbeitet eine Abfrage des Agenten.
func (m *Monitor) ingestUniFi(site SiteCfg, devs []unifiDevice, errText string) {
	now := time.Now()
	nowMs := now.UnixMilli()
	inMaint := m.inMaint(site.Org, "", now)
	unifiLiveState.Lock()
	st := unifiLiveState.bySite[site.ID]
	if st == nil {
		st = &unifiSiteState{Devices: map[string]*unifiLive{}, open: map[string]*Event{}}
		unifiLiveState.bySite[site.ID] = st
	}
	st.At = nowMs
	var starts, ends []Event
	if errText != "" {
		st.Err = errText
		st.ErrN++
		if st.ErrN == 3 && st.errEv == nil { // 3 Minuten in Folge keine Antwort
			e := m.newSiteEvent(site, "device", "UniFi-Konsole", nowMs, inMaint)
			st.errEv = e
			starts = append(starts, *e)
		}
		unifiLiveState.Unlock()
		m.unifiNotify(site, starts, nil, errText)
		return
	}
	st.Err, st.ErrN = "", 0
	if st.errEv != nil {
		ends = append(ends, m.closeSiteEvent(st.errEv, nowMs))
		st.errEv = nil
	}
	seen := map[string]bool{}
	first := len(st.Devices) == 0
	for _, d := range devs {
		key := d.MAC
		if key == "" {
			key = d.Name
		}
		seen[key] = true
		old := st.Devices[key]
		if old == nil || old.State != d.State {
			st.Devices[key] = &unifiLive{unifiDevice: d, Since: nowMs}
		} else {
			old.unifiDevice = d
		}
		off := unifiOffline(d.State)
		e := st.open[key]
		switch {
		case off && e == nil && !first:
			name := d.Name
			if name == "" {
				name = d.Model + " " + d.MAC
			}
			ne := m.newSiteEvent(site, "device", name, nowMs, inMaint)
			st.open[key] = ne
			starts = append(starts, *ne)
		case off && e == nil && first:
			// beim ersten Abruf bereits offline: Ereignis ohne Alarm anlegen
			ne := m.newSiteEvent(site, "device", d.Name, nowMs, true)
			st.open[key] = ne
		case !off && e != nil:
			ends = append(ends, m.closeSiteEvent(e, nowMs))
			delete(st.open, key)
		}
	}
	for key := range st.Devices {
		if !seen[key] { // aus der Konsole entfernt
			delete(st.Devices, key)
			if e := st.open[key]; e != nil {
				m.closeSiteEvent(e, nowMs)
				delete(st.open, key)
			}
		}
	}
	unifiLiveState.Unlock()
	m.unifiNotify(site, starts, ends, "")
}

func (m *Monitor) newSiteEvent(site SiteCfg, typ, name string, at int64, maint bool) *Event {
	m.evMu.Lock()
	m.nextEv++
	e := &Event{ID: m.nextEv, Site: site.ID, HostName: name, Org: site.Org, Type: typ, Start: at, Last: at, Maint: maint, Decided: true, Notified: !maint}
	m.events = append(m.events, e)
	m.evMu.Unlock()
	return e
}

func (m *Monitor) closeSiteEvent(e *Event, at int64) Event {
	m.evMu.Lock()
	e.End, e.Last = at, at
	cp := *e
	m.evMu.Unlock()
	m.store.AppendEvent(cp)
	return cp
}

func (m *Monitor) unifiNotify(site SiteCfg, starts, ends []Event, errText string) {
	for _, e := range starts {
		if e.Maint {
			continue
		}
		body := "Gerät meldet sich nicht mehr bei der UniFi-Konsole (" + site.Name + ")"
		if errText != "" {
			body = "Der Agent erreicht die UniFi-Konsole nicht: " + errText
		}
		m.notify(notice{"devoff", "", "🔴 UniFi: " + e.HostName + " offline", body, site.Org})
	}
	for _, e := range ends {
		if !e.Notified {
			continue
		}
		m.notify(notice{"devon", "", "🟢 UniFi: " + e.HostName + " wieder online",
			fmt.Sprintf("War %s offline (%s)", fmtDurShort(e.End-e.Start), site.Name), site.Org})
	}
}

// ---------- API ----------

// POST /api/agent/unifi {devices, error}
func (s *Server) handleAgentUniFi(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Devices []unifiDevice `json:"devices"`
		Error   string        `json:"error"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	if site.UniFi == nil || !site.UniFi.On {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	s.m.ingestUniFi(site, req.Devices, req.Error)
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/unifi?site= → Geräteliste · POST /api/unifi {site, on, url, key, usite}
func (s *Server) handleUniFi(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		site := s.siteFor(r, r.URL.Query().Get("site"))
		if site == nil {
			http.Error(w, "Standort unbekannt", 404)
			return
		}
		writeJSON(w, unifiView(site))
		return
	}
	var req struct {
		Site, URL, Key, USite string
		On                    bool
	}
	if !readJSON(w, r, &req) {
		return
	}
	site := s.siteFor(r, req.Site)
	if site == nil {
		http.Error(w, "Standort unbekannt", 404)
		return
	}
	u := strings.TrimRight(strings.TrimSpace(req.URL), "/")
	if u != "" && !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if req.On && !strings.HasPrefix(u, "https://") {
		http.Error(w, "Adresse der UniFi-Konsole angeben, z. B. https://192.168.1.1", 400)
		return
	}
	m := s.m
	m.mu.Lock()
	for i := range m.cfg.Sites {
		sc := &m.cfg.Sites[i]
		if sc.ID != site.ID {
			continue
		}
		key := strings.TrimSpace(req.Key)
		if key == "" && sc.UniFi != nil {
			key = sc.UniFi.Key
		}
		if req.On && key == "" {
			m.mu.Unlock()
			http.Error(w, "API-Schlüssel fehlt", 400)
			return
		}
		sc.UniFi = &UniFiCfg{On: req.On, URL: u, Key: key, Site: strings.TrimSpace(req.USite)}
	}
	m.cfg.Version++
	m.saveConfigLocked()
	m.mu.Unlock()
	if !req.On {
		unifiLiveState.Lock()
		delete(unifiLiveState.bySite, site.ID)
		unifiLiveState.Unlock()
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type unifiSummary struct {
	On      bool        `json:"on"`
	URL     string      `json:"url,omitempty"`
	Site    string      `json:"site,omitempty"`
	At      int64       `json:"at,omitempty"`
	Err     string      `json:"err,omitempty"`
	Total   int         `json:"total"`
	Offline int         `json:"offline"`
	Devices []unifiLive `json:"devices,omitempty"`
}

func unifiView(site *SiteCfg) unifiSummary {
	v := unifiSummary{}
	if site.UniFi == nil {
		return v
	}
	v.On, v.URL, v.Site = site.UniFi.On, site.UniFi.URL, site.UniFi.Site
	unifiLiveState.Lock()
	defer unifiLiveState.Unlock()
	st := unifiLiveState.bySite[site.ID]
	if st == nil {
		return v
	}
	v.At, v.Err = st.At, st.Err
	for _, d := range st.Devices {
		v.Total++
		if unifiOffline(d.State) {
			v.Offline++
		}
		v.Devices = append(v.Devices, *d)
	}
	sort.Slice(v.Devices, func(i, j int) bool {
		oi, oj := unifiOffline(v.Devices[i].State), unifiOffline(v.Devices[j].State)
		if oi != oj {
			return oi
		}
		return strings.ToLower(v.Devices[i].Name) < strings.ToLower(v.Devices[j].Name)
	})
	return v
}

// ---------- Agent ----------

// unifiLoop fragt die Konsole jede Minute ab, solange sie in der Konfiguration steht.
func (a *Agent) unifiLoop() {
	for {
		a.mu.Lock()
		cfg := a.unifi
		a.mu.Unlock()
		if cfg != nil && cfg.On {
			devs, err := unifiFetch(cfg.URL, cfg.Key, cfg.Site, true)
			body := map[string]any{"devices": devs}
			if err != nil {
				body["error"] = err.Error()
			}
			b, _ := json.Marshal(body)
			if resp, e := a.req("POST", "/api/agent/unifi", bytes.NewReader(b), false); e == nil {
				resp.Body.Close()
			} else {
				log.Printf("UniFi-Status senden: %v", e)
			}
		}
		select {
		case <-a.stopAll:
			return
		case <-time.After(60 * time.Second):
		}
	}
}

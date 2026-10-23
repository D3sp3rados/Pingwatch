package main

import (
	"net/http"
	"strings"
	"time"
)

// Schnell-Stummschaltung: alle / Kunde / Host – nur Meldungen aus (Wartungsfenster)
// oder Prüfung pausieren, jeweils bis Zeitpunkt oder dauerhaft.

const foreverMs = int64(100 * 365 * 24 * 3600 * 1000)

type muteView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	End     int64  `json:"end"`
	Forever bool   `json:"forever"`
	All     bool   `json:"all"`
	Hosts   int    `json:"hosts"`
}

// activeMutesLocked: laufende einmalige Wartungsfenster des Kunden (m.mu muss gehalten werden).
func (m *Monitor) activeMutesLocked(org string, now time.Time) []muteView {
	out := []muteView{}
	for _, w := range m.cfg.Maints {
		if w.Org != org || w.recurring() || !w.activeAt(now) {
			continue
		}
		out = append(out, muteView{ID: w.ID, Name: w.Name, End: w.End, Forever: w.End-now.UnixMilli() > foreverMs/2,
			All: len(w.Hosts) == 0, Hosts: len(w.Hosts)})
	}
	return out
}

func (m *Monitor) activeMutes(org string) []muteView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeMutesLocked(org, time.Now())
}

func (s *Server) handleMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	p := principal(r)
	var req struct {
		Scope string `json:"scope"` // all, group, customer, site, machine, host
		ID    string `json:"id"`
		Mode  string `json:"mode"`  // mute (nur Meldungen aus) oder pause (Prüfung anhalten)
		Until int64  `json:"until"` // ms; -1 = dauerhaft
	}
	if !readJSON(w, r, &req) {
		return
	}
	if p.Scope != nil && !p.canMute(req.Scope, req.ID) {
		http.Error(w, "Keine Berechtigung", 403)
		return
	}
	now := time.Now()
	if req.Until != -1 && req.Until <= now.UnixMilli() {
		http.Error(w, "Zeitpunkt liegt in der Vergangenheit", 400)
		return
	}
	m := s.m
	var hosts []string
	label := ""
	m.mu.RLock()
	switch req.Scope {
	case "all":
		label = "Alle Meldungen"
		if req.Mode == "pause" {
			for _, h := range m.cfg.Hosts {
				if h.Org == p.Org {
					hosts = append(hosts, h.ID)
				}
			}
		}
	case "group":
		for _, g := range m.cfg.Groups {
			if g.Org == p.Org && g.ID == req.ID {
				label = "Kunde " + g.Name
			}
		}
		for _, h := range m.cfg.Hosts {
			if h.Org == p.Org && h.Group == req.ID {
				hosts = append(hosts, h.ID)
			}
		}
		if req.Mode == "mute" {
			for _, mc := range m.cfg.Machines {
				if mc.Org == p.Org && mc.Group == req.ID {
					hosts = append(hosts, "m:"+mc.ID)
				}
			}
		}
	case "customer", "site":
		// Kunde bzw. Standort: alle Hosts und Rechner dort, dazu der Standort selbst (UniFi, Agent offline)
		sites := map[string]bool{}
		for _, sc := range m.cfg.Sites {
			if sc.Org != p.Org {
				continue
			}
			if (req.Scope == "site" && sc.ID == req.ID) || (req.Scope == "customer" && sc.Customer == req.ID && req.ID != "") {
				sites[sc.ID] = true
				if req.Scope == "site" {
					label = sc.Name
				}
			}
		}
		if req.Scope == "customer" {
			for _, c := range m.cfg.Customers {
				if c.Org == p.Org && c.ID == req.ID {
					label = c.Name
				}
			}
		}
		for _, h := range m.cfg.Hosts {
			if h.Org == p.Org && sites[h.Site] {
				hosts = append(hosts, h.ID)
			}
		}
		if req.Mode == "mute" {
			for _, mc := range m.cfg.Machines {
				if mc.Org == p.Org && sites[mc.Site] {
					hosts = append(hosts, "m:"+mc.ID)
				}
			}
			for id := range sites {
				hosts = append(hosts, "s:"+id)
			}
		}
	case "machine":
		for _, mc := range m.cfg.Machines {
			if mc.Org == p.Org && mc.ID == req.ID && req.Mode == "mute" {
				hosts = append(hosts, "m:"+mc.ID)
				label = mc.Name
			}
		}
	case "host":
		for _, h := range m.cfg.Hosts {
			if h.Org == p.Org && h.ID == req.ID {
				hosts = append(hosts, h.ID)
				label = h.Name
			}
		}
	}
	m.mu.RUnlock()
	if label == "" || (req.Scope != "all" && len(hosts) == 0) {
		http.Error(w, "Nichts gefunden – keine Hosts in dieser Auswahl", 400)
		return
	}
	switch req.Mode {
	case "mute":
		end := req.Until
		if end == -1 {
			end = now.UnixMilli() + foreverMs
		}
		x := Maint{ID: randHex(6), Org: p.Org, Name: "🔕 " + label, Hosts: hosts, Start: now.UnixMilli() - 1000, End: end, Created: now.UnixMilli()}
		if req.Scope == "all" {
			x.Hosts = nil
		}
		m.mu.Lock()
		m.cfg.Maints = append(m.cfg.Maints, x)
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "id": x.ID})
	case "pause":
		until := req.Until
		if until == -1 {
			until = 0
		}
		set := map[string]bool{}
		for _, id := range hosts {
			set[id] = true
		}
		var upd []HostCfg
		m.mu.RLock()
		for _, h := range m.cfg.Hosts {
			if set[h.ID] {
				h.Paused, h.PauseUntil = true, until
				upd = append(upd, h)
			}
		}
		m.mu.RUnlock()
		for _, h := range upd {
			m.UpsertHost(h)
		}
		writeJSON(w, map[string]any{"ok": true, "paused": len(upd)})
	default:
		http.Error(w, "mode: mute oder pause", 400)
	}
}

// resumePaused: Hosts mit abgelaufener Pause wieder starten (aus housekeeping).
func (m *Monitor) resumePaused(nowMs int64) {
	var upd []HostCfg
	m.mu.RLock()
	for _, h := range m.cfg.Hosts {
		if h.Paused && h.PauseUntil > 0 && h.PauseUntil <= nowMs {
			h.Paused, h.PauseUntil = false, 0
			upd = append(upd, h)
		}
	}
	m.mu.RUnlock()
	for _, h := range upd {
		m.UpsertHost(h)
	}
}

var _ = strings.TrimSpace

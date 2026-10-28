package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Group ist ein Ordner für Hosts, z. B. ein Endkunde („Oli Pfeiffer“: Antenne + Firewall),
// unabhängig davon, über welchen Standort die Hosts gepingt werden.
type Group struct {
	ID      string `json:"id"`
	Org     string `json:"org"`
	Name    string `json:"name"`
	Note    string `json:"note,omitempty"`
	Created int64  `json:"created"`
	// Monatsbericht nur für diese Gruppe an eigene Empfänger (z. B. den Endkunden)
	ReportTo   string `json:"reportTo,omitempty"`
	ReportSent string `json:"reportSent,omitempty"`
}

func (m *Monitor) groupByID(org, id string) *Group {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.cfg.Groups {
		if g.ID == id && g.Org == org {
			gg := g
			return &gg
		}
	}
	return nil
}

// groupHostSet: IDs der Hosts einer Gruppe ("-" = Hosts ohne Gruppe).
func (m *Monitor) groupHostSet(org, gid string) map[string]bool {
	set := map[string]bool{}
	valid := map[string]bool{}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.cfg.Groups {
		if g.Org == org {
			valid[g.ID] = true
		}
	}
	for _, h := range m.cfg.Hosts {
		if h.Org != org {
			continue
		}
		if h.Group == gid || (gid == "-" && !valid[h.Group]) {
			set[h.ID] = true
		}
	}
	for _, mc := range m.cfg.Machines {
		if mc.Org == org && (mc.Group == gid || (gid == "-" && !valid[mc.Group])) {
			set["m:"+mc.ID] = true
		}
	}
	return set
}

func filterEventsByHosts(evs []Event, set map[string]bool, limit int) []Event {
	out := evs[:0]
	for _, e := range evs {
		if set[e.HostID] && len(out) < limit {
			out = append(out, e)
		}
	}
	return out
}

func (m *Monitor) groupsOf(org string) []Group {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Group{}
	for _, g := range m.cfg.Groups {
		if g.Org == org {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (m *Monitor) groupOK(org, id string) bool {
	if id == "" {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.cfg.Groups {
		if g.ID == id && g.Org == org {
			return true
		}
	}
	return false
}

// GET/POST/DELETE /api/groups
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, m.groupsOf(p.Org))
	case http.MethodPost:
		var g Group
		if !readJSON(w, r, &g) {
			return
		}
		g.Name = strings.TrimSpace(g.Name)
		g.Note = strings.TrimSpace(g.Note)
		if as := splitAddrs(g.ReportTo); len(as) > 0 {
			for _, a := range as {
				if !validEmail(strings.ToLower(a)) {
					http.Error(w, "Ungültige E-Mail-Adresse: "+a, 400)
					return
				}
			}
			g.ReportTo = strings.ToLower(strings.Join(as, ", "))
		} else {
			g.ReportTo = ""
		}
		if g.Name == "" || len(g.Name) > 80 {
			http.Error(w, "Name fehlt (höchstens 80 Zeichen)", 400)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, x := range m.cfg.Groups {
			if x.Org == p.Org && x.ID != g.ID && strings.EqualFold(x.Name, g.Name) {
				http.Error(w, "Diese Gruppe gibt es schon", 409)
				return
			}
		}
		if g.ID == "" {
			g.ID, g.Org, g.Created = randHex(5), p.Org, time.Now().UnixMilli()
			m.cfg.Groups = append(m.cfg.Groups, g)
			m.auditLocked(p.Org, whoOf(p), "angelegt", "Gruppe", g.Name, "")
		} else {
			found := false
			for i := range m.cfg.Groups {
				if m.cfg.Groups[i].ID == g.ID && m.cfg.Groups[i].Org == p.Org {
					m.cfg.Groups[i].Name, m.cfg.Groups[i].Note, m.cfg.Groups[i].ReportTo = g.Name, g.Note, g.ReportTo
					g = m.cfg.Groups[i]
					found = true
				}
			}
			if !found {
				http.Error(w, "Gruppe unbekannt", 404)
				return
			}
		}
		m.saveConfigLocked()
		writeJSON(w, g)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if r.URL.Query().Get("hosts") == "1" { // Gruppe samt Hosts löschen
			var del []string
			m.mu.RLock()
			for _, hc := range m.cfg.Hosts {
				if hc.Group == id && hc.Org == p.Org && id != "" {
					del = append(del, hc.ID)
				}
			}
			m.mu.RUnlock()
			for _, hid := range del {
				if h := m.host(hid); h != nil {
					h.mu.Lock()
					c := h.cfg
					h.mu.Unlock()
					m.audit(c.Org, whoOf(p), "gelöscht", "Host", c.Name, m.hostWhere(c)+" · mit Gruppe gelöscht")
				}
				m.DeleteHost(hid)
			}
		}
		m.mu.Lock()
		out := m.cfg.Groups[:0]
		for _, g := range m.cfg.Groups {
			if !(g.ID == id && g.Org == p.Org) {
				out = append(out, g)
			} else {
				m.auditLocked(p.Org, whoOf(p), "gelöscht", "Gruppe", g.Name, "")
			}
		}
		m.cfg.Groups = out
		var moved []HostCfg
		for i := range m.cfg.Hosts {
			if m.cfg.Hosts[i].Group == id && m.cfg.Hosts[i].Org == p.Org {
				m.cfg.Hosts[i].Group = ""
				moved = append(moved, m.cfg.Hosts[i])
			}
		}
		m.saveConfigLocked()
		m.mu.Unlock()
		for _, hc := range moved { // laufende Hosts aktualisieren
			if h := m.host(hc.ID); h != nil {
				h.mu.Lock()
				h.cfg.Group = ""
				h.mu.Unlock()
			}
		}
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

// POST /api/groups/members {group, hosts:[…]} – legt fest, welche Hosts in der Gruppe sind.
func (s *Server) handleGroupMembers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group string   `json:"group"`
		Hosts []string `json:"hosts"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p := principal(r)
	if s.m.groupByID(p.Org, req.Group) == nil {
		http.Error(w, "Gruppe unbekannt", 404)
		return
	}
	want := map[string]bool{}
	for _, id := range req.Hosts {
		want[id] = true
	}
	var changed []HostCfg
	for _, h := range s.m.hostsOf(p.Org) {
		h.mu.Lock()
		cfg := h.cfg
		h.mu.Unlock()
		switch {
		case want[cfg.ID] && cfg.Group != req.Group:
			cfg.Group = req.Group
			changed = append(changed, cfg)
		case !want[cfg.ID] && cfg.Group == req.Group:
			cfg.Group = ""
			changed = append(changed, cfg)
		}
	}
	for _, hc := range changed {
		s.m.UpsertHost(hc)
	}
	writeJSON(w, map[string]any{"ok": true, "changed": len(changed)})
}

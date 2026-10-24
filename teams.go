package main

import (
	"net/http"
	"sort"
	"strings"
)

// TeamList: benannte Gruppe von Benutzern (z. B. „Technik-Team“) – Auswahlhilfe für Alarmwege.
type TeamList struct {
	ID    string   `json:"id"`
	Org   string   `json:"org"`
	Name  string   `json:"name"`
	Users []string `json:"users"`
}

// /api/teams  GET Liste · POST {id?, name, users} · DELETE ?id=
func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		m.mu.RLock()
		out := []TeamList{}
		for _, t := range m.cfg.Teams {
			if t.Org == p.Org {
				out = append(out, t)
			}
		}
		m.mu.RUnlock()
		sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
		writeJSON(w, out)
		return
	}
	if !p.canEdit() {
		http.Error(w, "Keine Berechtigung", 403)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req TeamList
		if !readJSON(w, r, &req) {
			return
		}
		req.Name = clipStr(strings.TrimSpace(req.Name), 60)
		if req.Name == "" {
			http.Error(w, "Name fehlt", 400)
			return
		}
		m.mu.Lock()
		valid := map[string]bool{}
		for _, u := range m.cfg.Users {
			if _, _, ok := u.membership(p.Org); ok {
				valid[u.ID] = true
			}
		}
		var users []string
		seen := map[string]bool{}
		for _, id := range req.Users {
			if valid[id] && !seen[id] {
				seen[id] = true
				users = append(users, id)
			}
		}
		found := false
		for i := range m.cfg.Teams {
			if t := &m.cfg.Teams[i]; t.ID == req.ID && t.Org == p.Org && req.ID != "" {
				t.Name, t.Users, found = req.Name, users, true
				req = *t
			}
		}
		if !found {
			req = TeamList{ID: randHex(5), Org: p.Org, Name: req.Name, Users: users}
			m.cfg.Teams = append(m.cfg.Teams, req)
		}
		m.cfg.Version++
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, req)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		l := m.cfg.Teams[:0]
		for _, t := range m.cfg.Teams {
			if !(t.ID == id && t.Org == p.Org) {
				l = append(l, t)
			}
		}
		m.cfg.Teams = l
		m.cfg.Version++
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode", 405)
	}
}

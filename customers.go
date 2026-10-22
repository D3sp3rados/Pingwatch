package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Kunden innerhalb eines Kontos (z. B. beim Systemhaus): fassen Standorte zusammen
// (Kunde „Kindergärten“ → sechs Kindergärten als Standorte). Reine Ordnung für Übersicht,
// Suche und Tabelle – Prüfung, Meldungen und Abo hängen weiter an Standort bzw. Konto.

type Customer struct {
	ID      string `json:"id"`
	Org     string `json:"org"`
	Name    string `json:"name"`
	Number  string `json:"number,omitempty"` // Kundennummer
	Note    string `json:"note,omitempty"`
	Created int64  `json:"created"`
}

func (m *Monitor) customersOfLocked(org string) []Customer {
	out := []Customer{}
	for _, c := range m.cfg.Customers {
		if c.Org == org {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (m *Monitor) customersOf(org string) []Customer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.customersOfLocked(org)
}

func (m *Monitor) customerExists(org, id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.cfg.Customers {
		if c.ID == id && c.Org == org {
			return true
		}
	}
	return false
}

// GET /api/customers · POST {id?, name, number, note} · DELETE ?id= (Standorte bleiben, nur ohne Kunde)
func (s *Server) handleCustomers(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, m.customersOf(p.Org))
	case http.MethodPost:
		if !p.canEdit() {
			http.Error(w, "Keine Berechtigung", 403)
			return
		}
		var c Customer
		if !readJSON(w, r, &c) {
			return
		}
		c.Name, c.Number, c.Note = strings.TrimSpace(c.Name), strings.TrimSpace(c.Number), strings.TrimSpace(c.Note)
		if c.Name == "" || len(c.Name) > 120 || len(c.Number) > 40 || len(c.Note) > 500 {
			http.Error(w, "Name fehlt (höchstens 120 Zeichen)", 400)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, x := range m.cfg.Customers {
			if x.Org == p.Org && x.ID != c.ID && strings.EqualFold(x.Name, c.Name) {
				http.Error(w, "Diesen Kunden gibt es schon", 409)
				return
			}
		}
		if c.ID == "" {
			c.ID, c.Org, c.Created = randHex(5), p.Org, time.Now().UnixMilli()
			m.cfg.Customers = append(m.cfg.Customers, c)
		} else {
			found := false
			for i := range m.cfg.Customers {
				if x := &m.cfg.Customers[i]; x.ID == c.ID && x.Org == p.Org {
					x.Name, x.Number, x.Note = c.Name, c.Number, c.Note
					c, found = *x, true
				}
			}
			if !found {
				http.Error(w, "Kunde unbekannt", 404)
				return
			}
		}
		m.saveConfigLocked()
		writeJSON(w, c)
	case http.MethodDelete:
		if !p.canEdit() {
			http.Error(w, "Keine Berechtigung", 403)
			return
		}
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		defer m.mu.Unlock()
		out, found := m.cfg.Customers[:0], false
		for _, c := range m.cfg.Customers {
			if c.ID == id && c.Org == p.Org {
				found = true
				continue
			}
			out = append(out, c)
		}
		if !found {
			http.Error(w, "Kunde unbekannt", 404)
			return
		}
		m.cfg.Customers = out
		for i := range m.cfg.Sites {
			if m.cfg.Sites[i].Customer == id && m.cfg.Sites[i].Org == p.Org {
				m.cfg.Sites[i].Customer = ""
			}
		}
		m.saveConfigLocked()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode", 405)
	}
}

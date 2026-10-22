package main

import (
	"log"
	"net/http"
	"strings"
)

// Eigenes Konto: Name und Login-E-Mail ändern (POST /api/me/profile).
// Das Passwort wird weiterhin über /api/settings (oldPassword/newPassword) geändert.

func (s *Server) handleMyProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	p := principal(r)
	if p == nil || p.User == nil {
		http.Error(w, "Beim Betreiber-Login (admin) gibt es kein eigenes Konto. Bitte unter Team einen eigenen Benutzer anlegen.", http.StatusBadRequest)
		return
	}
	var req struct {
		Name            *string `json:"name"`
		Email           *string `json:"email"`
		CurrentPassword string  `json:"currentPassword"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	m := s.m
	oldMail := ""
	newMail := ""
	m.mu.Lock()
	var u *User
	for i := range m.cfg.Users {
		if m.cfg.Users[i].ID == p.User.ID {
			u = &m.cfg.Users[i]
		}
	}
	if u == nil {
		m.mu.Unlock()
		http.Error(w, "Benutzer unbekannt", http.StatusNotFound)
		return
	}
	if req.Email != nil {
		e := strings.TrimSpace(*req.Email)
		if !strings.EqualFold(e, u.Email) {
			if !checkPassword(u.PassHash, req.CurrentPassword) {
				m.mu.Unlock()
				http.Error(w, "Aktuelles Passwort falsch", http.StatusForbidden)
				return
			}
			if !validEmail(e) {
				m.mu.Unlock()
				http.Error(w, "E-Mail-Adresse ungültig", http.StatusBadRequest)
				return
			}
			for i := range m.cfg.Users {
				if m.cfg.Users[i].ID != u.ID && strings.EqualFold(m.cfg.Users[i].Email, e) {
					m.mu.Unlock()
					http.Error(w, "Diese E-Mail-Adresse ist bereits vergeben", http.StatusConflict)
					return
				}
			}
			oldMail, newMail = u.Email, e
			u.Email = e
		} else if e != u.Email {
			u.Email = e // nur Groß-/Kleinschreibung
		}
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if len([]rune(n)) > 60 {
			n = string([]rune(n)[:60])
		}
		u.Name = n
	}
	m.saveConfigLocked()
	cp := *u
	m.mu.Unlock()
	s.setSession(w, r, s.makeToken(cp.ID, userRef(&cp)))
	if oldMail != "" && smtpConfigured() {
		go func() {
			text := "Hallo,\n\ndie Login-E-Mail-Adresse Ihres PingWacht-Kontos wurde geändert:\n" + oldMail + " → " + newMail +
				"\n\nWaren Sie das nicht, melden Sie sich bitte umgehend beim Anbieter.\n"
			if err := sendMail([]string{oldMail}, "PingWacht: E-Mail-Adresse geändert", text, "", nil); err != nil {
				log.Printf("Info-Mail E-Mail-Änderung an %s: %v", oldMail, err)
			}
		}()
	}
	writeJSON(w, map[string]any{"ok": true, "email": cp.Email, "name": cp.Name})
}

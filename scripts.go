package main

import (
	"net/http"
	"strings"
	"time"
)

// Script-Bibliothek je Konto (Testphase): PowerShell, cmd, bash oder sh. Ausgeführt wird nur, was
// genau so in der Bibliothek steht – auf Knopfdruck (mit Zwei-Faktor-Code) oder per Plan.
// Anlegen, Ändern und Löschen nur mit Zwei-Faktor-Code; frühere Fassungen bleiben erhalten.

type Script struct {
	ID      string          `json:"id"`
	Org     string          `json:"org"`
	Name    string          `json:"name"`
	Desc    string          `json:"desc,omitempty"`
	Lang    string          `json:"lang"`    // powershell, cmd, bash, sh
	Body    string          `json:"body"`    // Inhalt
	Timeout int             `json:"timeout"` // Sekunden (Standard 300, höchstens 3600)
	Check   bool            `json:"check"`   // als Prüfung: Exit-Code ≠ 0 → Meldung
	Version int             `json:"version"`
	By      string          `json:"by,omitempty"`
	Updated int64           `json:"updated"`
	History []ScriptVersion `json:"history,omitempty"`
}

type ScriptVersion struct {
	Version int    `json:"version"`
	T       int64  `json:"t"`
	By      string `json:"by"`
	Body    string `json:"body"`
}

var scriptLangs = map[string]bool{"powershell": true, "cmd": true, "bash": true, "sh": true}

func (m *Monitor) scriptLocked(org, id string) *Script {
	for i := range m.cfg.Scripts {
		if m.cfg.Scripts[i].ID == id && m.cfg.Scripts[i].Org == org {
			sc := m.cfg.Scripts[i]
			return &sc
		}
	}
	return nil
}

// GET /api/scripts · POST {script…, code} · DELETE ?id=&code=
func (s *Server) handleScripts(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	if r.Method == http.MethodGet {
		out := []Script{}
		m.mu.RLock()
		for _, x := range m.cfg.Scripts {
			if x.Org == p.Org {
				out = append(out, x)
			}
		}
		m.mu.RUnlock()
		writeJSON(w, out)
		return
	}
	if !p.canEdit() {
		http.Error(w, "Nur Inhaber und Admins", 403)
		return
	}
	var req struct {
		Script
		Code string `json:"code"`
	}
	if r.Method == http.MethodDelete {
		req.ID, req.Code = r.URL.Query().Get("id"), r.URL.Query().Get("code")
	} else if r.Method == http.MethodPost {
		if !readJSON(w, r, &req) {
			return
		}
	} else {
		http.Error(w, "Methode", 405)
		return
	}
	x := req.Script
	if r.Method == http.MethodPost {
		x.Name, x.Desc = strings.TrimSpace(x.Name), strings.TrimSpace(x.Desc)
		x.Body = strings.ReplaceAll(x.Body, "\r\n", "\n")
		switch {
		case x.Name == "" || len(x.Name) > 80:
			http.Error(w, "Bitte einen Namen angeben", 400)
			return
		case !scriptLangs[x.Lang]:
			http.Error(w, "Sprache unbekannt", 400)
			return
		case strings.TrimSpace(x.Body) == "" || len(x.Body) > 64<<10:
			http.Error(w, "Script leer oder zu groß (höchstens 64 KB)", 400)
			return
		}
		if x.Timeout <= 0 {
			x.Timeout = 300
		}
		if x.Timeout > 3600 {
			x.Timeout = 3600
		}
	}
	sub, sec, _ := m.totpOf(p)
	if sec == "" {
		http.Error(w, "Bitte zuerst unter Einstellungen → Mein Konto die Zwei-Faktor-Anmeldung einrichten", 412)
		return
	}
	if err := totpCheck(sub, sec, req.Code, time.Now()); err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	by := "Betreiber"
	if p.User != nil {
		by = p.User.Email
	}
	now := time.Now().UnixMilli()
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method == http.MethodDelete {
		out := m.cfg.Scripts[:0]
		for _, y := range m.cfg.Scripts {
			if !(y.ID == x.ID && y.Org == p.Org) {
				out = append(out, y)
			}
		}
		m.cfg.Scripts = out
		m.saveConfigLocked()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	for _, y := range m.cfg.Scripts {
		if y.Org == p.Org && y.ID != x.ID && strings.EqualFold(y.Name, x.Name) {
			http.Error(w, "Ein Script mit diesem Namen gibt es schon", 409)
			return
		}
	}
	if x.ID == "" {
		x.ID, x.Org, x.Version, x.By, x.Updated, x.History = randHex(5), p.Org, 1, by, now, nil
		m.cfg.Scripts = append(m.cfg.Scripts, x)
	} else {
		found := false
		for i := range m.cfg.Scripts {
			c := &m.cfg.Scripts[i]
			if c.ID != x.ID || c.Org != p.Org {
				continue
			}
			found = true
			hist := c.History
			if c.Body != x.Body {
				hist = append([]ScriptVersion{{Version: c.Version, T: c.Updated, By: c.By, Body: c.Body}}, hist...)
				if len(hist) > 10 {
					hist = hist[:10]
				}
				x.Version = c.Version + 1
			} else {
				x.Version = c.Version
			}
			x.Org, x.By, x.Updated, x.History = c.Org, by, now, hist
			*c = x
		}
		if !found {
			http.Error(w, "Script unbekannt", 404)
			return
		}
	}
	m.saveConfigLocked()
	writeJSON(w, x)
}

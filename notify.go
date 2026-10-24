package main

import (
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Persönliche Benachrichtigungs-Einstellungen je Benutzer:
// Push an/aus, E-Mail an/aus, Urlaubsmodus (Pause bis) und Kunden (Gruppen) stummschalten.

type NotifyPrefs struct {
	NoPush     bool             `json:"noPush,omitempty"`     // Push auf allen eigenen Geräten aus
	Mail       bool             `json:"mail,omitempty"`       // persönliche E-Mail bei Ausfällen
	MailTo     string           `json:"mailTo,omitempty"`     // leer = Login-E-Mail
	MailAll    bool             `json:"mailAll,omitempty"`    // Betreiber: Mails für alle Kunden
	PauseUntil int64            `json:"pauseUntil,omitempty"` // ms; > jetzt = keine Meldungen
	Mute       map[string]int64 `json:"mute,omitempty"`       // Gruppen-ID → bis (ms), -1 = dauerhaft
	// Feineinstellung je Kanal: nil = wie bisher (Push: alles laut App, E-Mail: Ausfälle aller Geräte)
	PushSel *NotifySel `json:"pushSel,omitempty"`
	MailSel *NotifySel `json:"mailSel,omitempty"`
}

// NotifySel: Meldungsarten und Geräte (leer = alle) für Push bzw. E-Mail eines Benutzers.
type NotifySel struct {
	Outage    bool     `json:"outage"`
	Offline   bool     `json:"offline"`
	Loss      bool     `json:"loss"`
	Spike     bool     `json:"spike"`
	Hosts     []string `json:"hosts,omitempty"`
	Groups    []string `json:"groups,omitempty"`
	Customers []string `json:"customers,omitempty"`
	Machines  []string `json:"machines,omitempty"`
}

func (s *NotifySel) wants(n notice, group, cust string) bool {
	if s == nil {
		return true
	}
	return Channel{Outage: s.Outage, Offline: s.Offline, Loss: s.Loss, Spike: s.Spike,
		Hosts: s.Hosts, Groups: s.Groups, Customers: s.Customers, Machines: s.Machines}.wantsIn(n, group, cust)
}

var mailThrottle = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

// suppressed: Meldung für diesen Benutzer unterdrücken (Pause oder Kunde stumm)?
func (p *NotifyPrefs) suppressed(group string, now int64) bool {
	if p == nil {
		return false
	}
	if p.PauseUntil > now {
		return true
	}
	if group != "" {
		if until, ok := p.Mute[group]; ok && (until < 0 || until > now) {
			return true
		}
	}
	return false
}

// hostGroupLocked liefert die Gruppe eines Hosts (m.mu muss gehalten werden).
func (m *Monitor) hostGroupLocked(hostID string) string {
	if hostID == "" {
		return ""
	}
	if strings.HasPrefix(hostID, "m:") {
		return m.machineGroupLocked(hostID)
	}
	for _, h := range m.cfg.Hosts {
		if h.ID == hostID {
			return h.Group
		}
	}
	return ""
}

// mailKind: welche Meldungen per persönlicher E-Mail kommen (keine Verlust-/Spitzen-Flut).
func mailKind(k string) bool {
	switch k {
	case "loss", "spike", "test":
		return false
	}
	return true
}

// personalMails verschickt die persönlichen E-Mails zu einer Meldung.
func (m *Monitor) personalMails(n notice) {
	if !smtpConfigured() {
		return
	}
	if n.kind == "test" {
		return
	}
	now := time.Now()
	m.mu.RLock()
	group := m.hostGroupLocked(n.hostID)
	cust := m.noticeCustomerLocked(n)
	type rcpt struct {
		to, org string
		u       User
	}
	var to []rcpt
	for _, u := range m.cfg.Users {
		p := u.Notify
		if p == nil || !p.Mail {
			continue
		}
		if _, _, member := u.membership(n.org); !member && !(u.isSuper() && p.MailAll) {
			continue
		}
		if u.Expires > 0 && u.Expires < now.UnixMilli() {
			continue
		}
		if p.suppressed(group, now.UnixMilli()) {
			continue
		}
		if p.MailSel != nil {
			if !p.MailSel.wants(n, group, cust) {
				continue
			}
			if n.kind == "loss" || n.kind == "spike" { // höchstens alle 10 Minuten je Host und Benutzer
				k := u.ID + "|" + n.hostID + "|" + n.kind
				mailThrottle.Lock()
				last := mailThrottle.m[k]
				ok := time.Since(last) >= 10*time.Minute
				if ok {
					mailThrottle.m[k] = now
				}
				mailThrottle.Unlock()
				if !ok {
					continue
				}
			}
		} else if !mailKind(n.kind) {
			continue
		}
		addr := strings.TrimSpace(p.MailTo)
		if addr == "" {
			addr = u.Email
		}
		if validEmail(addr) {
			to = append(to, rcpt{addr, u.Org, u})
		}
	}
	m.mu.RUnlock()
	// eingeschränkte Benutzer nur für ihre Kunden/Hosts
	keep := to[:0]
	for _, r := range to {
		if m.noticeVisible(&r.u, n) {
			keep = append(keep, r)
		}
	}
	to = keep
	if len(to) == 0 {
		return
	}
	orgName := ""
	if o := m.orgByID(n.org); o != nil {
		orgName = o.Name
	}
	for _, r := range to {
		go func(addr string) {
			c := Channel{Type: "email", Target: addr, Name: "persönlich"}
			if err := sendAlert(c, n, now, orgName); err != nil {
				log.Printf("Persönliche Mail an %s: %v", addr, err)
			}
		}(r.to)
	}
}

// ---------- API: /api/me/notify ----------

type notifyView struct {
	NotifyPrefs
	Email   string `json:"email"`
	Name    string `json:"name"`
	Super   bool   `json:"super"`
	SMTP    bool   `json:"smtp"`
	Paused  bool   `json:"paused"`
	Devices int    `json:"devices"`
}

func (s *Server) handleMyNotify(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	uid := ""
	if p != nil && p.User != nil {
		uid = p.User.ID
	}
	// Admin: Einstellungen eines Teammitglieds (?user=)
	if t := r.URL.Query().Get("user"); t != "" && t != uid {
		if !p.canEdit() {
			http.Error(w, "Keine Berechtigung", 403)
			return
		}
		tu := m.userByID(t)
		if tu == nil {
			http.Error(w, "Benutzer unbekannt", 404)
			return
		}
		if _, _, ok := tu.membership(p.Org); !ok {
			http.Error(w, "Benutzer gehört nicht zu diesem Konto", 403)
			return
		}
		uid = t
	}
	if uid == "" {
		http.Error(w, "Nur mit persönlichem Benutzer möglich (nicht mit dem Betreiber-Passwort)", 400)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req struct {
			NoPush     *bool      `json:"noPush"`
			Mail       *bool      `json:"mail"`
			MailTo     *string    `json:"mailTo"`
			MailAll    *bool      `json:"mailAll"`
			PauseUntil *int64     `json:"pauseUntil"`
			MuteGroup  string     `json:"muteGroup"`
			MuteUntil  *int64     `json:"muteUntil"` // mit muteGroup: 0 = Stummschaltung aufheben, -1 = dauerhaft
			PushSel    *NotifySel `json:"pushSel"`
			MailSel    *NotifySel `json:"mailSel"`
			PushAll    bool       `json:"pushAll"` // Feineinstellung Push entfernen (= alles)
			MailAllSel bool       `json:"mailAllSel"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.MailTo != nil {
			if t := strings.TrimSpace(*req.MailTo); t != "" && !validEmail(t) {
				http.Error(w, "E-Mail-Adresse ungültig", 400)
				return
			}
		}
		m.mu.Lock()
		for i := range m.cfg.Users {
			u := &m.cfg.Users[i]
			if u.ID != uid {
				continue
			}
			if u.Notify == nil {
				u.Notify = &NotifyPrefs{}
			}
			n := u.Notify
			if req.NoPush != nil {
				n.NoPush = *req.NoPush
			}
			if req.Mail != nil {
				n.Mail = *req.Mail
			}
			if req.MailTo != nil {
				n.MailTo = strings.TrimSpace(*req.MailTo)
			}
			if req.MailAll != nil {
				n.MailAll = *req.MailAll && u.Super
			}
			if req.PauseUntil != nil {
				n.PauseUntil = *req.PauseUntil
			}
			if req.PushSel != nil {
				n.PushSel = req.PushSel
			}
			if req.PushAll {
				n.PushSel = nil
			}
			if req.MailSel != nil {
				n.MailSel = req.MailSel
			}
			if req.MailAllSel {
				n.MailSel = nil
			}
			if req.MuteGroup != "" && req.MuteUntil != nil {
				if n.Mute == nil {
					n.Mute = map[string]int64{}
				}
				if *req.MuteUntil == 0 {
					delete(n.Mute, req.MuteGroup)
				} else {
					n.Mute[req.MuteGroup] = *req.MuteUntil
				}
			}
			// abgelaufene Stummschaltungen aufräumen
			now := time.Now().UnixMilli()
			for g, until := range n.Mute {
				if until > 0 && until < now {
					delete(n.Mute, g)
				}
			}
		}
		m.saveConfigLocked()
		m.mu.Unlock()
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
		return
	}
	u := m.userByID(uid)
	if u == nil {
		http.Error(w, "Benutzer unbekannt", 404)
		return
	}
	v := notifyView{Email: u.Email, Name: u.Name, Super: u.Super, SMTP: smtpConfigured()}
	if u.Notify != nil {
		v.NotifyPrefs = *u.Notify
	}
	v.Paused = v.PauseUntil > time.Now().UnixMilli()
	m.mu.RLock()
	for _, d := range m.cfg.Devices {
		if d.User == u.ID {
			v.Devices++
		}
	}
	m.mu.RUnlock()
	writeJSON(w, v)
}

// notifySummary: kurze Beschreibung für Logs/Tests.
func (p *NotifyPrefs) String() string {
	if p == nil {
		return "Standard"
	}
	return fmt.Sprintf("push=%v mail=%v pause=%d mute=%d", !p.NoPush, p.Mail, p.PauseUntil, len(p.Mute))
}

var _ = html.EscapeString

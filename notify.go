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
	items := batchList(n)
	m.mu.RLock()
	meta := map[string][2]string{}
	for _, x := range items {
		meta[noticeKey(x)] = [2]string{m.hostGroupLocked(x.hostID), m.noticeCustomerLocked(x)}
	}
	type rcpt struct {
		to string
		u  User
		ok map[string]bool
		n  notice
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
		ok := map[string]bool{}
		for _, x := range items {
			g := meta[noticeKey(x)]
			if p.suppressed(g[0], now.UnixMilli()) {
				continue
			}
			if p.MailSel != nil {
				if !p.MailSel.wants(x, g[0], g[1]) {
					continue
				}
			} else if !mailKind(x.kind) {
				continue
			}
			ok[noticeKey(x)] = true
		}
		if len(ok) == 0 {
			continue
		}
		if p.MailSel != nil && (n.kind == "loss" || n.kind == "spike") { // höchstens alle 10 Minuten je Host und Benutzer
			k := u.ID + "|" + n.hostID + "|" + n.kind
			mailThrottle.Lock()
			last := mailThrottle.m[k]
			allow := time.Since(last) >= 10*time.Minute
			if allow {
				mailThrottle.m[k] = now
			}
			mailThrottle.Unlock()
			if !allow {
				continue
			}
		}
		addr := strings.TrimSpace(p.MailTo)
		if addr == "" {
			addr = u.Email
		}
		if validEmail(addr) {
			to = append(to, rcpt{to: addr, u: u, ok: ok})
		}
	}
	m.mu.RUnlock()
	// eingeschränkte Benutzer nur für ihre Kunden/Hosts; bei Sammelmeldungen je Empfänger zusammenstellen
	keep := to[:0]
	for _, r := range to {
		u := r.u
		nn, ok := m.pickFor(n, func(x notice) bool { return r.ok[noticeKey(x)] && m.noticeVisible(&u, x) })
		if ok {
			r.n = nn
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
		go func(addr string, n notice) {
			c := Channel{Type: "email", Target: addr, Name: "persönlich"}
			if err := sendAlert(c, n, now, orgName); err != nil {
				log.Printf("Persönliche Mail an %s: %v", addr, err)
			}
		}(r.to, r.n)
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

// GET /api/team/notify – persönliche Benachrichtigungen aller Teammitglieder (für die Übersicht bei Alarme & Wartung).
func (s *Server) handleTeamNotify(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	type row struct {
		ID      string     `json:"id"`
		Name    string     `json:"name"`
		Email   string     `json:"email"`
		NoPush  bool       `json:"noPush"`
		Mail    bool       `json:"mail"`
		MailTo  string     `json:"mailTo,omitempty"`
		PushSel *NotifySel `json:"pushSel,omitempty"`
		MailSel *NotifySel `json:"mailSel,omitempty"`
		Paused  bool       `json:"paused,omitempty"`
		Devices int        `json:"devices"`
		Me      bool       `json:"me,omitempty"`
	}
	m := s.m
	now := time.Now().UnixMilli()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []row{}
	for _, u := range m.cfg.Users {
		if _, _, ok := u.membership(p.Org); !ok {
			continue
		}
		me := p.User != nil && p.User.ID == u.ID
		if !p.canEdit() && !me {
			continue
		}
		x := row{ID: u.ID, Name: u.Name, Email: u.Email, Me: me}
		if n := u.Notify; n != nil {
			x.NoPush, x.Mail, x.MailTo, x.PushSel, x.MailSel, x.Paused = n.NoPush, n.Mail, n.MailTo, n.PushSel, n.MailSel, n.PauseUntil > now
		}
		for _, d := range m.cfg.Devices {
			if d.User == u.ID {
				x.Devices++
			}
		}
		out = append(out, x)
	}
	writeJSON(w, out)
}

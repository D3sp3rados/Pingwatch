package main

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Maint ist ein Wartungsfenster: keine Alarme, Ausfälle zählen nicht in die Verfügbarkeit.
type Maint struct {
	ID       string   `json:"id"`
	Org      string   `json:"org"`
	Name     string   `json:"name"`
	Hosts    []string `json:"hosts"` // leer = alle Hosts und Standorte der Org
	Start    int64    `json:"start"` // einmalig: Beginn/Ende (ms)
	End      int64    `json:"end"`
	Weekdays []int    `json:"weekdays"` // wöchentlich: 0 = Sonntag … 6 = Samstag
	From     string   `json:"from"`     // "HH:MM"
	To       string   `json:"to"`       // "HH:MM" (kleiner als From = über Mitternacht)
	Created  int64    `json:"created"`
}

func hhmm(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func (w Maint) recurring() bool { return len(w.Weekdays) > 0 }

func (w Maint) activeAt(t time.Time) bool {
	if !w.recurring() {
		ms := t.UnixMilli()
		return ms >= w.Start && ms < w.End
	}
	from, ok1 := hhmm(w.From)
	to, ok2 := hhmm(w.To)
	if !ok1 || !ok2 {
		return false
	}
	day := func(d time.Weekday) bool {
		for _, x := range w.Weekdays {
			if x == int(d) {
				return true
			}
		}
		return false
	}
	min := t.Hour()*60 + t.Minute()
	if from < to {
		return day(t.Weekday()) && min >= from && min < to
	}
	// über Mitternacht (oder ganzer Tag bei from == to)
	if from == to {
		return day(t.Weekday())
	}
	return (day(t.Weekday()) && min >= from) || (day(t.AddDate(0, 0, -1).Weekday()) && min < to)
}

func (w Maint) covers(hostID string) bool {
	if len(w.Hosts) == 0 {
		return true
	}
	for _, h := range w.Hosts {
		if h == hostID {
			return true
		}
	}
	return false
}

// inMaint: Host (oder bei hostID == "" der ganze Kunde) gerade in Wartung?
func (m *Monitor) inMaint(org, hostID string, t time.Time) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, w := range m.cfg.Maints {
		if w.Org != org || !w.activeAt(t) {
			continue
		}
		if hostID == "" {
			if len(w.Hosts) == 0 {
				return true
			}
			continue
		}
		if w.covers(hostID) {
			return true
		}
	}
	return false
}

// inMaintSite: ganzer Standort in Wartung (Konto-weit oder Stummschaltung für Standort/Kunde)?
func (m *Monitor) inMaintSite(org, siteID string, t time.Time) bool {
	return m.inMaint(org, "", t) || (siteID != "" && m.inMaint(org, "s:"+siteID, t))
}

// parentDown liefert den Namen des ausgefallenen übergeordneten Hosts (Abhängigkeit), sonst "".
func (m *Monitor) parentDown(h *Host) string {
	h.mu.Lock()
	pid := h.cfg.Parent
	h.mu.Unlock()
	for depth := 0; pid != "" && depth < 10; depth++ {
		p := m.host(pid)
		if p == nil {
			return ""
		}
		p.mu.Lock()
		down := p.oOut != nil || (p.cfg.OutageCount > 0 && p.consec >= p.cfg.OutageCount)
		name, next := p.cfg.Name, p.cfg.Parent
		p.mu.Unlock()
		if down {
			return name
		}
		pid = next
	}
	return ""
}

// validParent prüft, dass der übergeordnete Host zum selben Kunden gehört und keine Schleife entsteht.
func (m *Monitor) validParent(org, id, parent string) error {
	if parent == "" {
		return nil
	}
	if parent == id {
		return errors.New("Ein Host kann nicht von sich selbst abhängen")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	byID := map[string]HostCfg{}
	for _, hc := range m.cfg.Hosts {
		byID[hc.ID] = hc
	}
	p, ok := byID[parent]
	if !ok || p.Org != org {
		return errors.New("Übergeordneter Host unbekannt")
	}
	for cur, n := p.Parent, 0; cur != "" && n < 20; n++ {
		if cur == id {
			return errors.New("Abhängigkeit würde eine Schleife bilden")
		}
		cur = byID[cur].Parent
	}
	return nil
}

// eventStarted ergänzt ein neues Ereignis um Wartung/Ursache, meldet es und startet ggf. die Routenverfolgung.
func (m *Monitor) eventStarted(e *Event, h *Host) {
	start := time.UnixMilli(e.Start)
	maint := m.inMaint(e.Org, e.HostID, start) || m.inMaint(e.Org, e.HostID, time.Now())
	if e.HostID == "" && e.Site != "" {
		maint = maint || m.inMaintSite(e.Org, e.Site, start) || m.inMaintSite(e.Org, e.Site, time.Now())
	}
	m.evMu.Lock()
	e.Maint = maint
	if e.Type != "outage" {
		e.Decided = true
	}
	m.evMu.Unlock()
	if e.Type != "outage" {
		return
	}
	h.mu.Lock()
	hasParent := h.cfg.Parent != ""
	h.mu.Unlock()
	if hasParent && !maint {
		// Übergeordneter Host fällt oft ein paar Sekunden später auf – kurz abwarten.
		time.Sleep(parentGrace)
	}
	cause := ""
	if hasParent {
		cause = m.parentDown(h)
	}
	h.mu.Lock()
	if isUniFiWanHost(h.cfg) {
		cause = "Leitungsausfall wird über UniFi gemeldet"
	}
	h.mu.Unlock()
	m.evMu.Lock()
	e.Cause = cause
	e.Notified = !maint && cause == "" && !e.Late
	e.Decided = true
	cp := *e
	m.evMu.Unlock()
	if cp.Notified {
		m.notifyEventStart(cp)
		m.requestTrace(cp)
	}
}

var parentGrace = 20 * time.Second

// quiet: keine Meldung für Ereignisse in der Wartung oder mit übergeordneter Ursache.
func (e Event) quiet() bool { return e.Maint || e.Cause != "" || e.Late }

// ---------- API ----------

func (s *Server) handleMaint(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		out := []map[string]any{}
		now := time.Now()
		m.mu.RLock()
		for _, x := range m.cfg.Maints {
			if x.Org == p.Org {
				out = append(out, map[string]any{"id": x.ID, "name": x.Name, "hosts": x.Hosts, "start": x.Start, "end": x.End,
					"weekdays": x.Weekdays, "from": x.From, "to": x.To, "active": x.activeAt(now),
					"past": !x.recurring() && x.End < now.UnixMilli()})
			}
		}
		m.mu.RUnlock()
		sort.SliceStable(out, func(i, j int) bool { return out[i]["start"].(int64) > out[j]["start"].(int64) })
		writeJSON(w, out)
	case http.MethodPost:
		var x Maint
		if !readJSON(w, r, &x) {
			return
		}
		x.Name = strings.TrimSpace(x.Name)
		if x.Name == "" {
			x.Name = "Wartung"
		}
		if x.recurring() {
			for _, d := range x.Weekdays {
				if d < 0 || d > 6 {
					http.Error(w, "Wochentag ungültig", 400)
					return
				}
			}
			if _, ok := hhmm(x.From); !ok {
				http.Error(w, "Uhrzeit „von“ ungültig", 400)
				return
			}
			if _, ok := hhmm(x.To); !ok {
				http.Error(w, "Uhrzeit „bis“ ungültig", 400)
				return
			}
			x.Start, x.End = 0, 0
		} else {
			if x.Start <= 0 || x.End <= x.Start {
				http.Error(w, "Bitte Beginn und Ende angeben (Ende nach Beginn)", 400)
				return
			}
			if x.End-x.Start > 31*86400000 {
				http.Error(w, "Ein Wartungsfenster darf höchstens 31 Tage dauern", 400)
				return
			}
		}
		own := map[string]bool{}
		for _, h := range m.hostsOf(p.Org) {
			own[h.cfg.ID] = true
		}
		hs := x.Hosts[:0]
		for _, id := range x.Hosts {
			if own[id] {
				hs = append(hs, id)
			}
		}
		x.Hosts = hs
		m.mu.Lock()
		defer m.mu.Unlock()
		if x.ID == "" {
			n := 0
			for _, y := range m.cfg.Maints {
				if y.Org == p.Org {
					n++
				}
			}
			if n >= 50 {
				http.Error(w, "Höchstens 50 Wartungsfenster", 400)
				return
			}
			x.ID, x.Org, x.Created = randHex(6), p.Org, time.Now().UnixMilli()
			m.cfg.Maints = append(m.cfg.Maints, x)
		} else {
			found := false
			for i := range m.cfg.Maints {
				if m.cfg.Maints[i].ID == x.ID && m.cfg.Maints[i].Org == p.Org {
					x.Org, x.Created = p.Org, m.cfg.Maints[i].Created
					m.cfg.Maints[i] = x
					found = true
				}
			}
			if !found {
				http.Error(w, "Wartungsfenster unbekannt", 404)
				return
			}
		}
		m.saveConfigLocked()
		writeJSON(w, map[string]any{"ok": true, "id": x.ID})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		out := m.cfg.Maints[:0]
		for _, x := range m.cfg.Maints {
			if !(x.ID == id && x.Org == p.Org) {
				out = append(out, x)
			}
		}
		m.cfg.Maints = out
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

// cleanupMaints entfernt einmalige Fenster, die seit mehr als 90 Tagen vorbei sind.
func (m *Monitor) cleanupMaints() {
	cut := time.Now().AddDate(0, 0, -90).UnixMilli()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.cfg.Maints[:0]
	changed := false
	for _, x := range m.cfg.Maints {
		if !x.recurring() && x.End < cut {
			changed = true
			continue
		}
		out = append(out, x)
	}
	m.cfg.Maints = out
	if changed {
		m.saveConfigLocked()
	}
}

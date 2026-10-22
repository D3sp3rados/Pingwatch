package main

import (
	"net/http"
	"strings"
	"time"
)

// Update-Pläne: z. B. „jeden Mittwoch 22:00 bei Kunde X Sicherheitsupdates installieren und bei
// Bedarf neu starten“. Anlegen und Ändern nur mit Zwei-Faktor-Code; ausgeführt wird dann ohne Code.
// Während eines Laufs sind Meldungen der betroffenen Rechner und Standorte 2 Stunden stumm.

type UpdatePlan struct {
	ID       string `json:"id"`
	Org      string `json:"org"`
	Name     string `json:"name"`
	Scope    string `json:"scope"`  // site, customer, machine
	Target   string `json:"target"` // ID von Standort / Kunde / Rechner
	Weekdays []int  `json:"weekdays"`
	At       string `json:"at"`     // "HH:MM"
	Cat      string `json:"cat"`    // security, all
	Reboot   string `json:"reboot"` // never, ifneeded
	On       bool   `json:"on"`
	By       string `json:"by,omitempty"`
	LastRun  int64  `json:"lastRun,omitempty"`
	Created  int64  `json:"created"`
}

// GET /api/updateplans · POST {plan…, code} · DELETE ?id=
func (s *Server) handleUpdatePlans(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		out := []UpdatePlan{}
		m.mu.RLock()
		for _, x := range m.cfg.UpdatePlans {
			if x.Org == p.Org {
				out = append(out, x)
			}
		}
		m.mu.RUnlock()
		writeJSON(w, out)
	case http.MethodPost:
		if !p.canEdit() {
			http.Error(w, "Nur Inhaber und Admins", 403)
			return
		}
		var req struct {
			UpdatePlan
			Code string `json:"code"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		x := req.UpdatePlan
		x.Name = strings.TrimSpace(x.Name)
		if x.Name == "" || len(x.Name) > 80 {
			http.Error(w, "Bitte einen Namen angeben", 400)
			return
		}
		if _, ok := hhmm(x.At); !ok {
			http.Error(w, "Uhrzeit ungültig", 400)
			return
		}
		days := []int{}
		for _, d := range x.Weekdays {
			if d >= 0 && d <= 6 {
				days = append(days, d)
			}
		}
		if len(days) == 0 {
			http.Error(w, "Bitte mindestens einen Wochentag wählen", 400)
			return
		}
		x.Weekdays = days
		if x.Cat != "all" {
			x.Cat = "security"
		}
		if x.Reboot != "ifneeded" {
			x.Reboot = "never"
		}
		if !m.planTargetOK(p.Org, x.Scope, x.Target) {
			http.Error(w, "Ziel unbekannt", 400)
			return
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
		x.By = "Betreiber"
		if p.User != nil {
			x.By = p.User.Email
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if x.ID == "" {
			x.ID, x.Org, x.Created = randHex(5), p.Org, time.Now().UnixMilli()
			m.cfg.UpdatePlans = append(m.cfg.UpdatePlans, x)
		} else {
			found := false
			for i := range m.cfg.UpdatePlans {
				if c := &m.cfg.UpdatePlans[i]; c.ID == x.ID && c.Org == p.Org {
					x.Org, x.Created, x.LastRun = c.Org, c.Created, c.LastRun
					*c, found = x, true
				}
			}
			if !found {
				http.Error(w, "Plan unbekannt", 404)
				return
			}
		}
		m.saveConfigLocked()
		writeJSON(w, x)
	case http.MethodDelete:
		if !p.canEdit() {
			http.Error(w, "Nur Inhaber und Admins", 403)
			return
		}
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		out := m.cfg.UpdatePlans[:0]
		for _, x := range m.cfg.UpdatePlans {
			if !(x.ID == id && x.Org == p.Org) {
				out = append(out, x)
			}
		}
		m.cfg.UpdatePlans = out
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode", 405)
	}
}

func (m *Monitor) planTargetOK(org, scope, id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	switch scope {
	case "site":
		for _, s := range m.cfg.Sites {
			if s.ID == id && s.Org == org {
				return true
			}
		}
	case "customer":
		for _, c := range m.cfg.Customers {
			if c.ID == id && c.Org == org {
				return true
			}
		}
	case "machine":
		for _, mc := range m.cfg.Machines {
			if mc.ID == id && mc.Org == org {
				return true
			}
		}
	}
	return false
}

// planTargets: Rechner eines Plans an Standorten mit erlaubten Aktionen.
func (m *Monitor) planTargetsLocked(org, scope, id string) (machines []MachineCfg, sites map[string]bool) {
	sites = map[string]bool{}
	for _, sc := range m.cfg.Sites {
		if sc.Org != org || sc.Sys == nil || !sc.Sys.On || !sc.Sys.Updates {
			continue
		}
		if scope == "site" && sc.ID != id || scope == "customer" && (id == "" || sc.Customer != id) {
			continue
		}
		sites[sc.ID] = true
	}
	for _, mc := range m.cfg.Machines {
		if mc.Org == org && sites[mc.Site] && (scope != "machine" || mc.ID == id) {
			machines = append(machines, mc)
		}
	}
	return
}

// queueActionsLocked: Aufträge anlegen und protokollieren (m.mu gehalten).
func (m *Monitor) queueActionsLocked(org, by, kind, cat, reboot, opts string, targets []MachineCfg, now int64) {
	jobs.Lock()
	for _, mc := range targets {
		j := agentJob{ID: randHex(8), Kind: kind, Params: map[string]string{"machine": mc.ID, "cat": cat, "reboot": reboot}}
		q := mc.Site
		if mc.SysOnly {
			q = "m:" + mc.ID
		}
		jobs.byID[j.ID] = &jobState{agentJob: j, Org: org, Site: mc.Site, Status: "queued", Created: now}
		jobs.queue[q] = append(jobs.queue[q], j)
		m.cfg.Actions = append(m.cfg.Actions, ActionLog{ID: randHex(6), Org: org, T: now, By: by, Kind: kind, Opts: opts, Machine: mc.ID, Name: mc.Name, Site: mc.Site, Job: j.ID, Status: "queued"})
	}
	jobs.Unlock()
	if n := len(m.cfg.Actions); n > 3000 {
		m.cfg.Actions = append([]ActionLog(nil), m.cfg.Actions[n-3000:]...)
	}
	// Meldungen der Rechner und Standorte während Updates/Neustart stumm (2 Stunden)
	if kind == "reboot" || reboot != "never" {
		var hs []string
		seen := map[string]bool{}
		for _, mc := range targets {
			hs = append(hs, "m:"+mc.ID)
			if !seen[mc.Site] {
				seen[mc.Site] = true
				hs = append(hs, "s:"+mc.Site)
			}
		}
		m.cfg.Maints = append(m.cfg.Maints, Maint{ID: randHex(6), Org: org, Name: "🔄 " + actionKinds[kind] + " (" + by + ")", Hosts: hs, Start: now - 1000, End: now + 2*3600*1000, Created: now})
	}
}

func actionOpts(cat, reboot string) string {
	return map[string]string{"security": "Sicherheitsupdates", "all": "alle Updates"}[cat] + " · " +
		map[string]string{"never": "kein Neustart", "ifneeded": "Neustart wenn nötig", "now": "danach neu starten"}[reboot]
}

// updatePlanLoop prüft jede halbe Minute, ob ein Plan fällig ist.
func (m *Monitor) updatePlanLoop() {
	for {
		m.runDuePlans(time.Now())
		time.Sleep(30 * time.Second)
	}
}

func (m *Monitor) runDuePlans(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := now.Hour()*60 + now.Minute()
	changed := false
	for i := range m.cfg.UpdatePlans {
		x := &m.cfg.UpdatePlans[i]
		if !x.On {
			continue
		}
		at, ok := hhmm(x.At)
		day := false
		for _, d := range x.Weekdays {
			day = day || d == int(now.Weekday())
		}
		// fällig: richtiger Tag, Uhrzeit erreicht (bis 10 Minuten danach) und heute noch nicht gelaufen
		if !ok || !day || cur < at || cur > at+10 || (x.LastRun > 0 && time.UnixMilli(x.LastRun).Format("2006-01-02") == now.Format("2006-01-02")) {
			continue
		}
		x.LastRun = now.UnixMilli()
		changed = true
		targets, _ := m.planTargetsLocked(x.Org, x.Scope, x.Target)
		if len(targets) > 0 {
			m.queueActionsLocked(x.Org, "Plan „"+x.Name+"“", "winupdate", x.Cat, x.Reboot, actionOpts(x.Cat, x.Reboot), targets, now.UnixMilli())
		}
	}
	if changed {
		m.saveConfigLocked()
	}
}

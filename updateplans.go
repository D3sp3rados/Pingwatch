package main

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	At       string `json:"at"`                 // "HH:MM"
	Cat      string `json:"cat"`                // security, all
	Reboot   string `json:"reboot"`             // never, ifneeded
	Kind     string `json:"kind,omitempty"`     // winupdate (Standard) oder script
	ScriptID string `json:"scriptId,omitempty"` // bei Kind script
	Every    int    `json:"every,omitempty"`    // Minuten: regelmäßig statt Wochentag/Uhrzeit (mind. 5)
	On       bool   `json:"on"`
	By       string `json:"by,omitempty"`
	LastRun  int64  `json:"lastRun,omitempty"`
	Created  int64  `json:"created"`
	SP       string `json:"sp,omitempty"`  // unterschriebener Text
	Sig      string `json:"sig,omitempty"` // Signatur
	Kid      string `json:"kid,omitempty"` // Schlüssel
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
			Code    string `json:"code"`
			Preview bool   `json:"preview"`
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
		if x.Kind != "script" {
			x.Kind, x.ScriptID = "winupdate", ""
		} else {
			m.mu.RLock()
			ok := m.scriptLocked(p.Org, x.ScriptID) != nil
			m.mu.RUnlock()
			if !ok {
				http.Error(w, "Bitte ein Script wählen", 400)
				return
			}
		}
		if x.Every < 0 || x.Kind != "script" {
			x.Every = 0
		}
		if x.Every > 0 && x.Every < 5 {
			x.Every = 5
		}
		days := []int{}
		for _, d := range x.Weekdays {
			if d >= 0 && d <= 6 {
				days = append(days, d)
			}
		}
		if x.Every == 0 {
			if _, ok := hhmm(x.At); !ok {
				http.Error(w, "Uhrzeit ungültig", 400)
				return
			}
			if len(days) == 0 {
				http.Error(w, "Bitte mindestens einen Wochentag wählen", 400)
				return
			}
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
		shash := ""
		if x.Kind == "script" {
			m.mu.RLock()
			if sc := m.scriptLocked(p.Org, x.ScriptID); sc != nil {
				shash = scriptHash(sc.Body)
			}
			m.mu.RUnlock()
		}
		keys := m.orgSignKeys(p.Org)
		payload := planPayload(p.Org, x, shash)
		if req.Preview {
			writeJSON(w, map[string]any{"payload": payload, "signed": len(keys) > 0})
			return
		}
		x.SP = ""
		if len(keys) > 0 {
			if !verifyWithKeys(keys, x.Kid, payload, x.Sig) {
				http.Error(w, "Signatur fehlt oder ist ungültig – bitte in einem Browser mit Signaturschlüssel dieses Kontos speichern", 400)
				return
			}
			x.SP = payload
		} else {
			x.Sig, x.Kid = "", ""
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

// actReq: was an welche Rechner geht.
type actReq struct {
	Org, By, Kind, Cat, Reboot, Opts string
	Script                           *Script // bei Kind "script"
	Quiet                            bool    // regelmäßige Script-Prüfung: nur Fehler protokollieren
	Plan                             string  // Name des Plans (für Meldungen)
	SP, Sig, Kid                     string  // unterschriebener Text, Signatur, Schlüssel (signierte Aufträge)
}

// jobMeta: Angaben zu Aufträgen, die (noch) keinen Protokolleintrag haben.
var jobMeta = struct {
	sync.Mutex
	m map[string]ActionLog
}{m: map[string]ActionLog{}}

// queueActionsLocked: Aufträge anlegen und protokollieren (m.mu gehalten).
func (m *Monitor) queueActionsLocked(q actReq, targets []MachineCfg, now int64) {
	jobs.Lock()
	for _, mc := range targets {
		j := agentJob{ID: randHex(8), Kind: q.Kind, Params: map[string]string{"machine": mc.ID, "cat": q.Cat, "reboot": q.Reboot}}
		if q.SP != "" {
			j.Params["sp"], j.Params["sig"], j.Params["kid"] = q.SP, q.Sig, q.Kid
		}
		if q.Script != nil {
			j.Params["script"], j.Params["lang"], j.Params["timeout"] = q.Script.Body, q.Script.Lang, strconv.Itoa(q.Script.Timeout)
			j.Params["sid"], j.Params["sname"] = q.Script.ID, q.Script.Name
		}
		qk := mc.Site
		if mc.SysOnly {
			qk = "m:" + mc.ID
		}
		jobs.byID[j.ID] = &jobState{agentJob: j, Org: q.Org, Site: mc.Site, Status: "queued", Created: now}
		jobs.queue[qk] = append(jobs.queue[qk], j)
		a := ActionLog{ID: randHex(6), Org: q.Org, T: now, By: q.By, Kind: q.Kind, Opts: q.Opts, Machine: mc.ID, Name: mc.Name, Site: mc.Site, Job: j.ID, Status: "queued"}
		if q.Script != nil {
			a.Script = q.Script.ID
		}
		if q.Quiet {
			jobMeta.Lock()
			jobMeta.m[j.ID] = a
			if len(jobMeta.m) > 20000 {
				jobMeta.m = map[string]ActionLog{}
			}
			jobMeta.Unlock()
		} else {
			m.cfg.Actions = append(m.cfg.Actions, a)
		}
	}
	jobs.Unlock()
	if n := len(m.cfg.Actions); n > 3000 {
		m.cfg.Actions = append([]ActionLog(nil), m.cfg.Actions[n-3000:]...)
	}
	// Meldungen der Rechner und Standorte während Updates/Neustart stumm (2 Stunden)
	if q.Kind == "reboot" || q.Kind == "winupdate" && q.Reboot != "never" {
		var hs []string
		seen := map[string]bool{}
		for _, mc := range targets {
			hs = append(hs, "m:"+mc.ID)
			if !seen[mc.Site] {
				seen[mc.Site] = true
				hs = append(hs, "s:"+mc.Site)
			}
		}
		m.cfg.Maints = append(m.cfg.Maints, Maint{ID: randHex(6), Org: q.Org, Name: "🔄 " + actionKinds[q.Kind] + " (" + q.By + ")", Hosts: hs, Start: now - 1000, End: now + 2*3600*1000, Created: now})
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

// planLast: letzter Lauf von Intervall-Plänen (nur im Speicher, damit nicht ständig gespeichert wird).
var planLast = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

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
		if x.Every > 0 {
			planLast.Lock()
			last := planLast.m[x.ID]
			due := now.Sub(last) >= time.Duration(x.Every)*time.Minute-15*time.Second
			if due {
				planLast.m[x.ID] = now
			}
			planLast.Unlock()
			if !due {
				continue
			}
		} else {
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
		}
		targets, _ := m.planTargetsLocked(x.Org, x.Scope, x.Target)
		if len(targets) == 0 {
			continue
		}
		// Konto mit Signaturschlüsseln: nur gültig unterschriebene Pläne (z. B. nach Script-Änderung neu speichern)
		shash := ""
		if x.Kind == "script" {
			if sc := m.scriptLocked(x.Org, x.ScriptID); sc != nil {
				shash = scriptHash(sc.Body)
			}
		}
		if hasKeys := m.orgHasKeysLocked(x.Org); hasKeys && (x.SP == "" || x.SP != planPayload(x.Org, *x, shash)) {
			if !planWarned[x.ID] {
				planWarned[x.ID] = true
				log.Printf("Plan %s (%s) nicht gültig unterschrieben – übersprungen", x.Name, x.Org)
			}
			continue
		}
		q := actReq{Org: x.Org, By: "Plan „" + x.Name + "“", Kind: "winupdate", Cat: x.Cat, Reboot: x.Reboot, Opts: actionOpts(x.Cat, x.Reboot), Plan: x.Name, SP: x.SP, Sig: x.Sig, Kid: x.Kid}
		if x.Kind == "script" {
			sc := m.scriptLocked(x.Org, x.ScriptID)
			if sc == nil {
				continue
			}
			q = actReq{Org: x.Org, By: q.By, Kind: "script", Cat: x.Cat, Reboot: x.Reboot, Opts: "Script „" + sc.Name + "“", Script: sc, Quiet: x.Every > 0, Plan: x.Name, SP: x.SP, Sig: x.Sig, Kid: x.Kid}
		}
		m.queueActionsLocked(q, targets, now.UnixMilli())
	}
	if changed {
		m.saveConfigLocked()
	}
}

var planWarned = map[string]bool{}

func (m *Monitor) orgHasKeysLocked(org string) bool {
	for _, o := range m.cfg.Orgs {
		if o.ID == org {
			return len(o.SignKeys) > 0
		}
	}
	return false
}

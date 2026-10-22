package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Aktionen auf Rechnern (Testphase): Windows-/Linux-Updates installieren und Neustart.
// Absicherung:
//   - nur feste Aktionen, keine freien Befehle
//   - nur Inhaber/Admins, jede Auslösung mit frischem Zwei-Faktor-Code
//   - pro Standort ausdrücklich erlauben (Rechner-Überwachung → „Updates und Neustarts erlauben“)
//   - auf dem Rechner abschaltbar: "noActions": true in der agent.json
//   - jede Aktion steht im Protokoll (wer, wann, was, Ergebnis)

const actionJobTTL = 12 * time.Hour // so lange wartet ein Auftrag auf einen ausgeschalteten Rechner

type ActionLog struct {
	ID      string   `json:"id"`
	Org     string   `json:"org"`
	T       int64    `json:"t"`
	By      string   `json:"by"`
	Kind    string   `json:"kind"`             // winupdate, reboot, script
	Script  string   `json:"script,omitempty"` // Script-ID
	Opts    string   `json:"opts,omitempty"`   // z. B. „Sicherheit · Neustart wenn nötig“
	Machine string   `json:"machine"`          // Rechner-ID
	Name    string   `json:"name"`             // Rechnername zum Zeitpunkt
	Site    string   `json:"site"`             // Standort
	Job     string   `json:"job,omitempty"`    // Auftrag
	Status  string   `json:"status"`           // queued, running, done, error
	Result  string   `json:"result,omitempty"` // Zusammenfassung
	Detail  []string `json:"detail,omitempty"` // installierte / fehlgeschlagene Updates
	Done    int64    `json:"done,omitempty"`   // Ende
}

// actionResult: Antwort des Agenten auf winupdate/reboot.
type actionResult struct {
	Message string   `json:"message"`
	Exit    *int     `json:"exit,omitempty"`   // Script: Exit-Code
	Output  string   `json:"output,omitempty"` // Script: Ausgabe (gekürzt)
	OK      []string `json:"ok,omitempty"`
	Fail    []string `json:"fail,omitempty"`
	Reboot  bool     `json:"reboot,omitempty"`  // Neustart erforderlich
	Planned string   `json:"planned,omitempty"` // geplanter Neustart, z. B. „in 5 Minuten“
}

var actionKinds = map[string]string{"winupdate": "Updates installieren", "reboot": "Neustart", "script": "Script"}

// POST /api/actions {kind, scope: machine|site|customer, id, cat: security|all, reboot: never|ifneeded|now, code}
// GET /api/actions?machine= → Protokoll
func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	if r.Method == http.MethodGet {
		mid := r.URL.Query().Get("machine")
		out := []ActionLog{}
		m.mu.RLock()
		for i := len(m.cfg.Actions) - 1; i >= 0 && len(out) < 300; i-- {
			a := m.cfg.Actions[i]
			if a.Org == p.Org && (mid == "" || a.Machine == mid) {
				out = append(out, a)
			}
		}
		m.mu.RUnlock()
		writeJSON(w, out)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Methode", 405)
		return
	}
	if !p.canEdit() {
		http.Error(w, "Nur Inhaber und Admins dürfen Updates und Neustarts auslösen", 403)
		return
	}
	var req struct {
		Kind, Scope, ID, Cat, Reboot, Code, Script string
		Preview                                    bool   // nur den zu unterschreibenden Text liefern
		TS                                         int64  `json:"ts"`
		Nonce, Sig, Kid                            string // Signatur (wenn das Konto Schlüssel hat)
	}
	if !readJSON(w, r, &req) {
		return
	}
	if actionKinds[req.Kind] == "" {
		http.Error(w, "Aktion unbekannt", 400)
		return
	}
	if req.Cat != "all" {
		req.Cat = "security"
	}
	if req.Reboot != "now" && req.Reboot != "ifneeded" {
		req.Reboot = "never"
	}
	m.mu.RLock()
	targets, _ := m.planTargetsLocked(p.Org, req.Scope, req.ID)
	m.mu.RUnlock()
	if req.Scope != "machine" && req.Scope != "site" && req.Scope != "customer" {
		targets = nil
	}
	if len(targets) == 0 {
		http.Error(w, "Keine Rechner – Updates und Neustarts sind für diesen Standort nicht erlaubt (Rechner-Überwachung → „Updates und Neustarts erlauben“)", 400)
		return
	}
	shash := ""
	if req.Kind == "script" {
		m.mu.RLock()
		sc := m.scriptLocked(p.Org, req.Script)
		m.mu.RUnlock()
		if sc == nil {
			http.Error(w, "Script unbekannt", 400)
			return
		}
		shash = scriptHash(sc.Body)
	}
	keys := m.orgSignKeys(p.Org)
	if req.Preview {
		ts, nonce := time.Now().UnixMilli(), randHex(8)
		writeJSON(w, map[string]any{"payload": actPayload(p.Org, req.Kind, req.Scope, req.ID, req.Cat, req.Reboot, shash, ts, nonce), "ts": ts, "nonce": nonce, "signed": len(keys) > 0})
		return
	}
	sp := ""
	if len(keys) > 0 {
		sp = actPayload(p.Org, req.Kind, req.Scope, req.ID, req.Cat, req.Reboot, shash, req.TS, req.Nonce)
		if time.Since(time.UnixMilli(req.TS)) > 15*time.Minute || !verifyWithKeys(keys, req.Kid, sp, req.Sig) {
			http.Error(w, "Signatur fehlt oder ist ungültig – bitte in einem Browser mit Signaturschlüssel dieses Kontos auslösen", 400)
			return
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
	opts := ""
	if req.Kind == "winupdate" {
		opts = actionOpts(req.Cat, req.Reboot)
	}
	m.mu.Lock()
	q := actReq{Org: p.Org, By: by, Kind: req.Kind, Cat: req.Cat, Reboot: req.Reboot, Opts: opts, SP: sp, Sig: req.Sig, Kid: req.Kid}
	if req.Kind == "script" {
		q.Script = m.scriptLocked(p.Org, req.Script)
		if q.Script == nil {
			m.mu.Unlock()
			http.Error(w, "Script unbekannt", 400)
			return
		}
		q.Opts = "Script „" + q.Script.Name + "“"
	}
	m.queueActionsLocked(q, targets, time.Now().UnixMilli())
	m.saveConfigLocked()
	m.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "machines": len(targets)})
}

// actionJobDone: Ergebnis eines Aktions-Auftrags ins Protokoll und als Meldung.
func (m *Monitor) actionJobDone(jobID, kind, errText string, raw json.RawMessage) {
	var res actionResult
	if errText == "" {
		json.Unmarshal(raw, &res)
	}
	failed := errText != "" || len(res.Fail) > 0 || (kind == "script" && res.Exit != nil && *res.Exit != 0)
	jobMeta.Lock()
	meta, quiet := jobMeta.m[jobID]
	delete(jobMeta.m, jobID)
	jobMeta.Unlock()
	m.mu.Lock()
	var a *ActionLog
	for i := range m.cfg.Actions {
		if m.cfg.Actions[i].Job == jobID {
			a = &m.cfg.Actions[i]
		}
	}
	org, sid, mid := meta.Org, meta.Script, meta.Machine
	if a != nil {
		org, sid, mid = a.Org, a.Script, a.Machine
	}
	isCheck := false
	if kind == "script" {
		if sc := m.scriptLocked(org, sid); sc != nil {
			isCheck = sc.Check
		}
	}
	transition, wasBad := false, false
	if isCheck {
		key := sid + "|" + mid
		scriptChecks.Lock()
		prev, known := scriptChecks.m[key]
		scriptChecks.m[key] = failed
		scriptChecks.Unlock()
		transition, wasBad = (!known && failed) || (known && prev != failed), known && prev
	}
	if a == nil {
		if !quiet || !failed {
			m.mu.Unlock()
			if quiet && transition && wasBad {
				m.scriptCheckNote(meta, false, "")
			}
			return
		}
		// regelmäßige Prüfung fehlgeschlagen → jetzt protokollieren
		m.cfg.Actions = append(m.cfg.Actions, meta)
		a = &m.cfg.Actions[len(m.cfg.Actions)-1]
	}
	a.Done = time.Now().UnixMilli()
	if errText != "" {
		a.Status, a.Result = "error", errText
	} else {
		a.Status, a.Result = "done", res.Message
		if failed && kind == "script" {
			a.Status = "error"
		}
		for _, t := range res.OK {
			a.Detail = append(a.Detail, "✓ "+t)
		}
		for _, t := range res.Fail {
			a.Detail = append(a.Detail, "✗ "+t)
		}
		if res.Output != "" {
			lines := strings.Split(strings.TrimRight(res.Output, "\r\n"), "\n")
			if len(lines) > 200 {
				lines = append(lines[:200], "…")
			}
			a.Detail = append(a.Detail, lines...)
		}
	}
	cp := *a
	m.saveConfigLocked()
	m.mu.Unlock()
	if quiet {
		if transition {
			m.scriptCheckNote(cp, failed, cp.Result)
		}
		return
	}
	title := "✅ " + actionKinds[kind] + ": " + cp.Name
	if failed {
		title = "⚠️ " + actionKinds[kind] + ": " + cp.Name
	}
	if kind == "script" {
		title += " · " + strings.TrimPrefix(cp.Opts, "Script ")
	}
	body := cp.Result
	if mc, ok := m.machineByID(cp.Machine); ok {
		body = m.sysInfo(mc, cp.Result)
	}
	m.notify(notice{"action", "m:" + cp.Machine, title, body, cp.Org, ""})
}

// scriptChecks: letzter Zustand je Script-Prüfung und Rechner (true = fehlgeschlagen).
var scriptChecks = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func (m *Monitor) scriptCheckNote(a ActionLog, bad bool, why string) {
	name := strings.TrimSuffix(strings.TrimPrefix(a.Opts, "Script „"), "“")
	title, body := "🟢 Prüfung wieder OK: "+name+" ("+a.Name+")", "Das Script meldet wieder Erfolg."
	if bad {
		title, body = "🔴 Prüfung fehlgeschlagen: "+name+" ("+a.Name+")", why
	}
	if mc, ok := m.machineByID(a.Machine); ok {
		body = m.sysInfo(mc, body)
		if m.inMaint(mc.Org, "m:"+mc.ID, time.Now()) || m.inMaintSite(mc.Org, mc.Site, time.Now()) {
			return
		}
	}
	m.notify(notice{"syswarn", "m:" + a.Machine, title, body, a.Org, ""})
}

// actionJobStarted: Agent hat den Auftrag abgeholt.
func (m *Monitor) actionJobsTaken(js []agentJob) {
	var ids []string
	for _, j := range js {
		if actionKinds[j.Kind] != "" {
			ids = append(ids, j.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	m.mu.Lock()
	for i := range m.cfg.Actions {
		for _, id := range ids {
			if m.cfg.Actions[i].Job == id && m.cfg.Actions[i].Status == "queued" {
				m.cfg.Actions[i].Status = "running"
			}
		}
	}
	m.mu.Unlock()
}

// actionSummary: kurze Zusammenfassung für das Agent-Ergebnis.
func actionSummary(ok, fail int, reboot bool, planned string) string {
	var parts []string
	switch {
	case ok == 0 && fail == 0:
		parts = append(parts, "keine passenden Updates – Rechner ist aktuell")
	default:
		parts = append(parts, fmt.Sprintf("%d installiert", ok))
		if fail > 0 {
			parts = append(parts, fmt.Sprintf("%d fehlgeschlagen", fail))
		}
	}
	if planned != "" {
		parts = append(parts, "Neustart "+planned)
	} else if reboot {
		parts = append(parts, "Neustart erforderlich")
	}
	return strings.Join(parts, " · ")
}

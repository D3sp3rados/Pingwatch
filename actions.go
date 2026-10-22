package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	Kind    string   `json:"kind"`             // winupdate, reboot
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
	OK      []string `json:"ok,omitempty"`
	Fail    []string `json:"fail,omitempty"`
	Reboot  bool     `json:"reboot,omitempty"`  // Neustart erforderlich
	Planned string   `json:"planned,omitempty"` // geplanter Neustart, z. B. „in 5 Minuten“
}

var actionKinds = map[string]string{"winupdate": "Updates installieren", "reboot": "Neustart"}

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
	var req struct{ Kind, Scope, ID, Cat, Reboot, Code string }
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
	m.queueActionsLocked(p.Org, by, req.Kind, req.Cat, req.Reboot, opts, targets, time.Now().UnixMilli())
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
	m.mu.Lock()
	var a *ActionLog
	for i := range m.cfg.Actions {
		if m.cfg.Actions[i].Job == jobID {
			a = &m.cfg.Actions[i]
		}
	}
	if a == nil {
		m.mu.Unlock()
		return
	}
	a.Done = time.Now().UnixMilli()
	if errText != "" {
		a.Status, a.Result = "error", errText
	} else {
		a.Status, a.Result = "done", res.Message
		for _, t := range res.OK {
			a.Detail = append(a.Detail, "✓ "+t)
		}
		for _, t := range res.Fail {
			a.Detail = append(a.Detail, "✗ "+t)
		}
	}
	cp := *a
	m.saveConfigLocked()
	m.mu.Unlock()
	title := "✅ " + actionKinds[kind] + ": " + cp.Name
	if cp.Status == "error" || len(res.Fail) > 0 {
		title = "⚠️ " + actionKinds[kind] + ": " + cp.Name
	}
	body := cp.Result
	if mc, ok := m.machineByID(cp.Machine); ok {
		body = m.sysInfo(mc, cp.Result)
	}
	m.notify(notice{"action", "m:" + cp.Machine, title, body, cp.Org})
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

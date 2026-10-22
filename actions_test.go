package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTOTPVector(t *testing.T) {
	sec := b32.EncodeToString([]byte("12345678901234567890"))
	if c := totpAt(sec, uint64(59/30)); c != "287082" {
		t.Fatal(c)
	}
	if c := totpAt(sec, uint64(1111111109/30)); c != "081804" {
		t.Fatal(c)
	}
}

func TestActionsAndPlans(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	_, body := c.do("POST", "/api/sites", map[string]any{"name": "Büro"})
	var sc SiteCfg
	json.Unmarshal([]byte(body), &sc)
	mc, err := s.m.registerMachine(sc, "pc1", "PC1", true)
	if err != nil {
		t.Fatal(err)
	}
	// Standort erlaubt noch keine Aktionen
	if code, _ := c.do("POST", "/api/actions", map[string]any{"kind": "winupdate", "scope": "machine", "id": mc.ID, "code": "000000"}); code != 400 {
		t.Fatal("ohne Erlaubnis", code)
	}
	c.do("POST", "/api/sites", map[string]any{"id": sc.ID, "sys": map[string]any{"on": true, "updates": true}})
	// ohne Zwei-Faktor
	if code, _ := c.do("POST", "/api/actions", map[string]any{"kind": "winupdate", "scope": "machine", "id": mc.ID, "code": "000000"}); code != 412 {
		t.Fatal("ohne 2FA", code)
	}
	_, body = c.do("POST", "/api/totp", map[string]string{"action": "setup"})
	var su struct{ Secret, URI string }
	json.Unmarshal([]byte(body), &su)
	if su.Secret == "" || !strings.HasPrefix(su.URI, "otpauth://totp/") {
		t.Fatal(body)
	}
	now := time.Now()
	if code, b := c.do("POST", "/api/totp", map[string]string{"action": "enable", "code": totpAt(su.Secret, uint64(now.Unix()/30)-1)}); code != 200 {
		t.Fatal("enable", code, b)
	}
	if code, _ := c.do("POST", "/api/actions", map[string]any{"kind": "winupdate", "scope": "machine", "id": mc.ID, "code": "123456"}); code != 403 {
		t.Fatal("falscher Code", code)
	}
	if code, b := c.do("POST", "/api/actions", map[string]any{"kind": "winupdate", "scope": "site", "id": sc.ID, "cat": "security", "reboot": "ifneeded", "code": totpAt(su.Secret, uint64(now.Unix()/30))}); code != 200 {
		t.Fatal("Aktion", code, b)
	}
	// derselbe Code nicht zweimal
	if code, _ := c.do("POST", "/api/actions", map[string]any{"kind": "reboot", "scope": "machine", "id": mc.ID, "code": totpAt(su.Secret, uint64(now.Unix()/30))}); code != 403 {
		t.Fatal("Code doppelt verwendet", code)
	}
	jb := takeJobs("m:" + mc.ID)
	if len(jb) != 1 || jb[0].Kind != "winupdate" || jb[0].Params["reboot"] != "ifneeded" {
		t.Fatalf("Auftrag: %+v", jb)
	}
	s.m.actionJobsTaken(jb)
	if !s.m.inMaint("default", "m:"+mc.ID, time.Now()) {
		t.Fatal("während Updates nicht stumm")
	}
	res, _ := json.Marshal(actionResult{Message: "2 installiert", OK: []string{"KB1", "KB2"}, Reboot: true})
	s.m.actionJobDone(jb[0].ID, "winupdate", "", res)
	_, body = c.do("GET", "/api/actions?machine="+mc.ID, nil)
	var logs []ActionLog
	json.Unmarshal([]byte(body), &logs)
	if len(logs) != 1 || logs[0].Status != "done" || len(logs[0].Detail) != 2 || logs[0].By != "Betreiber" {
		t.Fatalf("Protokoll: %s", body)
	}
	// Plan: anlegen (mit Code) und fällig ausführen
	code2 := totpAt(su.Secret, uint64(now.Unix()/30)+1)
	wd := int(now.Weekday())
	at := now.Add(-2 * time.Minute)
	if code, b := c.do("POST", "/api/updateplans", map[string]any{"name": "Mittwochs", "scope": "site", "target": sc.ID, "weekdays": []int{wd}, "at": at.Format("15:04"), "cat": "all", "reboot": "ifneeded", "on": true, "code": code2}); code != 200 {
		t.Fatal("Plan", code, b)
	}
	s.m.runDuePlans(now)
	s.m.runDuePlans(now) // nur einmal am Tag
	if jb := takeJobs("m:" + mc.ID); len(jb) != 1 || jb[0].Params["cat"] != "all" {
		t.Fatalf("Plan-Auftrag: %+v", jb)
	}
}

func TestAgentActionsGuard(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	mk := func(no bool) *Agent {
		dir := t.TempDir()
		return &Agent{file: AgentFile{Server: ts.URL, Token: site.Token, NoActions: no}, dataDir: dir, hosts: map[string]*agentHost{},
			refresh: make(chan struct{}, 1), stopAll: make(chan struct{}), notify: func(string, string, bool) {}, changed: func() {},
			client: &http.Client{Timeout: 5 * time.Second}, machine: sysMachineID(dir)}
	}
	a, b := mk(false), mk(true)
	a.fetchConfig()
	if a.sysActions {
		t.Fatal("ohne Erlaubnis am Standort an")
	}
	c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "updates": true}})
	a.fetchConfig()
	b.fetchConfig()
	if !a.sysActions || b.sysActions {
		t.Fatalf("Erlaubnis: %v / noActions: %v", a.sysActions, b.sysActions)
	}
	// Auftrag für fremden Rechner bzw. mit noActions wird abgelehnt (Ergebnis landet im Protokoll)
	mc, _ := s.m.registerMachine(site, b.machine, "PC-B", false)
	now := time.Now().UnixMilli()
	s.m.mu.Lock()
	s.m.queueActionsLocked("default", "Test", "reboot", "", "never", "", []MachineCfg{mc}, now)
	s.m.mu.Unlock()
	jb := takeJobs(site.ID)
	if len(jb) != 1 {
		t.Fatal(jb)
	}
	a.runJob(jb[0]) // falscher Rechner
	b.runJob(jb[0]) // noActions
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.m.mu.RLock()
		st, res := s.m.cfg.Actions[0].Status, s.m.cfg.Actions[0].Result
		s.m.mu.RUnlock()
		if st == "error" {
			if !strings.Contains(res, "noActions") && !strings.Contains(res, "anderen Rechner") {
				t.Fatal(res)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("kein Ergebnis im Protokoll")
}

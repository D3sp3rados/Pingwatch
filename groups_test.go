package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestGroups(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("admin", "admin-pass-123")
	code, body := c.do("POST", "/api/groups", map[string]string{"name": "Oli Pfeiffer"})
	if code != 200 {
		t.Fatal(code, body)
	}
	var g Group
	mustJSON(t, body, &g)
	if code, _ := c.do("POST", "/api/groups", map[string]string{"name": "oli pfeiffer"}); code != 409 {
		t.Fatal("doppelter Name", code)
	}
	code, body = c.do("POST", "/api/hosts", map[string]any{"name": "Antenne", "addr": "10.0.0.1", "site": "cloud", "group": g.ID})
	var hc HostCfg
	mustJSON(t, body, &hc)
	if hc.Group != g.ID {
		t.Fatal("Gruppe nicht gesetzt:", body)
	}
	_, body = c.do("POST", "/api/hosts", map[string]any{"name": "X", "addr": "10.0.0.2", "site": "cloud", "group": "fremd"})
	if strings.Contains(body, `"group":"fremd"`) {
		t.Fatal("fremde Gruppe angenommen")
	}
	_, body = c.do("GET", "/api/state", nil)
	if !strings.Contains(body, `"groups":[{"id":"`+g.ID) {
		t.Fatal("state ohne Gruppen")
	}
	c.do("DELETE", "/api/groups?id="+g.ID, nil)
	if s.m.host(hc.ID).cfg.Group != "" {
		t.Fatal("Host noch in gelöschter Gruppe")
	}
}

func TestGroupReportAndEvents(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("admin", "admin-pass-123")
	_, body := c.do("POST", "/api/groups", map[string]string{"name": "Oli Pfeiffer", "reportTo": "Oli@Pfeiffer.de"})
	var g Group
	mustJSON(t, body, &g)
	if g.ReportTo != "oli@pfeiffer.de" {
		t.Fatal("reportTo:", body)
	}
	if code, _ := c.do("POST", "/api/groups", map[string]any{"id": g.ID, "name": "Oli Pfeiffer", "reportTo": "kaputt"}); code != 400 {
		t.Fatal("ungültige Adresse angenommen")
	}
	a := s.m.UpsertHost(HostCfg{Name: "Antenne Pfeiffer", Addr: "10.0.0.1", Site: "cloud", Org: "default", Group: g.ID})
	b := s.m.UpsertHost(HostCfg{Name: "Fremder Router", Addr: "10.0.0.2", Site: "cloud", Org: "default"})
	now := time.Now().UnixMilli()
	s.m.evMu.Lock()
	s.m.events = append(s.m.events, &Event{ID: 901, HostID: a.ID, HostName: a.Name, Org: "default", Type: "outage", Start: now - 600000, End: now - 300000},
		&Event{ID: 902, HostID: b.ID, HostName: b.Name, Org: "default", Type: "outage", Start: now - 600000, End: now - 300000})
	s.m.evMu.Unlock()
	_, body = c.do("GET", "/api/events?group="+g.ID, nil)
	if !strings.Contains(body, "Antenne Pfeiffer") || strings.Contains(body, "Fremder") {
		t.Fatal("Gruppenfilter:", body)
	}
	_, body = c.do("GET", "/api/events?group=-", nil)
	if strings.Contains(body, "Antenne Pfeiffer") || !strings.Contains(body, "Fremder") {
		t.Fatal("ohne Gruppe:", body)
	}
	pdf, err := s.m.buildReport("default", time.Now(), g.ID)
	if err != nil || !bytes.Contains(pdf, []byte("Oli Pfeiffer")) {
		t.Fatal("PDF:", err)
	}
	var to []string
	old := sendMailFn
	sendMailFn = func(rcpt []string, msg []byte) error { to = rcpt; return nil }
	defer func() { sendMailFn = old }()
	code, body := c.do("POST", "/api/report/send", map[string]string{"month": time.Now().Format("2006-01"), "group": g.ID})
	if code != 200 || len(to) != 1 || to[0] != "oli@pfeiffer.de" {
		t.Fatal("Versand:", code, body, to)
	}
}

func TestAdminEventsSuperOnly(t *testing.T) {
	_, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("admin", "admin-pass-123")
	_, body := admin.do("POST", "/api/admin/orgs", map[string]any{"name": "K", "plan": "basis", "ownerEmail": "k@k.de"})
	var r struct{ TempPassword string }
	mustJSON(t, body, &r)
	if code, body := admin.do("GET", "/api/admin/events", nil); code != 200 || !strings.Contains(body, `"orgs"`) {
		t.Fatal(code, body)
	}
	k := newClient(t, ts)
	k.login("k@k.de", r.TempPassword)
	if code, _ := k.do("GET", "/api/admin/events", nil); code == 200 {
		t.Fatal("Kunde darf nicht alle Ereignisse sehen")
	}
}

package main

import (
	"strings"
	"testing"
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

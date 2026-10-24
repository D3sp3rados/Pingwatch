package main

import (
	"strings"
	"testing"
)

func TestNotifySel(t *testing.T) {
	s := &NotifySel{Outage: true, Loss: true, Groups: []string{"g1"}}
	if !s.wants(notice{kind: "outage", hostID: "h1"}, "g1", "") {
		t.Fatal("Ausfall in Gruppe g1")
	}
	if s.wants(notice{kind: "outage", hostID: "h2"}, "g2", "") {
		t.Fatal("andere Gruppe")
	}
	if !s.wants(notice{kind: "loss", hostID: "h1"}, "g1", "") || s.wants(notice{kind: "spike", hostID: "h1"}, "g1", "") {
		t.Fatal("Arten")
	}
	var nilSel *NotifySel
	if !nilSel.wants(notice{kind: "spike"}, "", "") {
		t.Fatal("nil = alles")
	}
}

func TestNotifyAPIForUser(t *testing.T) {
	_, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	code, body := c.do("POST", "/api/users", map[string]any{"email": "r@x.de", "name": "Reimund", "role": "admin"})
	if code != 200 {
		t.Fatal(code, body)
	}
	_, body = c.do("GET", "/api/users", nil)
	var us []struct{ ID, Email string }
	mustJSON(t, body, &us)
	var u struct{ ID string }
	for _, x := range us {
		if x.Email == "r@x.de" {
			u.ID = x.ID
		}
	}
	code, body = c.do("POST", "/api/me/notify?user="+u.ID, map[string]any{"mail": true, "mailSel": map[string]any{"outage": true, "loss": true, "groups": []string{"g1"}}})
	if code != 200 || !strings.Contains(body, `"mailSel"`) || !strings.Contains(body, `"g1"`) {
		t.Fatal(code, body)
	}
	code, body = c.do("POST", "/api/me/notify?user="+u.ID, map[string]any{"mailAllSel": true})
	if code != 200 || strings.Contains(body, `"mailSel"`) {
		t.Fatal(code, body)
	}
}

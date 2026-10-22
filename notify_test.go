package main

import (
	"strings"
	"testing"
	"time"
)

func TestNotifyPrefs(t *testing.T) {
	now := time.Now().UnixMilli()
	p := &NotifyPrefs{Mute: map[string]int64{"g1": -1, "g2": now - 1000, "g3": now + 60000}}
	if !p.suppressed("g1", now) || p.suppressed("g2", now) || !p.suppressed("g3", now) || p.suppressed("", now) {
		t.Fatal("Stummschaltung falsch")
	}
	p.PauseUntil = now + 1000
	if !p.suppressed("", now) {
		t.Fatal("Pause greift nicht")
	}
	var nilp *NotifyPrefs
	if nilp.suppressed("g1", now) {
		t.Fatal("nil")
	}

	_, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("admin", "admin-pass-123")
	code, body := admin.do("POST", "/api/users", map[string]any{"email": "tech@rst.de", "role": "viewer"})
	if code != 200 {
		t.Fatal(code, body)
	}
	var r struct{ TempPassword string }
	mustJSON(t, body, &r)
	u := newClient(t, ts)
	u.login("tech@rst.de", r.TempPassword)
	code, body = u.do("POST", "/api/me/notify", map[string]any{"noPush": true, "mail": true, "pauseUntil": now + 3600000, "muteGroup": "abc", "muteUntil": -1})
	if code != 200 || !strings.Contains(body, `"noPush":true`) || !strings.Contains(body, `"paused":true`) || !strings.Contains(body, `"abc":-1`) {
		t.Fatal(code, body)
	}
	code, body = u.do("POST", "/api/me/notify", map[string]any{"muteGroup": "abc", "muteUntil": 0, "mailTo": "kaputt"})
	if code != 400 {
		t.Fatal("ungültige Mail muss abgelehnt werden", code, body)
	}
	_, body = u.do("POST", "/api/me/notify", map[string]any{"muteGroup": "abc", "muteUntil": 0})
	if strings.Contains(body, `"abc"`) {
		t.Fatal("Stummschaltung nicht aufgehoben", body)
	}
}

func TestChannelGroups(t *testing.T) {
	c := Channel{Outage: true, Groups: []string{"oli"}}
	n := notice{kind: "outage", hostID: "h1"}
	if !c.wantsIn(n, "oli") || c.wantsIn(n, "andere") || c.wantsIn(n, "") {
		t.Fatal("Kundenfilter falsch")
	}
	c.Hosts = []string{"h1"}
	if !c.wantsIn(n, "andere") {
		t.Fatal("einzeln gewählter Host muss trotzdem melden")
	}
	if !(Channel{Outage: true}).wantsIn(n, "x") {
		t.Fatal("ohne Filter = alle")
	}
}

func TestWithInfo(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	m.cfg.Sites = []SiteCfg{{ID: "s1", Name: "Albbruck Büro"}}
	m.cfg.Groups = []Group{{ID: "g1", Name: "Oli Pfeiffer"}}
	m.cfg.Hosts = []HostCfg{{ID: "h1", Addr: "185.51.186.213", Site: "s1", Group: "g1"}}
	got := m.withInfo("h1", "s1", "Seit 09:31:02 keine Antwort")
	want := "185.51.186.213 · Kunde Oli Pfeiffer\nSeit 09:31:02 keine Antwort\nGeprüft von: Agent Albbruck Büro"
	if got != want {
		t.Fatalf("%q", got)
	}
}

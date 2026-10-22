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

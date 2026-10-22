package main

import (
	"strings"
	"testing"
	"time"
)

func TestMute(t *testing.T) {
	srv, ts := newTestServer(t)
	a := newClient(t, ts)
	a.login("admin", "admin-pass-123")
	_, body := a.do("POST", "/api/groups", map[string]any{"name": "Oli"})
	var g struct{ ID string }
	mustJSON(t, body, &g)
	_, body = a.do("POST", "/api/hosts", map[string]any{"name": "Antenne", "addr": "10.0.0.1", "site": "cloud", "group": g.ID})
	var h struct{ ID string }
	mustJSON(t, body, &h)
	until := time.Now().Add(time.Hour).UnixMilli()
	if code, body := a.do("POST", "/api/mute", map[string]any{"scope": "group", "id": g.ID, "mode": "mute", "until": until}); code != 200 {
		t.Fatal(code, body)
	}
	_, st := a.do("GET", "/api/state", nil)
	if !strings.Contains(st, "🔕 Kunde Oli") {
		t.Fatal("Stummschaltung fehlt im Zustand", st[:200])
	}
	if !srv.m.inMaint("default", h.ID, time.Now()) {
		t.Fatal("Host muss stumm sein")
	}
	a.do("POST", "/api/mute", map[string]any{"scope": "all", "mode": "mute", "until": -1})
	_, st = a.do("GET", "/api/state", nil)
	if !strings.Contains(st, `"forever":true`) {
		t.Fatal("dauerhaft fehlt")
	}
	// Prüfung pausieren mit Ablauf
	a.do("POST", "/api/mute", map[string]any{"scope": "host", "id": h.ID, "mode": "pause", "until": time.Now().Add(time.Hour).UnixMilli()})
	_, hs := a.do("GET", "/api/hosts", nil)
	if !strings.Contains(hs, `"paused":true`) || !strings.Contains(hs, "pauseUntil") {
		t.Fatal(hs)
	}
	srv.m.resumePaused(time.Now().Add(2 * time.Hour).UnixMilli())
	_, hs = a.do("GET", "/api/hosts", nil)
	if strings.Contains(hs, `"paused":true`) {
		t.Fatal("Pause muss automatisch enden", hs)
	}
	if code, _ := a.do("POST", "/api/mute", map[string]any{"scope": "all", "mode": "mute", "until": 5}); code != 400 {
		t.Fatal("Vergangenheit muss abgelehnt werden")
	}
}

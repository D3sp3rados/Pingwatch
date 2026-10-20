package main

import (
	"strings"
	"testing"
	"time"
)

func TestOperatorUserAndExpiry(t *testing.T) {
	_, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("admin", "admin-pass-123")
	code, body := admin.do("POST", "/api/users", map[string]any{"email": "kollege@rst.de", "role": "admin", "super": true})
	if code != 200 {
		t.Fatal(code, body)
	}
	var r struct{ TempPassword string }
	mustJSON(t, body, &r)
	k := newClient(t, ts)
	k.login("kollege@rst.de", r.TempPassword)
	if code, _ := k.do("GET", "/api/admin/orgs", nil); code != 200 {
		t.Fatal("Betreiber-Benutzer muss Kunden sehen", code)
	}
	// Kunde mit abgelaufenem Testzugang
	code, body = admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Test GmbH", "plan": "basis", "ownerEmail": "t@test.de", "expires": time.Now().Add(-time.Hour).UnixMilli()})
	if code != 200 {
		t.Fatal(code, body)
	}
	mustJSON(t, body, &r)
	code, body = k.do("POST", "/api/app/login", map[string]string{"email": "t@test.de", "password": r.TempPassword})
	if code == 200 || !strings.Contains(body, "abgelaufen") {
		t.Fatalf("abgelaufen: %d %s", code, body)
	}
	// Kunden-Inhaber kann sich nicht selbst zum Betreiber machen
	code, body = admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Normal", "plan": "basis", "ownerEmail": "n@normal.de"})
	mustJSON(t, body, &r)
	n := newClient(t, ts)
	n.login("n@normal.de", r.TempPassword)
	n.do("POST", "/api/users", map[string]any{"email": "boese@normal.de", "role": "admin", "super": true})
	_, body = n.do("GET", "/api/users", nil)
	if strings.Contains(body, `"super":true`) {
		t.Fatal("Kunde darf keine Betreiber anlegen")
	}
}

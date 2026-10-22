package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestMyProfile(t *testing.T) {
	_, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")

	// Betreiber-Login hat kein eigenes Konto
	if code, _ := admin.do("POST", "/api/me/profile", map[string]string{"name": "X"}); code != http.StatusBadRequest {
		t.Fatalf("admin: %d", code)
	}

	var a, b struct{ ID, TempPassword string }
	_, body := admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde A", "plan": "basis", "ownerEmail": "a@kunde-a.de"})
	mustJSON(t, body, &a)
	_, body = admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde B", "plan": "basis", "ownerEmail": "b@kunde-b.de"})
	mustJSON(t, body, &b)

	ca := newClient(t, ts)
	ca.login("a@kunde-a.de", a.TempPassword)

	// Name ohne Passwort
	if code, body := ca.do("POST", "/api/me/profile", map[string]string{"name": "  Anna A  "}); code != 200 {
		t.Fatal(code, body)
	}
	var me struct{ Me map[string]any }
	_, body = ca.do("GET", "/api/me", nil)
	mustJSON(t, body, &me)
	if me.Me["name"] != "Anna A" {
		t.Fatalf("Name: %v", me.Me)
	}

	// E-Mail ohne / mit falschem Passwort
	if code, _ := ca.do("POST", "/api/me/profile", map[string]string{"email": "neu@kunde-a.de"}); code != http.StatusForbidden {
		t.Fatalf("ohne Passwort: %d", code)
	}
	if code, _ := ca.do("POST", "/api/me/profile", map[string]string{"email": "neu@kunde-a.de", "currentPassword": "falsch123"}); code != http.StatusForbidden {
		t.Fatalf("falsches Passwort: %d", code)
	}
	// doppelt (auch andere Org, Groß/Klein egal)
	if code, _ := ca.do("POST", "/api/me/profile", map[string]string{"email": "B@Kunde-B.de", "currentPassword": a.TempPassword}); code != http.StatusConflict {
		t.Fatalf("doppelt: %d", code)
	}
	// ungültig
	if code, _ := ca.do("POST", "/api/me/profile", map[string]string{"email": "kaputt", "currentPassword": a.TempPassword}); code != http.StatusBadRequest {
		t.Fatalf("ungültig: %d", code)
	}
	// Erfolg
	code, body := ca.do("POST", "/api/me/profile", map[string]string{"email": "neu@kunde-a.de", "currentPassword": a.TempPassword})
	if code != 200 {
		t.Fatal(code, body)
	}
	// Sitzung bleibt gültig
	if code, _ := ca.do("GET", "/api/me", nil); code != 200 {
		t.Fatalf("Sitzung nach Änderung: %d", code)
	}
	// Login: neue Mail geht, alte nicht
	cn := newClient(t, ts)
	cn.login("neu@kunde-a.de", a.TempPassword)
	co := newClient(t, ts)
	if code, _ := co.do("POST", "/api/app/login", map[string]string{"email": "a@kunde-a.de", "password": a.TempPassword}); code == 200 {
		t.Fatal("alte E-Mail funktioniert noch")
	}
	// Passwort ändern über /api/settings, danach tempPw weg
	_, body = cn.do("GET", "/api/me", nil)
	mustJSON(t, body, &me)
	if me.Me["tempPw"] != true {
		t.Fatalf("tempPw fehlt: %v", me.Me)
	}
	if code, body := cn.do("POST", "/api/settings", map[string]string{"oldPassword": a.TempPassword, "newPassword": "NeuesPasswort1"}); code != 200 {
		t.Fatal(code, body)
	}
	cn2 := newClient(t, ts)
	cn2.login("neu@kunde-a.de", "NeuesPasswort1")
	_, body = cn2.do("GET", "/api/me", nil)
	var raw map[string]map[string]any
	json.Unmarshal([]byte(body), &raw)
	if _, ok := raw["me"]["tempPw"]; ok {
		t.Fatalf("tempPw noch gesetzt: %v", raw["me"])
	}
}

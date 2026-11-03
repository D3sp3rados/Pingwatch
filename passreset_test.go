package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPassReset(t *testing.T) {
	s, ts := newTestServer(t)
	s.m.mu.Lock()
	s.m.cfg.Users = append(s.m.cfg.Users, User{ID: "u1", Email: "tim@test.de", Org: "default", Role: "owner", PassHash: hashPassword("altes-pw-123")})
	s.m.mu.Unlock()
	post := func(path string, v url.Values) string {
		resp, err := http.PostForm(ts.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	// Antwort verrät nicht, ob es die Adresse gibt
	a := post("/forgot", url.Values{"email": {"tim@test.de"}})
	b := post("/forgot", url.Values{"email": {"gibtsnicht@test.de"}})
	if !strings.Contains(a, "unterwegs") || !strings.Contains(b, "unterwegs") {
		t.Fatal("forgot:", a)
	}
	u := s.m.userByID("u1")
	tok := s.resetToken(u, time.Now().Add(time.Hour))
	if r := post("/reset", url.Values{"t": {tok}, "p1": {"neues-pw-123"}, "p2": {"anderes-123"}}); !strings.Contains(r, "stimmen nicht") {
		t.Fatal("mismatch:", r)
	}
	if r := post("/reset", url.Values{"t": {tok}, "p1": {"neues-pw-123"}, "p2": {"neues-pw-123"}}); !strings.Contains(r, "Passwort geändert") {
		t.Fatal("reset:", r)
	}
	if !checkPassword(s.m.userByID("u1").PassHash, "neues-pw-123") {
		t.Fatal("Passwort nicht gesetzt")
	}
	if r := post("/reset", url.Values{"t": {tok}, "p1": {"drittes-pw-1"}, "p2": {"drittes-pw-1"}}); !strings.Contains(r, "ungültig") {
		t.Fatal("Link darf nur einmal gehen:", r)
	}
	if s.resetUser(s.resetToken(s.m.userByID("u1"), time.Now().Add(-time.Minute))) != nil {
		t.Fatal("abgelaufener Link gilt noch")
	}
	// mit Zwei-Faktor
	sec := newTOTPSecret()
	s.m.mu.Lock()
	s.m.cfg.Users[len(s.m.cfg.Users)-1].TOTP = sec
	s.m.mu.Unlock()
	tok = s.resetToken(s.m.userByID("u1"), time.Now().Add(time.Hour))
	if r := post("/reset", url.Values{"t": {tok}, "p1": {"viertes-pw-1"}, "p2": {"viertes-pw-1"}, "code": {"000000"}}); !strings.Contains(r, "Zwei-Faktor") || strings.Contains(r, "Passwort geändert") {
		t.Fatal("2FA nicht geprüft:", r)
	}
	code := totpAt(sec, uint64(time.Now().Unix()/30))
	if r := post("/reset", url.Values{"t": {tok}, "p1": {"viertes-pw-1"}, "p2": {"viertes-pw-1"}, "code": {code}}); !strings.Contains(r, "Passwort geändert") {
		t.Fatal("2FA richtig:", r)
	}
}

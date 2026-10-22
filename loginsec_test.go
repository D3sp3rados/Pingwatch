package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoginLockAnd2FA(t *testing.T) {
	s, ts := newTestServer(t)
	loginGuard.Lock()
	loginGuard.fails, loginGuard.until = map[string][]time.Time{}, map[string]time.Time{}
	loginGuard.Unlock()
	totpState.Lock()
	totpState.used = map[string]uint64{}
	totpState.Unlock()
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(v url.Values) string {
		resp, err := cl.PostForm(ts.URL+"/login", v)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	// Sperre nach 5 Fehlversuchen
	for i := 0; i < 5; i++ {
		post(url.Values{"email": {"admin"}, "password": {"falsch-falsch"}})
	}
	if loc := post(url.Values{"email": {"admin"}, "password": {"admin-pass-123"}}); !strings.Contains(loc, "gesperrt") {
		t.Fatal("keine Sperre:", loc)
	}
	loginGuard.Lock()
	loginGuard.fails, loginGuard.until = map[string][]time.Time{}, map[string]time.Time{}
	loginGuard.Unlock()
	// Zwei-Faktor für den Betreiber einrichten
	sec := newTOTPSecret()
	s.m.mu.Lock()
	s.m.cfg.AdminTOTP = sec
	s.m.mu.Unlock()
	if loc := post(url.Values{"email": {"admin"}, "password": {"admin-pass-123"}}); loc != "/login?step=2fa" {
		t.Fatal("kein zweiter Schritt:", loc)
	}
	if loc := post(url.Values{"step": {"2fa"}, "code": {"000000"}}); !strings.Contains(loc, "step=2fa&f=1") {
		t.Fatal("falscher Code angenommen:", loc)
	}
	if loc := post(url.Values{"step": {"2fa"}, "code": {totpAt(sec, uint64(time.Now().Unix()/30))}}); loc != "/" {
		t.Fatal("richtiger Code abgelehnt:", loc)
	}
	// App: Code ans Passwort angehängt
	totpState.Lock()
	totpState.used = map[string]uint64{}
	totpState.Unlock()
	c := newClient(t, ts)
	if code, _ := c.do("POST", "/api/app/login", map[string]string{"email": "", "password": "admin-pass-123"}); code != 401 {
		t.Fatal("App ohne Code", code)
	}
	if code, b := c.do("POST", "/api/app/login", map[string]string{"email": "", "password": "admin-pass-123" + totpAt(sec, uint64(time.Now().Unix()/30))}); code != 200 {
		t.Fatal("App mit angehängtem Code", code, b)
	}
}

package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func hostGet(t *testing.T, base, host, path string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Host = host
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestLandingRouting(t *testing.T) {
	_, ts := newTestServer(t)
	// Produktseite auf pingwacht.de
	resp, body := hostGet(t, ts.URL, "pingwacht.de", "/")
	if resp.StatusCode != 200 || !strings.Contains(body, "bevor der Kunde anruft") || !strings.Contains(body, "https://app.pingwacht.de/signup") {
		t.Fatalf("Landing: %d %.200s", resp.StatusCode, body)
	}
	if strings.Contains(body, "__APP__") || strings.Contains(body, "3,90") {
		t.Fatal("Platzhalter oder Partnerpreis auf der Produktseite")
	}
	if !strings.Contains(body, "9,80 €") || !strings.Contains(body, "19,80 €") {
		t.Fatal("Preise fehlen")
	}
	// www → 301
	resp, _ = hostGet(t, ts.URL, "www.pingwacht.de", "/preise?x=1")
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://pingwacht.de/preise?x=1" {
		t.Fatalf("www: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Login/Signup → App-Adresse
	resp, _ = hostGet(t, ts.URL, "pingwacht.de", "/login")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "https://app.pingwacht.de/login" {
		t.Fatalf("login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Assets, robots, sitemap, Impressum
	for _, p := range []string{"/landing/overview.webp", "/robots.txt", "/sitemap.xml", "/impressum", "/icon.svg"} {
		if resp, _ := hostGet(t, ts.URL, "pingwacht.de", p); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	// App-Host unverändert: "/" ohne Anmeldung → Login
	resp, body = hostGet(t, ts.URL, "app.pingwacht.de", "/")
	if resp.StatusCode == 200 && strings.Contains(body, "bevor der Kunde anruft") {
		t.Fatal("App-Host zeigt Produktseite")
	}
	// LANDING_HOSTS überschreibbar
	os.Setenv("LANDING_HOSTS", "produkt.example")
	defer os.Unsetenv("LANDING_HOSTS")
	if _, body := hostGet(t, ts.URL, "produkt.example", "/"); !strings.Contains(body, "bevor der Kunde anruft") {
		t.Fatal("LANDING_HOSTS")
	}
	if _, body := hostGet(t, ts.URL, "pingwacht.de", "/"); strings.Contains(body, "bevor der Kunde anruft") {
		t.Fatal("pingwacht.de trotz LANDING_HOSTS")
	}
}

func TestSignupTrialLifecycle(t *testing.T) {
	os.Setenv("ALLOW_SIGNUP", "1")
	os.Setenv("TRIAL_DAYS", "14")
	defer os.Unsetenv("ALLOW_SIGNUP")
	defer os.Unsetenv("TRIAL_DAYS")
	var mu sync.Mutex
	var sent []string
	old := trialMail
	trialMail = func(to []string, subject, text string) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, strings.Join(to, ",")+"|"+subject)
		return nil
	}
	defer func() { trialMail = old }()
	os.Setenv("PROVIDER_EMAIL", "betreiber@example.de")
	defer os.Unsetenv("PROVIDER_EMAIL")
	signupRL.Lock()
	signupRL.hits = map[string][]time.Time{}
	signupRL.Unlock()

	s, ts := newTestServer(t)
	post := func(form url.Values, ip string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/signup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", ip)
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	f := url.Values{"org": {"Testfirma"}, "name": {"Tina"}, "email": {"tina@testfirma.de"}, "password": {"geheim1234"}, "terms": {"on"}}
	// Honeypot
	hp := url.Values{}
	for k, v := range f {
		hp[k] = v
	}
	hp.Set("website", "http://spam")
	hp.Set("email", "bot@spam.de")
	if resp, body := post(hp, "10.0.0.9"); resp.StatusCode != 200 || !strings.Contains(body, "nicht möglich") || s.m.userByEmail("bot@spam.de") != nil {
		t.Fatal("Honeypot")
	}
	// Erfolg
	resp, _ := post(f, "10.0.0.1")
	if resp.StatusCode != 302 || !strings.Contains(resp.Header.Get("Location"), "install") {
		t.Fatalf("Registrierung: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	u := s.m.userByEmail("tina@testfirma.de")
	if u == nil {
		t.Fatal("Benutzer fehlt")
	}
	o := s.m.orgByID(u.Org)
	if o.Plan != "pro" || o.Sites != 1 || o.TrialUntil < time.Now().Add(13*24*time.Hour).UnixMilli() || o.TrialUntil > time.Now().Add(15*24*time.Hour).UnixMilli() {
		t.Fatalf("Testphase: %+v", *o)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(sent) != 2 || !strings.HasPrefix(sent[0], "tina@testfirma.de|Willkommen") || !strings.HasPrefix(sent[1], "betreiber@example.de|") {
		t.Fatalf("Mails: %v", sent)
	}
	sent = nil
	mu.Unlock()
	// Rate-Limit: 3 pro Stunde und IP
	for i := 0; i < 2; i++ {
		post(url.Values{"org": {"X"}, "email": {"x" + string(rune('a'+i)) + "@x.de"}, "password": {"geheim1234"}, "terms": {"on"}}, "10.0.0.2")
	}
	post(url.Values{"org": {"X"}, "email": {"xc@x.de"}, "password": {"geheim1234"}, "terms": {"on"}}, "10.0.0.2")
	if _, body := post(url.Values{"org": {"X"}, "email": {"xd@x.de"}, "password": {"geheim1234"}, "terms": {"on"}}, "10.0.0.2"); !strings.Contains(body, "Zu viele") {
		t.Fatal("Rate-Limit greift nicht")
	}

	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	sent = nil
	mu.Unlock()
	// Hosts anlegen (8 Stück), dann Testphase ablaufen lassen
	for i := 0; i < 8; i++ {
		s.m.UpsertHost(HostCfg{Name: "H" + string(rune('0'+i)), Addr: "10.9.0." + string(rune('1'+i)), Site: "cloud", Org: o.ID, IntervalMs: 60000})
	}
	// Erinnerung 3 Tage vorher
	s.m.mu.Lock()
	for i := range s.m.cfg.Orgs {
		if s.m.cfg.Orgs[i].ID == o.ID {
			s.m.cfg.Orgs[i].TrialUntil = time.Now().Add(2 * 24 * time.Hour).UnixMilli()
		}
	}
	s.m.mu.Unlock()
	s.m.trialCheck(time.Now())
	s.m.trialCheck(time.Now())
	mu.Lock()
	if len(sent) != 1 || !strings.Contains(sent[0], "endet am") {
		t.Fatalf("Erinnerung: %v", sent)
	}
	sent = nil
	mu.Unlock()
	// Ablauf
	s.m.trialCheck(time.Now().Add(3 * 24 * time.Hour))
	o = s.m.orgByID(u.Org)
	if o.Plan != "free" || !o.TrialEnded {
		t.Fatalf("nach Ablauf: %+v", *o)
	}
	active, paused := 0, 0
	s.m.mu.RLock()
	for _, h := range s.m.cfg.Hosts {
		if h.Org == o.ID {
			if h.Paused {
				paused++
			} else {
				active++
			}
		}
	}
	s.m.mu.RUnlock()
	if active != 5 || paused != 3 {
		t.Fatalf("Hosts aktiv %d pausiert %d", active, paused)
	}
	mu.Lock()
	if len(sent) != 2 || !strings.Contains(sent[0], "Testphase beendet") || !strings.HasPrefix(sent[1], "betreiber@example.de|") {
		t.Fatalf("Ablauf-Mails: %v", sent)
	}
	mu.Unlock()
	// nichts doppelt
	s.m.trialCheck(time.Now().Add(4 * 24 * time.Hour))
	mu.Lock()
	if len(sent) != 2 {
		t.Fatalf("doppelt: %v", sent)
	}
	mu.Unlock()

	// Betreiber verlängert → wieder Pro
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	until := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if code, body := c.do("POST", "/api/admin/orgs", map[string]any{"id": o.ID, "trialUntil": until}); code != 200 {
		t.Fatal(code, body)
	}
	o = s.m.orgByID(u.Org)
	if o.Plan != "pro" || o.TrialEnded || o.TrialUntil != until {
		t.Fatalf("verlängert: %+v", *o)
	}
	// beenden (Paket regulär)
	c.do("POST", "/api/admin/orgs", map[string]any{"id": o.ID, "plan": "basis", "billing": "invoice", "trialUntil": 0})
	o = s.m.orgByID(u.Org)
	if o.Plan != "basis" || o.TrialUntil != 0 {
		t.Fatalf("beendet: %+v", *o)
	}
}

func TestSignupDisabled(t *testing.T) {
	os.Unsetenv("ALLOW_SIGNUP")
	_, ts := newTestServer(t)
	if resp, _ := hostGet(t, ts.URL, "app.pingwacht.de", "/signup"); resp.StatusCode != 404 {
		t.Fatalf("Signup ohne ALLOW_SIGNUP: %d", resp.StatusCode)
	}
}

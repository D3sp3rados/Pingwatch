package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAlertsAndReport(t *testing.T) {
	s, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")

	// Kunde im Free-Paket: Alarme gesperrt
	code, body := admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde A", "plan": "free", "ownerEmail": "a@kunde.de"})
	if code != 200 {
		t.Fatal(code, body)
	}
	var cr struct{ ID, TempPassword string }
	mustJSON(t, body, &cr)
	a := newClient(t, ts)
	a.login("a@kunde.de", cr.TempPassword)
	if code, _ := a.do("POST", "/api/alerts", map[string]any{"type": "webhook", "target": "https://x.example", "outage": true}); code != 402 {
		t.Fatalf("free darf keine Alarme: %d", code)
	}
	if code, _ := a.do("GET", "/api/report.pdf", nil); code != 402 {
		t.Fatalf("free darf keinen Bericht: %d", code)
	}
	admin.do("POST", "/api/admin/orgs", map[string]any{"id": cr.ID, "plan": "pro", "sites": 1})

	// Empfänger
	var mu sync.Mutex
	got := map[string][]byte{}
	hdrs := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = b
		hdrs[r.URL.Path] = r.Header
		mu.Unlock()
		w.Write([]byte(`{"ok":true,"result":[{"message":{"chat":{"id":4711,"type":"private","first_name":"Raphi"}}}]}`))
	}))
	defer srv.Close()
	oldTg := telegramAPI
	telegramAPI = srv.URL
	defer func() { telegramAPI = oldTg }()
	var mails [][]byte
	var mailTo [][]string
	oldMail := sendMailFn
	sendMailFn = func(to []string, msg []byte) error {
		mu.Lock()
		mails = append(mails, msg)
		mailTo = append(mailTo, to)
		mu.Unlock()
		return nil
	}
	defer func() { sendMailFn = oldMail }()

	// http:// nur im Test erlaubt → Validierung verlangt https, darum direkt in die Konfiguration
	if code, body := a.do("POST", "/api/alerts", map[string]any{"type": "webhook", "target": "http://x", "outage": true}); code != 400 {
		t.Fatalf("http-URL muss abgelehnt werden: %d %s", code, body)
	}
	if code, body := a.do("POST", "/api/alerts", map[string]any{"type": "email", "target": "Technik@Kunde.de; chef@kunde.de", "outage": true, "report": true}); code != 200 {
		t.Fatal(code, body)
	}
	if code, body := a.do("POST", "/api/alerts", map[string]any{"type": "telegram", "target": "4711", "secret": "123:ABC", "outage": true, "loss": true}); code != 200 {
		t.Fatal(code, body)
	}
	if code, body := a.do("POST", "/api/alerts", map[string]any{"type": "webhook", "target": "https://example.invalid/hook", "secret": "geheim", "outage": true}); code != 200 {
		t.Fatal(code, body)
	}
	s.m.mu.Lock()
	for i := range s.m.cfg.Channels {
		if s.m.cfg.Channels[i].Type == "webhook" {
			s.m.cfg.Channels[i].Target = srv.URL + "/hook"
		}
	}
	s.m.cfg.Channels = append(s.m.cfg.Channels, Channel{ID: "teams1", Org: cr.ID, Type: "teams", Name: "Teams", Target: srv.URL + "/teams", Outage: true})
	// Kanal einer anderen Org darf nichts bekommen
	s.m.cfg.Channels = append(s.m.cfg.Channels, Channel{ID: "fremd", Org: "default", Type: "teams", Target: srv.URL + "/fremd", Outage: true})
	s.m.mu.Unlock()

	// Liste: Secret wird nicht ausgeliefert
	_, body = a.do("GET", "/api/alerts", nil)
	if strings.Contains(body, "geheim") || strings.Contains(body, "123:ABC") {
		t.Fatal("Secret sichtbar: " + body)
	}
	var lst struct {
		Channels []channelView
		Allowed  bool
	}
	mustJSON(t, body, &lst)
	if len(lst.Channels) != 4 || !lst.Allowed {
		t.Fatalf("Kanäle: %s", body)
	}
	if lst.Channels[0].Target != "technik@kunde.de, chef@kunde.de" {
		t.Fatalf("E-Mail normalisiert: %q", lst.Channels[0].Target)
	}

	s.m.dispatchAlerts(notice{"outage", "h1", "🔴 Router antwortet nicht", "Seit 10:00 keine Antwort", cr.ID})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 && len(mails) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if _, ok := got["/fremd"]; ok {
		t.Fatal("Alarm an fremde Org")
	}
	var tg map[string]any
	json.Unmarshal(got["/bot123:ABC/sendMessage"], &tg)
	if tg["chat_id"] != "4711" || !strings.Contains(tg["text"].(string), "Router") {
		t.Fatalf("telegram: %s", got["/bot123:ABC/sendMessage"])
	}
	if !bytes.Contains(got["/teams"], []byte("AdaptiveCard")) {
		t.Fatalf("teams: %s", got["/teams"])
	}
	if !strings.HasPrefix(hdrs["/hook"].Get("X-PingWacht-Signature"), "sha256=") || !bytes.Contains(got["/hook"], []byte(`"event":"outage"`)) {
		t.Fatalf("webhook: %s %v", got["/hook"], hdrs["/hook"])
	}
	if len(mails) != 1 || len(mailTo[0]) != 2 || !bytes.Contains(mails[0], []byte("Subject: =?utf-8?q?")) {
		t.Fatalf("mail: %d %v", len(mails), mailTo)
	}
	mu.Unlock()

	// Paketverlust nur an Telegram (abonniert), Drosselung 10 min
	delete(got, "/bot123:ABC/sendMessage")
	s.m.dispatchAlerts(notice{"loss", "h1", "Verlust", "x", cr.ID})
	s.m.dispatchAlerts(notice{"loss", "h1", "Verlust", "x", cr.ID})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if _, ok := got["/bot123:ABC/sendMessage"]; !ok {
		t.Fatal("loss an telegram fehlt")
	}
	mu.Unlock()

	// Telegram-Chats finden
	code, body = a.do("POST", "/api/alerts/telegram", map[string]any{"token": "123:ABC"})
	if code != 200 || !strings.Contains(body, "4711") || !strings.Contains(body, "Raphi") {
		t.Fatalf("telegram chats: %d %s", code, body)
	}

	// Bericht
	h := s.m.UpsertHost(HostCfg{Name: "Router Büro", Addr: "192.168.1.1", Site: "cloud", Method: "icmp", IntervalMs: 1000, Org: cr.ID})
	_ = h
	s.m.evMu.Lock()
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 9, 0, 0, 0, time.Local).UnixMilli()
	s.m.events = append(s.m.events, &Event{ID: 99, HostID: h.ID, HostName: "Router Büro", Org: cr.ID, Type: "outage", Start: start, End: start + 90*60000})
	s.m.evMu.Unlock()
	code, body = a.do("GET", "/api/report.pdf?month="+now.Format("2006-01"), nil)
	if code != 200 || !strings.HasPrefix(body, "%PDF-1.4") || !strings.Contains(body, "%%EOF") {
		t.Fatalf("pdf: %d %.200s", code, body)
	}
	if os.Getenv("PW_PDF_OUT") != "" {
		os.WriteFile(os.Getenv("PW_PDF_OUT"), []byte(body), 0o644)
	}
	mails = nil
	code, body = a.do("POST", "/api/report/send", map[string]any{"month": now.Format("2006-01")})
	if code != 200 || len(mails) != 1 || !bytes.Contains(mails[0], []byte("application/pdf")) {
		t.Fatalf("report send: %d %s %d", code, body, len(mails))
	}
	// Testnachricht
	code, body = a.do("POST", "/api/alerts/test", map[string]any{"id": "teams1"})
	if code != 200 {
		t.Fatal(code, body)
	}
}

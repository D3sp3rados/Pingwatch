package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	os.Setenv("PINGWATCH_SIM", "1")
	simMode = true
	nbState.Lock()
	nbState.b = map[string]*nbBucket{}
	nbState.Unlock()
	m := NewMonitor(NewFileStore(t.TempDir()))
	m.notices = make(chan notice, 100)
	s := &Server{m: m, envPass: "admin-pass-123"}
	m.Start()
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() { ts.Close() })
	return s, ts
}

type client struct {
	t   *testing.T
	ts  *httptest.Server
	hc  *http.Client
	tok string
}

func newClient(t *testing.T, ts *httptest.Server) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, ts: ts, hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *client) do(method, path string, body any) (int, string) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.ts.URL+path, rd)
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (c *client) login(email, pw string) {
	code, body := c.do("POST", "/api/app/login", map[string]string{"email": email, "password": pw})
	if code != 200 {
		c.t.Fatalf("login %s: %d %s", email, code, body)
	}
	var r struct{ Token string }
	json.Unmarshal([]byte(body), &r)
	c.tok = r.Token
}

func mustJSON(t *testing.T, s string, v any) {
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("json: %v in %s", err, s)
	}
}

func TestTenantIsolationAndLimits(t *testing.T) {
	s, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")

	// Betreiber legt zwei Kunden an
	var a, b struct {
		ID, TempPassword string
	}
	code, body := admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde A", "plan": "free", "ownerEmail": "a@kunde-a.de"})
	if code != 200 {
		t.Fatal(body)
	}
	mustJSON(t, body, &a)
	_, body = admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde B", "plan": "basis", "sites": 2, "ownerEmail": "b@kunde-b.de"})
	mustJSON(t, body, &b)
	if a.TempPassword == "" || b.TempPassword == "" {
		t.Fatal("kein Startpasswort")
	}

	ca := newClient(t, ts)
	ca.login("a@kunde-a.de", a.TempPassword)
	cb := newClient(t, ts)
	cb.login("B@Kunde-B.de", b.TempPassword) // Groß/Klein egal

	// Kunde A (Free): 1 Standort, 5 Hosts
	code, body = ca.do("POST", "/api/sites", map[string]string{"name": "Büro A"})
	if code != 200 {
		t.Fatal(body)
	}
	var siteA SiteCfg
	mustJSON(t, body, &siteA)
	if siteA.Org != a.ID {
		t.Fatalf("Standort hat falsche Org: %s", siteA.Org)
	}
	if code, body = ca.do("POST", "/api/sites", map[string]string{"name": "Zweiter"}); code != http.StatusPaymentRequired {
		t.Fatalf("2. Standort im Free-Paket erlaubt: %d %s", code, body)
	}
	var hostA HostCfg
	for i := 0; i < 5; i++ {
		code, body = ca.do("POST", "/api/hosts", map[string]any{"name": fmt.Sprint("H", i), "addr": "10.0.0.1", "site": siteA.ID, "intervalMs": 500})
		if code != 200 {
			t.Fatalf("Host %d: %s", i, body)
		}
		mustJSON(t, body, &hostA)
	}
	if hostA.IntervalMs != 5000 {
		t.Fatalf("Mindestintervall Free nicht erzwungen: %d", hostA.IntervalMs)
	}
	if code, _ = ca.do("POST", "/api/hosts", map[string]any{"addr": "10.0.0.9"}); code != http.StatusPaymentRequired {
		t.Fatalf("6. Host im Free-Paket erlaubt: %d", code)
	}

	// Kunde B sieht nichts von A und kann nichts davon ändern
	_, body = cb.do("GET", "/api/state", nil)
	var st struct {
		Hosts []struct{ ID string }
		Sites []struct{ ID string }
		Org   struct {
			ID, Plan  string
			SiteLimit int
			HostLimit int
		}
	}
	mustJSON(t, body, &st)
	if len(st.Hosts) != 0 || len(st.Sites) != 1 || st.Org.ID != b.ID || st.Org.SiteLimit != 2 || st.Org.HostLimit != 50 {
		t.Fatalf("Kunde B sieht falsche Daten: %+v", st)
	}
	for _, try := range []struct{ m, p string }{
		{"GET", "/api/series?host=" + hostA.ID},
		{"GET", "/api/stats?host=" + hostA.ID},
		{"DELETE", "/api/hosts?id=" + hostA.ID},
		{"DELETE", "/api/sites?id=" + siteA.ID},
		{"GET", "/download/agent.json?site=" + siteA.ID},
		{"GET", "/api/export/samples.csv?host=" + hostA.ID},
	} {
		if code, _ := cb.do(try.m, try.p, nil); code != 404 {
			t.Fatalf("%s %s für fremden Kunden: %d", try.m, try.p, code)
		}
	}
	if code, _ = cb.do("POST", "/api/hosts", map[string]any{"id": hostA.ID, "addr": "1.1.1.1"}); code != 404 {
		t.Fatalf("fremden Host überschreiben: %d", code)
	}
	if code, _ = cb.do("POST", "/api/hosts", map[string]any{"addr": "1.1.1.1", "site": siteA.ID}); code != 400 {
		t.Fatalf("Host an fremdem Standort: %d", code)
	}
	if code, _ = cb.do("GET", "/api/admin/orgs", nil); code != 403 {
		t.Fatalf("Kunde sieht Kundenliste: %d", code)
	}

	// Ereignisse sind getrennt
	h := s.m.host(hostA.ID)
	now := time.Now().UnixMilli()
	for i := 0; i < 4; i++ {
		s.m.Ingest(h, now+int64(i)*1000, -1, "", "x")
	}
	_, body = ca.do("GET", "/api/events", nil)
	if !strings.Contains(body, hostA.ID) {
		t.Fatalf("Kunde A sieht eigenes Ereignis nicht: %s", body)
	}
	_, body = cb.do("GET", "/api/events", nil)
	if body != "[]\n" {
		t.Fatalf("Kunde B sieht fremde Ereignisse: %s", body)
	}

	// Rollen: Viewer darf lesen, nicht ändern
	code, body = ca.do("POST", "/api/users", map[string]string{"email": "leser@kunde-a.de", "role": "viewer"})
	var nu struct{ TempPassword string }
	mustJSON(t, body, &nu)
	cv := newClient(t, ts)
	cv.login("leser@kunde-a.de", nu.TempPassword)
	if code, _ = cv.do("GET", "/api/state", nil); code != 200 {
		t.Fatal("Viewer kann nicht lesen")
	}
	if code, _ = cv.do("DELETE", "/api/hosts?id="+hostA.ID, nil); code != 403 {
		t.Fatalf("Viewer kann löschen: %d", code)
	}
	// eigenes Passwort ändern geht, alte Sitzung wird ungültig
	if code, body = cv.do("POST", "/api/settings", map[string]string{"oldPassword": nu.TempPassword, "newPassword": "neues-pw-123"}); code != 200 {
		t.Fatalf("Viewer Passwort: %d %s", code, body)
	}
	if code, _ = cv.do("GET", "/api/state", nil); code != 401 {
		t.Fatalf("alte Sitzung nach Passwortwechsel noch gültig: %d", code)
	}
	cv.login("leser@kunde-a.de", "neues-pw-123")

	// letzter Inhaber kann nicht herabgestuft werden
	_, body = ca.do("GET", "/api/users", nil)
	var us []userView
	mustJSON(t, body, &us)
	for _, u := range us {
		if u.Role == "owner" {
			if code, _ = ca.do("POST", "/api/users", map[string]string{"id": u.ID, "role": "viewer"}); code != 400 {
				t.Fatalf("letzter Inhaber herabgestuft: %d", code)
			}
		}
	}

	// Betreiber wechselt in Kunde A und sieht dessen Hosts
	admin.do("POST", "/api/admin/switch", map[string]string{"org": a.ID})
	_, body = admin.do("GET", "/api/state?org="+a.ID, nil)
	mustJSON(t, body, &st)
	if len(st.Hosts) != 5 {
		t.Fatalf("Betreiber sieht %d Hosts von Kunde A", len(st.Hosts))
	}

	// Kunde löschen räumt alles weg
	if code, _ = admin.do("DELETE", "/api/admin/orgs?id="+a.ID, nil); code != 200 {
		t.Fatal("löschen")
	}
	if s.m.host(hostA.ID) != nil || s.m.userByEmail("a@kunde-a.de") != nil {
		t.Fatal("Daten von Kunde A nicht gelöscht")
	}
	if code, _ = ca.do("GET", "/api/state", nil); code != 401 {
		t.Fatal("Sitzung gelöschter Kunde noch gültig")
	}
}

func TestStripeFlow(t *testing.T) {
	s, ts := newTestServer(t)
	var mu sync.Mutex
	var calls []string
	var lastForm url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		lastForm = r.PostForm
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk_test_x" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/v1/checkout/sessions":
			fmt.Fprint(w, `{"url":"https://checkout.stripe.test/abc"}`)
		case r.URL.Path == "/v1/billing_portal/sessions":
			fmt.Fprint(w, `{"url":"https://portal.stripe.test/xyz"}`)
		case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			q := r.PostForm.Get("items[0][quantity]")
			if q == "" {
				q = "3"
			}
			price := r.PostForm.Get("items[0][price]")
			if price == "" {
				price = "price_basis"
			}
			fmt.Fprintf(w, `{"id":"sub_1","status":"active","customer":"cus_1","metadata":{"org":"%s"},
				"items":{"data":[{"id":"si_1","quantity":%s,"price":{"id":"%s"},"current_period_end":1900000000}]}}`, orgIDForTest, q, price)
		}
	}))
	defer fake.Close()
	stripeAPI = fake.URL
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_x")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_test")
	t.Setenv("STRIPE_PRICE_BASIS", "price_basis")
	t.Setenv("STRIPE_PRICE_PRO", "price_pro")

	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")
	var o struct{ ID, TempPassword string }
	_, body := admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Zahler GmbH", "ownerEmail": "chef@zahler.de"})
	mustJSON(t, body, &o)
	orgIDForTest = o.ID
	c := newClient(t, ts)
	c.login("chef@zahler.de", o.TempPassword)

	// Checkout
	code, body := c.do("POST", "/api/billing/checkout", map[string]any{"plan": "basis", "sites": 3})
	if code != 200 || !strings.Contains(body, "checkout.stripe.test") {
		t.Fatalf("checkout: %d %s", code, body)
	}
	if lastForm.Get("line_items[0][price]") != "price_basis" || lastForm.Get("line_items[0][quantity]") != "3" ||
		lastForm.Get("client_reference_id") != o.ID || lastForm.Get("customer_email") != "chef@zahler.de" || lastForm.Get("mode") != "subscription" {
		t.Fatalf("checkout-Formular: %v", lastForm)
	}

	// Webhook mit falscher Signatur wird abgelehnt
	payload := fmt.Sprintf(`{"type":"checkout.session.completed","data":{"object":{"client_reference_id":"%s","customer":"cus_1","subscription":"sub_1","customer_details":{"email":"buchhaltung@zahler.de"}}}}`, o.ID)
	if code := postWebhook(t, ts, payload, "whsec_falsch"); code != 400 {
		t.Fatalf("falsche Signatur akzeptiert: %d", code)
	}
	if code := postWebhook(t, ts, payload, "whsec_test"); code != 200 {
		t.Fatalf("Webhook: %d", code)
	}
	org := s.m.orgByID(o.ID)
	if org.Plan != "basis" || org.Sites != 3 || org.StripeSub != "sub_1" || org.StripeItem != "si_1" || org.Email != "buchhaltung@zahler.de" || org.Status != "active" {
		t.Fatalf("Org nach Checkout: %+v", org)
	}

	// Upgrade auf Pro mit 4 Standorten
	if code, body = c.do("POST", "/api/billing/change", map[string]any{"plan": "pro", "sites": 4}); code != 200 {
		t.Fatalf("change: %d %s", code, body)
	}
	org = s.m.orgByID(o.ID)
	if org.Plan != "pro" || org.Sites != 4 {
		t.Fatalf("nach Upgrade: %+v", org)
	}
	if lastForm.Get("proration_behavior") != "create_prorations" {
		t.Fatal("keine anteilige Verrechnung")
	}

	// Portal
	if code, body = c.do("POST", "/api/billing/portal", nil); code != 200 || !strings.Contains(body, "portal.stripe.test") {
		t.Fatalf("portal: %d %s", code, body)
	}

	// Kündigung → Free
	del := fmt.Sprintf(`{"type":"customer.subscription.deleted","data":{"object":{"id":"sub_1","status":"canceled","customer":"cus_1","metadata":{"org":"%s"},"items":{"data":[{"id":"si_1","quantity":4,"price":{"id":"price_pro"}}]}}}}`, o.ID)
	postWebhook(t, ts, del, "whsec_test")
	org = s.m.orgByID(o.ID)
	if org.Plan != "free" || org.Status != "canceled" {
		t.Fatalf("nach Kündigung: %+v", org)
	}
}

var orgIDForTest string

func postWebhook(t *testing.T, ts *httptest.Server, payload, secret string) int {
	tsec := fmt.Sprint(time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsec + "." + payload))
	req, _ := http.NewRequest("POST", ts.URL+"/api/stripe/webhook", strings.NewReader(payload))
	req.Header.Set("Stripe-Signature", "t="+tsec+",v1="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("geheim123")
	if !checkPassword(h, "geheim123") || checkPassword(h, "geheim124") || checkPassword("x", "geheim123") {
		t.Fatal("Passwortprüfung fehlerhaft")
	}
}

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPAndDNSCheck(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/404" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "Willkommen bei RST")
	}))
	defer ts.Close()
	old := httpCheckClient
	httpCheckClient = ts.Client()
	defer func() { httpCheckClient = old }()
	h := HostCfg{ID: "web1", Method: "http", Addr: ts.URL}
	if r, _, e := httpCheck(h, 3*time.Second); r < 0 || e != "" {
		t.Fatal("ok erwartet", e)
	}
	if certExpiry("web1") == 0 {
		t.Fatal("Zertifikatsablauf fehlt")
	}
	h.Expect = "Kunde"
	if _, _, e := httpCheck(h, 3*time.Second); !strings.Contains(e, "nicht gefunden") {
		t.Fatal("Text-Prüfung:", e)
	}
	h.Expect, h.Addr = "", ts.URL+"/404"
	if _, _, e := httpCheck(h, 3*time.Second); !strings.Contains(e, "404") {
		t.Fatal("404:", e)
	}
	h.Expect = "404"
	if r, _, e := httpCheck(h, 3*time.Second); r < 0 {
		t.Fatal("erwarteter Status 404:", e)
	}
	if r, _, _ := dnsCheck(HostCfg{Addr: "localhost"}, time.Second); r < 0 {
		t.Fatal("localhost auflösbar")
	}
	if r, _, e := dnsCheck(HostCfg{Addr: "localhost", Expect: "10.9.9.9"}, time.Second); r >= 0 || !strings.Contains(e, "fehlt") {
		t.Fatal("erwartete Adresse:", e)
	}
	hc := HostCfg{Method: "http", Addr: "www.rst.de"}
	hc.normalize()
	if hc.Addr != "https://www.rst.de" || hc.IntervalMs < 30000 {
		t.Fatalf("normalize: %+v", hc)
	}
}

func TestJobsStatusUnifi(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	code, body := c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	if code != 200 {
		t.Fatal(code, body)
	}
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	agentGet := func(method, path string, v any) []byte {
		var rd io.Reader
		if v != nil {
			b, _ := json.Marshal(v)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+site.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	agentGet("GET", "/api/agent/config", nil) // Agent meldet sich → online
	code, body = c.do("POST", "/api/jobs", map[string]any{"site": site.ID, "kind": "scan", "params": map[string]string{"cidr": "8.8.8.0/24"}})
	if code != 400 {
		t.Fatal("öffentliches Netz darf nicht gescannt werden", code)
	}
	code, body = c.do("POST", "/api/jobs", map[string]any{"site": site.ID, "kind": "scan", "params": map[string]string{"cidr": "192.168.5.0/24"}})
	if code != 200 {
		t.Fatal(code, body)
	}
	var jr struct{ ID string }
	mustJSON(t, body, &jr)
	var cfg struct{ Jobs []agentJob }
	json.Unmarshal(agentGet("GET", "/api/agent/config", nil), &cfg)
	if len(cfg.Jobs) != 1 || cfg.Jobs[0].Kind != "scan" {
		t.Fatalf("Auftrag nicht ausgeliefert: %+v", cfg)
	}
	res, err := scanNetwork(cfg.Jobs[0].Params["cidr"])
	if err != nil || len(res.Hits) == 0 {
		t.Fatal("scan (Simulation)", err)
	}
	agentGet("POST", "/api/agent/job", map[string]any{"id": jr.ID, "result": res})
	_, body = c.do("GET", "/api/jobs?id="+jr.ID, nil)
	if !strings.Contains(body, `"status":"done"`) || !strings.Contains(body, "192.168.5.") {
		t.Fatal("Ergebnis:", body)
	}

	// UniFi-Integration-API nachgebildet
	uf := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "k1" {
			w.WriteHeader(401)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/integration/v1/sites"):
			io.WriteString(w, `{"data":[{"id":"s1","name":"Default"}]}`)
		case strings.Contains(r.URL.Path, "/sites/s1/devices"):
			io.WriteString(w, `{"data":[{"name":"USW Büro","model":"USW-24","macAddress":"AA:BB:CC:00:11:22","ipAddress":"192.168.1.20","state":"ONLINE"},{"name":"ohne IP","ipAddress":""}],"totalCount":2}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer uf.Close()
	devs, err := unifiDevices(uf.URL, "k1", "")
	if err != nil || len(devs) != 1 || devs[0].IP != "192.168.1.20" || devs[0].MAC != "aa:bb:cc:00:11:22" {
		t.Fatalf("unifi: %v %+v", err, devs)
	}
	if _, err := unifiDevices(uf.URL, "falsch", ""); err == nil || !strings.Contains(err.Error(), "abgelehnt") {
		t.Fatal("falscher Schlüssel:", err)
	}

	// Statusseite
	h := s.m.UpsertHost(HostCfg{Name: "Webshop", Addr: "10.0.0.5", Site: "cloud", Org: "default", IntervalMs: 1000})
	code, body = c.do("POST", "/api/statuspage", map[string]any{"on": true, "slug": "Ab", "hosts": []string{h.ID}})
	if code != 400 {
		t.Fatal("Slug zu kurz", code)
	}
	code, body = c.do("POST", "/api/statuspage", map[string]any{"on": true, "slug": "rst-status", "title": "RST Dienste", "hosts": []string{h.ID, "fremd"}})
	if code != 200 {
		t.Fatal(code, body)
	}
	resp, _ := http.Get(ts.URL + "/status/rst-status")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "RST Dienste") || !strings.Contains(string(b), "Webshop") || strings.Contains(string(b), "10.0.0.5") {
		t.Fatalf("Statusseite: %d %s", resp.StatusCode, b)
	}
	resp, _ = http.Get(ts.URL + "/status/gibtsnicht")
	if resp.StatusCode != 404 {
		t.Fatal("unbekannte Statusseite")
	}
	resp, _ = http.Get(ts.URL + "/status/rst-status.json")
	var sd statusData
	json.NewDecoder(resp.Body).Decode(&sd)
	if len(sd.Hosts) != 1 || len(sd.Hosts[0].Days) != statusDays {
		t.Fatalf("json: %+v", sd)
	}
}

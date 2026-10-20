package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUniFiLive(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Kunde X"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	if code, body := c.do("POST", "/api/unifi", map[string]any{"site": site.ID, "on": true, "url": "192.168.1.1"}); code != 400 || !strings.Contains(body, "Schlüssel") {
		t.Fatal("ohne Schlüssel", code, body)
	}
	if code, body := c.do("POST", "/api/unifi", map[string]any{"site": site.ID, "on": true, "url": "192.168.1.1", "key": "geheim"}); code != 200 {
		t.Fatal(code, body)
	}
	agent := func(method, path string, v any) string {
		b, _ := json.Marshal(v)
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+site.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		return string(rb)
	}
	cfg := agent("GET", "/api/agent/config", nil)
	if !strings.Contains(cfg, `"key":"geheim"`) || !strings.Contains(cfg, "https://192.168.1.1") {
		t.Fatal("Agent bekommt UniFi-Konfiguration nicht:", cfg)
	}
	_, st := c.do("GET", "/api/state", nil)
	if strings.Contains(st, "geheim") {
		t.Fatal("Schlüssel im Dashboard sichtbar")
	}
	dev := func(state string) []unifiDevice {
		return []unifiDevice{{Name: "AP Halle", MAC: "aa:01", State: "ONLINE"}, {Name: "USW Büro", MAC: "aa:02", State: state, Model: "USW-24"}}
	}
	agent("POST", "/api/agent/unifi", map[string]any{"devices": dev("ONLINE")})
	agent("POST", "/api/agent/unifi", map[string]any{"devices": dev("OFFLINE")})
	time.Sleep(50 * time.Millisecond)
	evs := s.m.queryEvents("default", "", "", map[string]bool{"device": true}, 0, 0, 10)
	if len(evs) != 1 || evs[0].HostName != "USW Büro" || evs[0].End != 0 {
		t.Fatalf("Offline-Ereignis: %+v", evs)
	}
	_, body := c.do("GET", "/api/unifi?site="+site.ID, nil)
	if !strings.Contains(body, `"offline":1`) || !strings.Contains(body, `"total":2`) {
		t.Fatal("Übersicht:", body)
	}
	agent("POST", "/api/agent/unifi", map[string]any{"devices": dev("ONLINE")})
	evs = s.m.queryEvents("default", "", "", map[string]bool{"device": true}, 0, 0, 10)
	if evs[0].End == 0 {
		t.Fatal("Ereignis nicht geschlossen")
	}
	// Konsole 3× nicht erreichbar → Ereignis
	for i := 0; i < 3; i++ {
		agent("POST", "/api/agent/unifi", map[string]any{"error": "Zeitüberschreitung"})
	}
	evs = s.m.queryEvents("default", "", "", map[string]bool{"device": true}, 0, 0, 10)
	if len(evs) != 2 || evs[0].HostName != "UniFi-Konsole" {
		t.Fatalf("Konsole: %+v", evs)
	}
}

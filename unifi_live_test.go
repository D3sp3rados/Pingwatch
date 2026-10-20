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

func TestUniFiRebootWANAndView(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("admin", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Zuhause"})
	c.do("POST", "/api/sites", nil)
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	c.do("POST", "/api/unifi", map[string]any{"site": site.ID, "on": true, "url": "192.168.1.1", "key": "k"})
	s.m.mu.RLock()
	site = s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	gw := func(up int64, wan2 bool) unifiDevice {
		return unifiDevice{Name: "UDM", MAC: "aa:00", Model: "UDM Pro Max", State: "ONLINE", Features: []string{"gateway"}, Uptime: up,
			WAN: []unifiWAN{{Name: "WAN", Up: true, IP: "1.2.3.4", Latency: 12}, {Name: "WAN2", Up: wan2}}}
	}
	ap := func(up int64) unifiDevice {
		return unifiDevice{Name: "U7 Pro", MAC: "aa:01", Model: "U7 Pro", State: "ONLINE", Uptime: up, IP: "192.168.1.9",
			Ping: &unifiPing{N: 3, Lost: 1, Avg: 4, Max: 6}}
	}
	s.m.ingestUniFi(site, []unifiDevice{gw(100000, true), ap(50000)}, "")
	// AP startet neu (Laufzeit 30 s), WAN2 fällt aus
	s.m.ingestUniFi(site, []unifiDevice{gw(100060, false), ap(30)}, "")
	evs := s.m.queryEvents("default", "", "", map[string]bool{"reboot": true, "wan": true}, 0, 0, 10)
	types := ""
	for _, e := range evs {
		types += e.Type + ":" + e.HostName + ";"
	}
	if !strings.Contains(types, "reboot:U7 Pro") || !strings.Contains(types, "wan:UDM · WAN2") {
		t.Fatalf("Ereignisse: %s", types)
	}
	v := s.m.unifiSiteView(&site)
	if v.Boots != 1 || v.WanDown != 1 || len(v.Devices) != 2 || v.Devices[0].Cat != "gateway" {
		t.Fatalf("Ansicht: %+v", v)
	}
	apv := v.Devices[1]
	if len(apv.Boots) != 1 || apv.Loss < 30 || apv.PingAvg != 4 || apv.Avail != 100 || apv.Bar[barSlots-1] != 2 {
		t.Fatalf("AP: %+v", apv)
	}
	_, body := c.do("GET", "/api/state", nil)
	if !strings.Contains(body, `"cat":"ap"`) || strings.Contains(body, `"key":"k"`) {
		t.Fatal("state:", body[:200])
	}
	// WAN2 wieder da → Ereignis geschlossen
	s.m.ingestUniFi(site, []unifiDevice{gw(100120, true), ap(90)}, "")
	evs = s.m.queryEvents("default", "", "", map[string]bool{"wan": true}, 0, 0, 10)
	if evs[0].End == 0 {
		t.Fatal("WAN-Ereignis offen")
	}
}

func TestParseWANs(t *testing.T) {
	g := map[string]any{"wan1": map[string]any{"up": true, "ip": "1.2.3.4"}, "wan2": map[string]any{"up": false, "ip": ""},
		"uptime_stats": map[string]any{"WAN": map[string]any{"latency_average": 11.5, "availability": 99.9}}, "active_wan": "wan1"}
	w := parseWANs(g)
	if len(w) != 2 || !w[0].Up || w[0].Latency != 11.5 || !w[0].Active || w[1].Up || w[1].Name != "WAN2" {
		t.Fatalf("%+v", w)
	}
}

func TestUniFiCategoryAndUnusedWAN(t *testing.T) {
	if c := unifiCategory(unifiDevice{Model: "UAP-AC-M-Pro", Features: []string{"switching", "accessPoint"}}); c != "ap" {
		t.Fatal(c)
	}
	w := parseWANs(map[string]any{"wan1": map[string]any{"up": true, "ip": "1.2.3.4"}, "wan3": map[string]any{"up": false}})
	if len(w) != 1 {
		t.Fatalf("%+v", w)
	}
}

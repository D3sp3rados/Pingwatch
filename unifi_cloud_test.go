package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUniFiCloud bildet api.ui.com nach: Site Manager (/v1/hosts) und Connector-Proxy.
type fakeUniFiCloud struct {
	mu        sync.Mutex
	apOnline  bool
	wanOK     bool
	consoleUp bool
	hits      map[string]int
}

func newFakeUniFiCloud(t *testing.T) (*fakeUniFiCloud, *httptest.Server) {
	f := &fakeUniFiCloud{apOnline: true, consoleUp: true, hits: map[string]int{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits[r.URL.Path]++
		if r.Header.Get("X-API-KEY") != "good-key" {
			w.WriteHeader(401)
			return
		}
		js := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
		p := r.URL.Path
		switch {
		case p == "/v1/hosts":
			state := "connected"
			if !f.consoleUp {
				state = "disconnected"
			}
			js(map[string]any{"data": []map[string]any{
				{"id": "c1", "type": "console", "ipAddress": "203.0.113.5", "reportedState": map[string]any{"name": "UDM Zuhause", "version": "4.3.6", "state": state, "hardware": map[string]any{"name": "UniFi Dream Machine Pro Max"}}},
				{"id": "c404", "type": "console", "reportedState": map[string]any{"hostname": "Alte UDM", "version": "4.0.1", "state": "connected"}},
			}, "httpStatusCode": 200})
		case strings.HasPrefix(p, "/v1/connector/consoles/c429/"):
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
		case strings.HasPrefix(p, "/v1/connector/consoles/c404/"):
			w.WriteHeader(404)
		case strings.HasPrefix(p, "/v1/connector/consoles/c1/"):
			if !f.consoleUp {
				w.WriteHeader(502)
				return
			}
			rest := strings.TrimPrefix(p, "/v1/connector/consoles/c1")
			switch {
			case rest == "/proxy/network/integration/v1/sites":
				js(map[string]any{"data": []map[string]any{{"id": "s1", "name": "Default", "internalReference": "default"}}})
			case rest == "/proxy/network/integration/v1/sites/s1/devices":
				ap := "ONLINE"
				if !f.apOnline {
					ap = "OFFLINE"
				}
				js(map[string]any{"data": []map[string]any{
					{"id": "d1", "name": "UDM", "model": "UDM-Pro-Max", "macAddress": "AA:00:00:00:00:01", "ipAddress": "192.168.1.1", "state": "ONLINE", "features": []string{"gateway"}, "firmwareVersion": "4.3.6"},
					{"id": "d2", "name": "AP Wohnzimmer", "model": "U7-Pro", "macAddress": "AA:00:00:00:00:02", "ipAddress": "192.168.1.20", "state": ap, "features": []string{"accessPoint"}},
				}, "totalCount": 2})
			case strings.HasSuffix(rest, "/statistics/latest"):
				js(map[string]any{"uptimeSec": 3600, "cpuUtilizationPct": 12.5, "memoryUtilizationPct": 40})
			case strings.HasPrefix(rest, "/proxy/network/api/s/default/stat/device/"):
				if !f.wanOK {
					w.WriteHeader(404)
					return
				}
				js(map[string]any{"data": []map[string]any{{"wan1": map[string]any{"up": true, "ip": "203.0.113.5", "enable": true}, "active_wan": "wan1"}}})
			default:
				w.WriteHeader(404)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	old := unifiCloudBase
	unifiCloudBase = ts.URL
	t.Cleanup(func() { unifiCloudBase = old })
	return f, ts
}

func TestUniFiCloudConsolesAndDiagnose(t *testing.T) {
	f, _ := newFakeUniFiCloud(t)
	if _, err := unifiConsoles("bad"); err == nil || !strings.Contains(err.Error(), "ungültig") {
		t.Fatalf("401 → Key ungültig: %v", err)
	}
	cons, err := unifiConsoles("good-key")
	if err != nil || len(cons) != 2 || cons[1].Name != "UDM Zuhause" || !cons[1].Online || cons[1].Model == "" || cons[0].Name != "Alte UDM" {
		t.Fatalf("Konsolen: %+v %v", cons, err)
	}
	steps, _ := unifiCloudDiagnose("good-key", "c1", "")
	txt, _ := json.Marshal(steps)
	if len(steps) != 6 || !steps[2].OK || !steps[3].OK || !steps[4].OK || !steps[5].Warn {
		t.Fatalf("Diagnose: %s", txt)
	}
	f.mu.Lock()
	f.wanOK = true
	f.mu.Unlock()
	if s, _ := unifiCloudDiagnose("good-key", "c1", ""); s[5].Warn || !s[5].OK {
		t.Fatalf("WAN verfügbar: %+v", s[5])
	}
	s404, _ := unifiCloudDiagnose("good-key", "c404", "")
	if last := s404[len(s404)-1]; last.OK || !strings.Contains(last.Msg, "5.0.3") {
		t.Fatalf("404 → Firmware-Hinweis: %+v", last)
	}
	f.mu.Lock()
	f.consoleUp = false
	f.mu.Unlock()
	if s, _ := unifiCloudDiagnose("good-key", "c1", ""); s[len(s)-1].OK || !strings.Contains(s[len(s)-1].Msg, "offline") {
		t.Fatalf("Konsole offline: %+v", s)
	}
	if s, _ := unifiCloudDiagnose("bad", "c1", ""); len(s) != 1 || s[0].OK {
		t.Fatalf("falscher Key: %+v", s)
	}
}

func TestUniFiCloudSiteFlow(t *testing.T) {
	f, _ := newFakeUniFiCloud(t)
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")

	// Standort ohne Agent anlegen
	code, body := c.do("POST", "/api/sites", map[string]any{"name": "Kunde Cloud", "kind": "unifi"})
	if code != 200 {
		t.Fatal(code, body)
	}
	var site SiteCfg
	json.Unmarshal([]byte(body), &site)
	if site.Kind != siteKindUniFi {
		t.Fatalf("Kind: %+v", site)
	}
	// ohne Kunden-Key
	if code, body := c.do("POST", "/api/unifi", map[string]any{"site": site.ID, "on": true, "console": "c1"}); code != 400 || !strings.Contains(body, "UniFi-Cloud-Key") {
		t.Fatal("ohne Key:", code, body)
	}
	// Konsolen laden ohne Key → Hinweis
	if code, _ := c.do("POST", "/api/unifi/consoles", map[string]any{}); code != 400 {
		t.Fatal("Konsolen ohne Key", code)
	}
	if code, _ := c.do("POST", "/api/unifi/cloud", map[string]any{"key": "good-key"}); code != 200 {
		t.Fatal("Key speichern", code)
	}
	_, body = c.do("GET", "/api/unifi/cloud", nil)
	if !strings.Contains(body, `"keySet":true`) || strings.Contains(body, "good-key") {
		t.Fatal("Key-Status:", body)
	}
	code, body = c.do("POST", "/api/unifi/consoles", map[string]any{})
	if code != 200 || !strings.Contains(body, "UDM Zuhause") {
		t.Fatal("Konsolen:", code, body)
	}
	code, body = c.do("POST", "/api/unifi/test", map[string]any{"console": "c1"})
	if code != 200 || !strings.Contains(body, "Cloud-Proxy") {
		t.Fatal("Test:", code, body)
	}
	if code, body := c.do("POST", "/api/unifi", map[string]any{"site": site.ID, "on": true, "console": "c1", "consoleName": "UDM Zuhause"}); code != 200 {
		t.Fatal(code, body)
	}
	time.Sleep(300 * time.Millisecond) // erste Abfrage läuft sofort
	sc, _ := s.m.siteByID(site.ID)
	if !sc.UniFi.isCloud() || sc.UniFi.Console != "c1" {
		t.Fatalf("Konfiguration: %+v", sc.UniFi)
	}
	// Hosts an einem Standort ohne Agent werden abgelehnt
	if code, _ := c.do("POST", "/api/hosts", map[string]any{"name": "x", "addr": "192.168.1.5", "site": site.ID}); code != 400 {
		t.Fatal("Host an Cloud-Standort", code)
	}
	// Agent eines Cloud-Standorts bekommt keine UniFi-Konfiguration
	req, _ := http.NewRequest("GET", ts.URL+"/api/agent/config", nil)
	req.Header.Set("Authorization", "Bearer "+sc.Token)
	resp, _ := http.DefaultClient.Do(req)
	var cfg map[string]any
	json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if _, has := cfg["unifi"]; has {
		t.Fatal("Agent bekommt Cloud-Konfiguration")
	}

	// Poller: gleiche Verarbeitung wie beim Agenten
	s.m.unifiCloudPollSite(sc)
	_, body = c.do("GET", "/api/unifi?site="+site.ID, nil)
	if !strings.Contains(body, `"total":2`) || !strings.Contains(body, `"mode":"cloud"`) || !strings.Contains(body, `"cpu":12.5`) {
		t.Fatal("Livestatus:", body)
	}
	_, st := c.do("GET", "/api/state", nil)
	if strings.Contains(st, "good-key") {
		t.Fatal("Key im State")
	}
	if !strings.Contains(st, `"kind":"unifi"`) {
		t.Fatal("Standort-Art fehlt im State")
	}
	_, sl := c.do("GET", "/api/sites", nil)
	if strings.Contains(sl, "good-key") {
		t.Fatal("Key in /api/sites")
	}
	on, _ := s.m.siteOnline(site.ID)
	if !on {
		t.Fatal("Cloud-Standort sollte online sein")
	}
	for len(s.m.notices) > 0 {
		<-s.m.notices
	}
	f.mu.Lock()
	f.apOnline = false
	f.mu.Unlock()
	s.m.unifiCloudPollSite(sc)
	select {
	case n := <-s.m.notices:
		if !strings.Contains(n.title, "AP Wohnzimmer") || !strings.Contains(n.body, "Geprüft von: UniFi-Cloud") {
			t.Fatalf("Meldung: %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("keine Offline-Meldung")
	}
	// Konsole weg → nach 3 Fehlversuchen Meldung „Konsole“ (Hysterese), Standort offline
	f.mu.Lock()
	f.consoleUp = false
	f.mu.Unlock()
	for i := 0; i < 3; i++ {
		s.m.unifiCloudPollSite(sc)
	}
	select {
	case n := <-s.m.notices:
		if !strings.Contains(n.title, "UniFi-Konsole") || !strings.Contains(n.body, "UniFi-Cloud") {
			t.Fatalf("Konsolen-Meldung: %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("keine Konsolen-Meldung")
	}
	if on, _ := s.m.siteOnline(site.ID); on {
		t.Fatal("Standort sollte offline sein")
	}
}

func TestUniFiCloudRateLimitAndModeSwitch(t *testing.T) {
	newFakeUniFiCloud(t)
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Mit Agent"})
	sc := s.m.cfg.Sites[0]
	// lokal mit Konsolen-Key, dann auf Cloud umstellen: lokaler Key darf nicht als Cloud-Key weiterleben
	c.do("POST", "/api/unifi", map[string]any{"site": sc.ID, "on": true, "url": "192.168.1.1", "key": "lokal-geheim"})
	c.do("POST", "/api/unifi/cloud", map[string]any{"key": "good-key"})
	if code, body := c.do("POST", "/api/unifi", map[string]any{"site": sc.ID, "on": true, "mode": "cloud", "console": "c429"}); code != 200 {
		t.Fatal(code, body)
	}
	sc, _ = s.m.siteByID(sc.ID)
	if sc.UniFi.Key != "" || s.m.unifiCloudKey(sc) != "good-key" {
		t.Fatalf("Key nach Umstellung: %q", sc.UniFi.Key)
	}
	time.Sleep(200 * time.Millisecond)
	unifiCloudPoll.Lock()
	unifiCloudPoll.next[sc.ID] = time.Time{}
	delete(unifiCloudPoll.next, "key:good-key")
	unifiCloudPoll.Unlock()
	s.m.unifiCloudPollSite(sc) // 429 mit Retry-After 120 → Pause, kein Fehler-Ereignis
	unifiCloudPoll.Lock()
	wait := time.Until(unifiCloudPoll.next[sc.ID])
	unifiCloudPoll.Unlock()
	if wait < 100*time.Second {
		t.Fatalf("keine Pause nach 429: %v", wait)
	}
	unifiLiveState.Lock()
	st := unifiLiveState.bySite[sc.ID]
	unifiLiveState.Unlock()
	if st != nil && st.Err != "" {
		t.Fatalf("429 als Fehler gewertet: %s", st.Err)
	}
	// Viewer darf keinen Key setzen
	if code, _ := c.do("POST", "/api/unifi/cloud", map[string]any{"remove": true}); code != 200 {
		t.Fatal("Key entfernen", code)
	}
	if s.m.cfg.UniFiKeys["default"] != "" {
		t.Fatal("Key nicht entfernt")
	}
}

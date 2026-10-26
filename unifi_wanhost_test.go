package main

import "testing"

func TestUniFiWanHost(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("admin", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	c.do("POST", "/api/unifi", map[string]any{"site": site.ID, "on": true, "url": "192.168.1.1", "key": "k"})
	s.m.mu.RLock()
	site = s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	gw := func(lat float64, up bool) []unifiDevice {
		return []unifiDevice{{Name: "UDM", MAC: "aa:00", State: "ONLINE", Features: []string{"gateway"}, Uptime: 100,
			WAN: []unifiWAN{{Name: "WAN", Up: up, IP: "1.2.3.4", Latency: lat}, {Name: "WAN2", Up: false}}}}
	}
	s.m.ingestUniFi(site, gw(12, true), "")
	s.m.ingestUniFi(site, gw(14, true), "")
	var hc *HostCfg
	n := 0
	s.m.mu.RLock()
	for i := range s.m.cfg.Hosts {
		if isUniFiWanHost(s.m.cfg.Hosts[i]) {
			hc = &s.m.cfg.Hosts[i]
			n++
		}
	}
	s.m.mu.RUnlock()
	if n != 1 || hc.Site != site.ID || hc.Method != methodUniFiWAN {
		t.Fatalf("erwartet 1 WAN-Host am Standort, n=%d %+v", n, hc)
	}
	h := s.m.host(hc.ID)
	h.mu.Lock()
	if len(h.samples) != 2 || h.samples[1].R != 140 {
		t.Fatalf("Messwerte: %+v", h.samples)
	}
	h.mu.Unlock()
	if _, hosts := s.m.usage(site.Org); hosts != 0 {
		t.Fatalf("WAN-Host darf nicht zählen: %d", hosts)
	}
	// Bearbeiten im Dashboard darf Methode/Quelle nicht ändern
	c.do("POST", "/api/hosts", map[string]any{"id": hc.ID, "name": "Leitung Büro", "addr": "x", "site": "cloud", "method": "icmp"})
	s.m.mu.RLock()
	for _, x := range s.m.cfg.Hosts {
		if x.ID == hc.ID && (x.Method != methodUniFiWAN || x.Site != site.ID || x.Name != "Leitung Büro") {
			t.Fatalf("nach Bearbeiten: %+v", x)
		}
	}
	s.m.mu.RUnlock()
	// gelöscht → nicht wieder anlegen
	s.m.DeleteHost(hc.ID)
	s.m.ingestUniFi(site, gw(12, true), "")
	s.m.mu.RLock()
	for _, x := range s.m.cfg.Hosts {
		if isUniFiWanHost(x) {
			t.Fatal("gelöschter WAN-Host wurde neu angelegt")
		}
	}
	s.m.mu.RUnlock()
}

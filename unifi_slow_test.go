package main

import (
	"strings"
	"testing"
	"time"
)

// 408 vom Cloud-Proxy bei verbundener Konsole: kein Ausfall, keine Meldung.
func TestUniFiCloudSlowNoAlarm(t *testing.T) {
	f, _ := newFakeUniFiCloud(t)
	resetUniFiCaches()
	s, _ := newTestServer(t)
	sc := SiteCfg{ID: "sx", Name: "Kunde", Org: "default", Kind: siteKindUniFi, UniFi: &UniFiCfg{On: true, Mode: unifiModeCloud, Console: "c1", Key: "good-key"}}
	s.m.mu.Lock()
	s.m.cfg.Sites = append(s.m.cfg.Sites, sc)
	s.m.mu.Unlock()
	s.m.unifiCloudPollSite(sc)
	if v := s.m.unifiSiteView(&sc); v.Total == 0 {
		t.Fatalf("erste Abfrage: %+v", v)
	}
	f.mu.Lock()
	f.slow = true
	f.mu.Unlock()
	for i := 0; i < 5; i++ {
		s.m.unifiCloudPollSite(sc)
	}
	v := s.m.unifiSiteView(&sc)
	if v.Err != "" || !strings.Contains(v.Note, "408") || v.Total == 0 {
		t.Fatalf("408 darf kein Ausfall sein: err=%q note=%q total=%d", v.Err, v.Note, v.Total)
	}
	if on, _ := s.m.siteOnline("sx"); !on {
		t.Fatal("Standort sollte online bleiben")
	}
	f.mu.Lock()
	f.slow = false
	f.mu.Unlock()
	s.m.unifiCloudPollSite(sc)
	if v := s.m.unifiSiteView(&sc); v.Note != "" || v.Err != "" {
		t.Fatalf("Hinweis bleibt: %+v", v.Note)
	}
}

func resetUniFiCaches() {
	unifiConsCache.Lock()
	unifiConsCache.m = map[string]struct {
		at   time.Time
		list []unifiConsole
	}{}
	unifiConsCache.Unlock()
	unifiCloudPoll.Lock()
	unifiCloudPoll.next = map[string]time.Time{}
	unifiCloudPoll.Unlock()
}

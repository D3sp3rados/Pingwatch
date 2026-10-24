package main

import (
	"strings"
	"testing"
	"time"
)

func TestBatchNotices(t *testing.T) {
	s, _ := newTestServer(t)
	m := s.m
	m.notices = make(chan notice, 100)
	m.mu.Lock()
	m.cfg.Orgs = append(m.cfg.Orgs, Org{ID: "bo", Name: "B"})
	m.cfg.Groups = append(m.cfg.Groups, Group{ID: "g1", Org: "bo", Name: "Kunde A"}, Group{ID: "g2", Org: "bo", Name: "Kunde B"})
	for i := 1; i <= 6; i++ {
		g := "g1"
		if i > 4 {
			g = "g2"
		}
		m.cfg.Hosts = append(m.cfg.Hosts, HostCfg{ID: "bh" + string(rune('0'+i)), Org: "bo", Name: "Host" + string(rune('0'+i)), Group: g})
	}
	m.mu.Unlock()
	old := batchQuietTest
	batchQuietTest = 100 * time.Millisecond
	defer func() { batchQuietTest = old }()
	for i := 1; i <= 6; i++ {
		id := "bh" + string(rune('0'+i))
		m.notify(notice{"outage", id, "🔴 Host" + string(rune('0'+i)) + " antwortet nicht", "x", "bo", ""})
	}
	// 2 einzeln
	if len(m.notices) != 2 {
		t.Fatalf("einzeln: %d", len(m.notices))
	}
	<-m.notices
	<-m.notices
	time.Sleep(300 * time.Millisecond)
	if len(m.notices) != 1 {
		t.Fatalf("sammel: %d", len(m.notices))
	}
	b := <-m.notices
	// alle
	all, ok := m.pickFor(b, func(notice) bool { return true })
	if !ok || !strings.Contains(all.title, "6 Geräte") || !strings.Contains(all.body, "Kunde A (4)") || !strings.Contains(all.body, "2 davon schon einzeln") {
		t.Fatalf("%v %+v", ok, all)
	}
	// nur Kunde B → 2 neue → Sammel mit 2
	only := func(ids ...string) func(notice) bool {
		return func(x notice) bool {
			for _, i := range ids {
				if x.hostID == i {
					return true
				}
			}
			return false
		}
	}
	x, ok := m.pickFor(b, only("bh5", "bh6"))
	if !ok || !strings.Contains(x.title, "2 Geräte") {
		t.Fatalf("B: %+v", x)
	}
	// nur einer → Einzelmeldung
	x, ok = m.pickFor(b, only("bh6"))
	if !ok || x.hostID != "bh6" {
		t.Fatalf("einzeln: %+v", x)
	}
	// nur schon gemeldete → nichts
	if _, ok = m.pickFor(b, only("bh1", "bh2")); ok {
		t.Fatal("doppelt")
	}
	// Ausfall im Topf + sofort wieder da → beides weg
	m.notify(notice{"outage", "bh1", "🔴 Host1 antwortet nicht", "x", "bo", ""})
	m.notify(notice{"recovered", "bh1", "🟢 Host1 wieder erreichbar", "x", "bo", ""})
	time.Sleep(300 * time.Millisecond)
	if len(m.notices) != 0 {
		t.Fatalf("flap: %d", len(m.notices))
	}
	// aus
	z := 0
	m.mu.Lock()
	for i := range m.cfg.Orgs {
		if m.cfg.Orgs[i].ID == "bo" {
			m.cfg.Orgs[i].BatchMin = &z
		}
	}
	m.mu.Unlock()
	for i := 0; i < 5; i++ {
		m.notify(notice{"outage", "bh3", "🔴 Host3 antwortet nicht", "x", "bo", ""})
	}
	if len(m.notices) != 5 {
		t.Fatalf("aus: %d", len(m.notices))
	}
}

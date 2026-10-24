package main

import (
	"testing"
	"time"
)

func TestLateSamplesNotNotified(t *testing.T) {
	s, _ := newTestServer(t)
	m := s.m
	hc := m.UpsertHost(HostCfg{Name: "Agent-Host", Addr: "192.168.1.5", Site: "cloud", Method: "icmp", Org: "default", OutageCount: 3})
	h := m.host(hc.ID)
	old := time.Now().Add(-10 * time.Minute).UnixMilli()
	for i := 0; i < 5; i++ {
		m.Ingest(h, old+int64(i)*10000, -1, "", "timeout")
	}
	time.Sleep(300 * time.Millisecond)
	if len(m.notices) != 0 {
		t.Fatalf("nachgeliefert gemeldet: %d", len(m.notices))
	}
	// aktueller Messwert, immer noch weg → jetzt melden
	m.Ingest(h, time.Now().UnixMilli(), -1, "", "timeout")
	time.Sleep(300 * time.Millisecond)
	if len(m.notices) != 1 {
		t.Fatalf("laufender Ausfall nicht nachgemeldet: %d", len(m.notices))
	}
	n := <-m.notices
	if n.kind != "outage" {
		t.Fatal(n)
	}
	m.Ingest(h, time.Now().UnixMilli()+1000, 5, "", "")
	time.Sleep(300 * time.Millisecond)
	if len(m.notices) != 1 || (<-m.notices).kind != "recovered" {
		t.Fatal("Entwarnung fehlt")
	}
	// Ausfall nur in der Vergangenheit, wieder da → gar nichts
	for i := 0; i < 5; i++ {
		m.Ingest(h, old+100000+int64(i)*10000, -1, "", "timeout")
	}
	m.Ingest(h, old+200000, 5, "", "")
	time.Sleep(500 * time.Millisecond)
	if len(m.notices) != 0 {
		t.Fatalf("vergangener Ausfall gemeldet: %d", len(m.notices))
	}
}

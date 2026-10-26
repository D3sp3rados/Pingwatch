package main

import (
	"testing"
	"time"
)

func TestPortSpeedDrop(t *testing.T) {
	sv, _ := newTestServer(t)
	m := sv.m
	m.mu.Lock()
	m.cfg.Sites = append(m.cfg.Sites, SiteCfg{ID: "s1", Name: "Büro", Org: "default"})
	m.mu.Unlock()
	hc := m.UpsertHost(HostCfg{Name: "FW", Addr: "10.0.0.1", Site: "s1", Org: "default", Mt: true, MtUser: "a", MtPass: "b", Watch: "1"})
	h := m.host(hc.ID)
	t0 := time.Now().Add(-5 * time.Minute).UnixMilli()
	m.ingestIfs(h, t0, []ifSample{{Idx: 1, Name: "ether1", Up: true, Speed: 1000}, {Idx: 2, Name: "ether2", Up: true, Speed: 1000}}, "")
	m.ingestIfs(h, t0+60000, []ifSample{{Idx: 1, Name: "ether1", Up: true, Speed: 100}, {Idx: 2, Name: "ether2", Up: true, Speed: 100}}, "")
	ih := getIfHost(hc.ID)
	if ih.slow[1] == nil || ih.slow[2] != nil {
		t.Fatalf("Alarm nur für überwachten Port erwartet: %v %v", ih.slow[1], ih.slow[2])
	}
	if ih.slow[1].HostName != "FW · ether1 · 1 G → 100 M" {
		t.Fatalf("Name: %q", ih.slow[1].HostName)
	}
	m.ingestIfs(h, t0+120000, []ifSample{{Idx: 1, Name: "ether1", Up: true, Speed: 1000}}, "")
	if ih.slow[1] != nil {
		t.Fatal("nicht geschlossen")
	}
}

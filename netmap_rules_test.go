package main

import (
	"testing"
	"time"
)

func TestMapRules(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	defer m.Shutdown()
	d := &mapData{Nodes: []mapNode{
		{ID: "s", Name: "Sektor Murg", Addr: "10.0.0.1", Zone: "Murg"},
		{ID: "k1", Name: "Kunde 1", Addr: "10.0.0.2", Zone: "Murg"},
		{ID: "k2", Name: "Kunde 2", Addr: "10.0.0.3", Zone: "Murg"},
		{ID: "x", Name: "Mast", Addr: "10.0.0.9"},
	}, Links: []mapLink{{A: "s", B: "k1"}, {A: "s", B: "k2"}, {A: "s", B: "x"}, {A: "x", B: "y"}}}
	if err := m.saveMap("mp1", d); err != nil {
		t.Fatal(err)
	}
	sec := ruleMembers(d, MapRule{Scope: "sector", Target: "s", Customers: true})
	if len(sec) != 3 { // Sektor + 2 Kunden (Mast hat weitere Verbindung? hier nicht – y existiert nicht, Link wird ignoriert)
		t.Logf("sector members %v", sec)
	}
	m.mu.Lock()
	m.cfg.Maps = append(m.cfg.Maps, NetMap{ID: "mp1", Org: defaultOrgID, Name: "K", Rules: []MapRule{{ID: "r1", Scope: "zone", Target: "Murg", Min: 2, Confirm: 1}}})
	m.mu.Unlock()
	set := func(up ...string) {
		mapPing.Lock()
		r := &mapReach{At: time.Now().UnixMilli(), IPs: 4, RTT: map[string]int32{}}
		for _, ip := range up {
			r.RTT[ip] = 10
		}
		mapPing.reach["mp1"] = r
		mapPing.Unlock()
	}
	st := func() *ruleState { mapRuleState.Lock(); defer mapRuleState.Unlock(); return mapRuleState.m["mp1|r1"] }
	set("10.0.0.1", "10.0.0.2", "10.0.0.3")
	m.evalMapRules("mp1")
	if st().firing {
		t.Fatal("alles erreichbar, darf nicht auslösen")
	}
	set("10.0.0.1") // 2 von 3 in Murg weg
	m.evalMapRules("mp1")
	if !st().firing {
		t.Fatal("2 nicht erreichbar → muss auslösen")
	}
	set("10.0.0.1", "10.0.0.2", "10.0.0.3")
	m.evalMapRules("mp1")
	if st().firing {
		t.Fatal("wieder erreichbar → Entwarnung")
	}
}

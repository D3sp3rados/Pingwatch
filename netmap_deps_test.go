package main

import (
	"strings"
	"testing"
)

func TestMapDeps(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	// core – sw(unüberwacht) – sektor – k1, k2 ; core – r2 – k3
	nodes := []mapNode{{ID: "core", Name: "Core", Addr: "10.8.0.1"}, {ID: "sw", Name: "Switch", Addr: "10.8.0.9"}, {ID: "sek", Name: "Sektor", Addr: "10.8.0.2"},
		{ID: "k1", Name: "K1", Addr: "10.8.1.1"}, {ID: "k2", Name: "K2", Addr: "10.8.1.2"}, {ID: "r2", Name: "R2", Addr: "10.8.0.3"}, {ID: "k3", Name: "K3", Addr: "10.8.1.3"}}
	links := []mapLink{{A: "core", B: "sw"}, {A: "sw", B: "sek"}, {A: "sek", B: "k1"}, {A: "sek", B: "k2"}, {A: "core", B: "r2"}, {A: "r2", B: "k3"}}
	code, body := c.do("POST", "/api/maps", map[string]any{"name": "D", "nodes": nodes, "links": links})
	if code != 200 {
		t.Fatal(code, body)
	}
	var mp struct{ ID string }
	mustJSON(t, body, &mp)
	ids := map[string]string{}
	for _, n := range nodes {
		if n.ID == "sw" {
			continue
		}
		hc := s.m.UpsertHost(HostCfg{Name: n.Name, Addr: n.Addr, Site: "cloud", Method: "icmp", Org: "default"})
		ids[n.Name] = hc.ID
	}
	code, body = c.do("GET", "/api/maps/deps?id="+mp.ID, nil)
	if code != 200 {
		t.Fatal(code, body)
	}
	var r struct {
		RootName string
		Items    []depItem
	}
	mustJSON(t, body, &r)
	want := map[string]string{"Sektor": "Core", "K1": "Sektor", "K2": "Sektor", "R2": "Core", "K3": "R2"}
	if r.RootName != "Core" || len(r.Items) != len(want) {
		t.Fatal(body)
	}
	var set []map[string]string
	for _, it := range r.Items {
		if want[it.HostName] != it.ParentName {
			t.Fatal(it)
		}
		set = append(set, map[string]string{"host": it.Host, "parent": it.Parent})
	}
	code, body = c.do("POST", "/api/maps/deps", map[string]any{"set": set})
	if code != 200 || !strings.Contains(body, `"set":5`) {
		t.Fatal(code, body)
	}
	if s.m.host(ids["K1"]).cfg.Parent != ids["Sektor"] {
		t.Fatal("parent nicht gesetzt")
	}
	_, body = c.do("GET", "/api/maps/deps?id="+mp.ID, nil)
	if !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"same":5`) {
		t.Fatal(body)
	}
}

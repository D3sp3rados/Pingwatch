package main

import (
	"strings"
	"testing"
	"time"
)

func TestMapStats(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	defer m.Shutdown()
	ips := []string{"10.0.0.1", "10.0.0.2"}
	t0 := time.Now().Truncate(time.Hour)
	m.recordMapReach("mp", ips, map[string]int32{"10.0.0.1": 20, "10.0.0.2": 40}, t0)
	m.recordMapReach("mp", ips, map[string]int32{"10.0.0.1": 40}, t0.Add(5*time.Minute))
	m.recordMapReach("mp", ips, map[string]int32{"10.0.0.1": 30, "10.0.0.2": 10}, t0.Add(10*time.Minute))
	m.recordMapSNMP("mp", map[string][]ifSample{"10.0.0.1": {{Name: "ether1", In: 1000, Out: 0, HC: true}}}, t0)
	m.recordMapSNMP("mp", map[string][]ifSample{"10.0.0.1": {{Name: "ether1", In: 1000 + 75_000_000, Out: 7_500_000, HC: true}}}, t0.Add(5*time.Minute))
	m.saveMapStats()
	mapStats.Lock()
	mapStats.days = map[string]*mapDay{}
	d := m.mapDayLocked("mp", t0.Format("2006-01-02"))
	mapStats.Unlock()
	a, b := d.IPs["10.0.0.1"], d.IPs["10.0.0.2"]
	h := t0.Hour()
	if a.H[h][0] != 3 || a.H[h][1] != 0 || a.H[h][2] != 90 || a.H[h][3] != 40 {
		t.Fatalf("ip1 %v", a.H[h])
	}
	if b.H[h][1] != 1 || len(b.Out) != 1 || b.Out[0][1] == 0 {
		t.Fatalf("ip2 %v %v", b.H[h], b.Out)
	}
	ih := a.If["ether1"]
	if ih == nil || ih.H[h][0] != 1 || int(ih.H[h][1]) != 2_000_000 || int(ih.H[h][2]) != 200_000 {
		t.Fatalf("traffic %+v", ih)
	}
}

func TestMapStatsAPI(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	code, body := c.do("POST", "/api/maps", map[string]any{"name": "K", "nodes": []mapNode{{ID: "s", Name: "Sektor", Addr: "10.9.0.1", Zone: "Murg"}, {ID: "k", Name: "Kunde", Addr: "10.9.0.2", Zone: "Murg"}, {ID: "x", Name: "X", Addr: "10.9.0.3"}}, "links": []mapLink{{A: "s", B: "k"}, {A: "s", B: "x"}}})
	if code != 200 {
		t.Fatal(code, body)
	}
	var mp struct{ ID string }
	mustJSON(t, body, &mp)
	if mp.ID == "" {
		var x struct{ Map NetMap }
		mustJSON(t, body, &x)
		mp.ID = x.Map.ID
	}
	now := time.Now()
	s.m.recordMapReach(mp.ID, []string{"10.9.0.1", "10.9.0.2", "10.9.0.3"}, map[string]int32{"10.9.0.1": 12, "10.9.0.3": 5}, now)
	for _, q := range []string{"&node=s", "&node=k", "&zone=Murg", "&sector=s", ""} {
		code, body = c.do("GET", "/api/maps/stats?id="+mp.ID+"&days=7"+q, nil)
		if code != 200 {
			t.Fatal(q, code, body)
		}
		t.Log(q, body[:min(len(body), 300)])
	}
	code, body = c.do("POST", "/api/maps", map[string]any{"id": mp.ID, "snmpComm": "public,rst", "snmpScope": "all"})
	if code != 200 {
		t.Fatal(code, body)
	}
	_, body = c.do("GET", "/api/maps", nil)
	if !strings.Contains(body, `"snmpComm":"public,rst"`) || !strings.Contains(body, `"snmpScope":"all"`) {
		t.Fatal(body)
	}
}

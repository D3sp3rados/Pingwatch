package main

import (
	"strings"
	"testing"
	"time"
)

func TestMaintWindows(t *testing.T) {
	loc := time.Local
	// Dienstag 20.10.2026
	tue := func(h, m int) time.Time { return time.Date(2026, 10, 20, h, m, 0, 0, loc) }
	w := Maint{Weekdays: []int{2}, From: "22:00", To: "02:00"}
	if !w.activeAt(tue(23, 0)) || w.activeAt(tue(21, 59)) {
		t.Fatal("Dienstagabend")
	}
	if !w.activeAt(tue(23, 0).Add(2*time.Hour)) || w.activeAt(tue(23, 0).Add(3*time.Hour+5*time.Minute)) {
		t.Fatal("über Mitternacht")
	}
	w2 := Maint{Weekdays: []int{1, 2}, From: "08:00", To: "09:00"}
	if !w2.activeAt(tue(8, 30)) || w2.activeAt(tue(9, 0)) {
		t.Fatal("Vormittag")
	}
	one := Maint{Start: tue(10, 0).UnixMilli(), End: tue(11, 0).UnixMilli()}
	if !one.activeAt(tue(10, 30)) || one.activeAt(tue(11, 0)) {
		t.Fatal("einmalig")
	}
}

func TestDependencyAndMaint(t *testing.T) {
	s, ts := newTestServer(t)
	parentGrace = 0
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	r := s.m.UpsertHost(HostCfg{Name: "Router", Addr: "10.0.0.1", Site: "cloud", Org: "default", IntervalMs: 1000})
	k := s.m.UpsertHost(HostCfg{Name: "Kamera", Addr: "10.0.0.2", Site: "cloud", Org: "default", IntervalMs: 1000})
	if err := s.m.validParent("default", r.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	k.Parent = r.ID
	code, body := c.do("POST", "/api/hosts", k)
	if code != 200 {
		t.Fatal(code, body)
	}
	r.Parent = k.ID
	if code, body := c.do("POST", "/api/hosts", r); code != 400 || !strings.Contains(body, "Schleife") {
		t.Fatalf("Schleife nicht erkannt: %d %s", code, body)
	}
	// Router ausgefallen → Kamera-Ausfall wird nicht gemeldet
	rh, kh := s.m.host(r.ID), s.m.host(k.ID)
	rh.mu.Lock()
	rh.oOut = &Event{ID: 1}
	rh.mu.Unlock()
	e := &Event{ID: 2, HostID: k.ID, Org: "default", Type: "outage", Start: time.Now().UnixMilli()}
	s.m.eventStarted(e, kh)
	if e.Cause != "Router" || e.Notified {
		t.Fatalf("Ursache: %+v", e)
	}
	// Wartungsfenster für alle → still
	code, body = c.do("POST", "/api/maint", map[string]any{"name": "Update", "start": time.Now().Add(-time.Minute).UnixMilli(), "end": time.Now().Add(time.Hour).UnixMilli()})
	if code != 200 {
		t.Fatal(code, body)
	}
	e2 := &Event{ID: 3, HostID: r.ID, Org: "default", Type: "outage", Start: time.Now().UnixMilli()}
	s.m.eventStarted(e2, rh)
	if !e2.Maint || e2.Notified {
		t.Fatalf("Wartung: %+v", e2)
	}
	_, body = c.do("GET", "/api/state", nil)
	if !strings.Contains(body, `"maint":true`) {
		t.Fatal("Host nicht als Wartung markiert")
	}
	// Traceroute (Simulation, Cloud-Host)
	code, body = c.do("POST", "/api/trace", map[string]string{"host": r.ID})
	if code != 200 || !strings.Contains(body, "Routenverfolgung") {
		t.Fatalf("trace: %d %s", code, body)
	}
}

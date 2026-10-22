package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAckAndEscalation(t *testing.T) {
	s, ts := newTestServer(t)
	escalateUnit = 20 * time.Millisecond
	defer func() { escalateUnit = time.Minute }()
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+" "+string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	h1 := s.m.UpsertHost(HostCfg{Name: "Router", Addr: "10.0.0.1", Site: "cloud", Org: "default", IntervalMs: 1000})
	h2 := s.m.UpsertHost(HostCfg{Name: "NAS", Addr: "10.0.0.2", Site: "cloud", Org: "default", IntervalMs: 1000})
	s.m.mu.Lock()
	s.m.cfg.Channels = append(s.m.cfg.Channels, Channel{ID: "chef", Org: "default", Type: "webhook", Target: srv.URL + "/chef", Outage: true, Escalate: 1})
	s.m.mu.Unlock()
	open := func(h HostCfg, id int64) *Event {
		e := &Event{ID: id, HostID: h.ID, Org: "default", Type: "outage", Start: time.Now().UnixMilli()}
		hh := s.m.host(h.ID)
		hh.mu.Lock()
		hh.oOut = e
		hh.mu.Unlock()
		s.m.evMu.Lock()
		s.m.events = append(s.m.events, e)
		s.m.evMu.Unlock()
		return e
	}
	open(h1, 501)
	open(h2, 502)
	// NAS wird quittiert → keine Eskalation
	code, body := c.do("POST", "/api/events/ack", map[string]any{"id": 502, "note": "Techniker unterwegs"})
	if code != 200 || !strings.Contains(body, "Techniker") {
		t.Fatal(code, body)
	}
	s.m.dispatchAlerts(notice{"outage", h1.ID, "Router aus", "x", "default", ""})
	s.m.dispatchAlerts(notice{"outage", h2.ID, "NAS aus", "x", "default", ""})
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := len(got)
	joined := strings.Join(got, "\n")
	mu.Unlock()
	if n != 1 || !strings.Contains(joined, "Router") || !strings.Contains(joined, "nicht quittiert") {
		t.Fatalf("Eskalation: %s", joined)
	}
	s.m.dispatchAlerts(notice{"recovered", h1.ID, "Router wieder da", "x", "default", ""})
	s.m.dispatchAlerts(notice{"recovered", h2.ID, "NAS wieder da", "x", "default", ""})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !strings.Contains(got[1], "Router wieder da") {
		t.Fatalf("Entwarnung nur an eskalierte: %v", got)
	}
}

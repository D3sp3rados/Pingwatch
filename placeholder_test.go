package main

import "testing"

func TestHostWithoutAddrStaysPaused(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	hc := m.UpsertHost(HostCfg{Name: "Crossborder", Site: "cloud", Method: "http"})
	if !hc.Paused {
		t.Fatal("ohne Adresse muss pausiert sein")
	}
	hc.Paused = false
	if hc = m.UpsertHost(hc); !hc.Paused {
		t.Fatal("ohne Adresse darf nicht fortgesetzt werden")
	}
	hc.Addr, hc.Paused = "https://example.org", false
	if hc = m.UpsertHost(hc); hc.Paused {
		t.Fatal("mit Adresse muss fortsetzen gehen")
	}
	m.Shutdown()
}

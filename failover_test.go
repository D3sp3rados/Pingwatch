package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestAgentFailover(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	site := SiteCfg{ID: "s1", Name: "Büro"}
	req := func(mid, os string) bool {
		r := httptest.NewRequest("GET", "/api/agent/config", nil)
		r.Header.Set("X-Machine", mid)
		r.Header.Set("X-OS", os)
		r.Header.Set("X-Host", mid)
		return m.agentStandby(site, r)
	}
	if req("pc", "windows/amd64") {
		t.Fatal("einziger Agent muss aktiv sein")
	}
	// Pi kommt dazu → übernimmt automatisch (Linux bevorzugt), PC wird Reserve
	if req("pi", "linux/arm64") {
		t.Fatal("Pi sollte übernehmen")
	}
	if !req("pc", "windows/amd64") {
		t.Fatal("PC sollte Reserve sein")
	}
	// Pi fällt aus → PC übernimmt
	st := m.sites["s1"]
	st.agents["pi"].LastSeen = time.Now().UnixMilli() - agentAliveMs - 1000
	if req("pc", "windows/amd64") {
		t.Fatal("PC sollte nach Ausfall übernehmen")
	}
	// Pi wieder da → übernimmt wieder
	if req("pi", "linux/arm64") || !req("pc", "windows/amd64") {
		t.Fatal("Pi sollte wieder Haupt-Agent sein")
	}
	// fester Haupt-Agent PC
	site.PrimaryAgent = "pc"
	if req("pc", "windows/amd64") || !req("pi", "linux/arm64") {
		t.Fatal("festgelegter Haupt-Agent PC")
	}
	// ältere Agenten ohne Kennung bleiben aktiv
	r := httptest.NewRequest("GET", "/", nil)
	if m.agentStandby(site, r) {
		t.Fatal("ohne X-Machine immer aktiv")
	}
	if ag := m.siteAgents(site); len(ag) != 2 || !ag[0].Active || ag[0].ID != "pc" || !ag[0].Primary {
		t.Fatalf("Liste: %+v", ag)
	}
}

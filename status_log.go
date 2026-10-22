package main

import (
	"log"
	"sync"
	"time"
)

// agentLogDue: Agenten-Abfragen nur einmal pro Stunde und Adresse protokollieren,
// damit wichtige Meldungen nicht aus dem (begrenzten) Container-Log verdrängt werden.
var agentLog = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

func agentLogDue(ip string) bool {
	agentLog.Lock()
	defer agentLog.Unlock()
	if time.Since(agentLog.m[ip]) < time.Hour {
		return false
	}
	agentLog.m[ip] = time.Now()
	return true
}

// statusLogLoop schreibt stündlich eine Zustandszeile ins Log.
func (m *Monitor) statusLogLoop() {
	m.logStatus()
	t := time.NewTicker(time.Hour)
	for range t.C {
		m.logStatus()
	}
}

func (m *Monitor) logStatus() {
	push := "aktiv"
	if m.pusher == nil {
		push = "aus (" + m.pushErr + ")"
	}
	m.mu.RLock()
	hosts, sites, devs := len(m.cfg.Hosts), len(m.cfg.Sites), len(m.cfg.Devices)
	m.mu.RUnlock()
	log.Printf("Status: Speicher %s, %d Hosts, %d Standorte, %d Push-Geräte, Apple-Push %s", m.store.Kind(), hosts, sites, devs, push)
}

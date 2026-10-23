package main

import (
	"testing"
	"time"
)

func TestSysStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m := NewMonitor(NewFileStore(dir))
	m.mu.Lock()
	m.cfg.Machines = append(m.cfg.Machines, MachineCfg{ID: "s1:pc-persist", Name: "pc"})
	m.mu.Unlock()
	now := time.Now().UnixMilli()
	sm := getSysMachine("s1:pc-persist")
	sm.mu.Lock()
	sm.at, sm.seen = now, now
	sm.cur = &sysReport{Host: "PC1", OS: "Windows 11", CPU: 12}
	sm.hist = []sysPoint{{T: now, C: 12, M: 40, D: 50}}
	sm.mu.Unlock()
	m.saveSysState()
	dropSysMachine("s1:pc-persist")

	m.loadSysState()
	sm2 := getSysMachine("s1:pc-persist")
	if sm2.cur == nil || sm2.cur.OS != "Windows 11" || len(sm2.hist) != 1 || sm2.seen != now {
		t.Fatalf("nicht wiederhergestellt: %+v", sm2)
	}
}

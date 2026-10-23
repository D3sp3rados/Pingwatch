package main

import (
	"encoding/json"
	"sync"
	"time"
)

// Zustand der Rechner (letzte Messung, Verlauf) überlebt Neustarts/Deploys:
// alle 5 Minuten und beim Beenden je Rechner als Blob gesichert, beim Start geladen.

type sysSnap struct {
	At    int64      `json:"at"`
	Seen  int64      `json:"seen"`
	Cur   *sysReport `json:"cur,omitempty"`
	Hist  []sysPoint `json:"hist,omitempty"`
	Hist7 []sysPoint `json:"hist7,omitempty"`
}

func sysKey(id string) string { return "sys:" + id }

var sysSaved = map[string]int64{} // Rechner → zuletzt gesicherte Messung (nur housekeeping/Shutdown)

func (m *Monitor) loadSysState() {
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.cfg.Machines))
	for _, mc := range m.cfg.Machines {
		ids = append(ids, mc.ID)
	}
	m.mu.RUnlock()
	cut := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	for _, id := range ids {
		b, err := bs.LoadBlob(sysKey(id))
		if err != nil || len(b) == 0 {
			continue
		}
		var s sysSnap
		if json.Unmarshal(b, &s) != nil || s.At < cut {
			continue
		}
		sm := getSysMachine(id)
		sm.mu.Lock()
		if sm.at == 0 {
			sm.at, sm.seen, sm.cur, sm.hist, sm.hist7 = s.At, s.Seen, s.Cur, s.Hist, s.Hist7
		}
		sm.mu.Unlock()
		sysSaved[id] = s.At
	}
}

var sysSaveMu sync.Mutex

func (m *Monitor) saveSysState() {
	sysSaveMu.Lock()
	defer sysSaveMu.Unlock()
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	sysData.Lock()
	all := make(map[string]*sysMachine, len(sysData.m))
	for id, sm := range sysData.m {
		all[id] = sm
	}
	sysData.Unlock()
	for id, sm := range all {
		sm.mu.Lock()
		if sm.at == 0 || sysSaved[id] == sm.at {
			sm.mu.Unlock()
			continue
		}
		s := sysSnap{At: sm.at, Seen: sm.seen, Cur: sm.cur,
			Hist: append([]sysPoint(nil), sm.hist...), Hist7: append([]sysPoint(nil), sm.hist7...)}
		sm.mu.Unlock()
		b, err := json.Marshal(s)
		if err != nil {
			continue
		}
		if bs.SaveBlob(sysKey(id), b) == nil {
			sysSaved[id] = s.At
		}
	}
}

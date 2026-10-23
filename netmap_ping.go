package main

import (
	"encoding/json"
	"net"
	"strings"
	"sync"
	"time"
)

// Erreichbarkeit der Geräte auf einer Netzkarte: Der Agent des eingestellten Standorts pingt alle
// Adressen der Karte regelmäßig (Auftrag „ping“, ab Agent 1.6.4). Das ist nur Anzeige (grün/rot),
// keine Überwachung mit Meldungen – dafür das Gerät als Host anlegen.

const mapPingEvery = 5 * time.Minute

type mapReach struct {
	At   int64            `json:"at"`
	RTT  map[string]int32 `json:"rtt"` // erreichbare IPs → Zehntel-ms; fehlende = nicht erreichbar
	IPs  int              `json:"ips"` // geprüfte Adressen
	Err  string           `json:"err,omitempty"`
	Wait bool             `json:"wait,omitempty"` // Auftrag läuft
}

var mapPing = struct {
	sync.Mutex
	last  map[string]time.Time // Karte → letzter Auftrag
	jobs  map[string]string    // Auftrag → Karte
	reach map[string]*mapReach // Karte → Ergebnis
	ips   map[string]int       // Auftrag → Anzahl Adressen
}{last: map[string]time.Time{}, jobs: map[string]string{}, reach: map[string]*mapReach{}, ips: map[string]int{}}

// mapPingTick: fällige Karten an den Agenten geben (aus housekeeping, jede Minute).
func (m *Monitor) mapPingTick() {
	m.mu.RLock()
	maps := append([]NetMap(nil), m.cfg.Maps...)
	m.mu.RUnlock()
	for _, mp := range maps {
		if mp.PingSite == "" {
			continue
		}
		mapPing.Lock()
		due := time.Since(mapPing.last[mp.ID]) >= mapPingEvery
		mapPing.Unlock()
		if due {
			m.queueMapPing(mp)
		}
	}
}

func (m *Monitor) queueMapPing(mp NetMap) {
	mapPing.Lock()
	mapPing.last[mp.ID] = time.Now()
	mapPing.Unlock()
	setErr := func(e string) {
		mapPing.Lock()
		r := mapPing.reach[mp.ID]
		if r == nil {
			r = &mapReach{RTT: map[string]int32{}}
			mapPing.reach[mp.ID] = r
		}
		r.Err, r.Wait = e, false
		mapPing.Unlock()
	}
	sc, ok := m.siteByID(mp.PingSite)
	if !ok || sc.Org != mp.Org {
		setErr("Standort für die Prüfung nicht gefunden")
		return
	}
	on, st := m.siteOnline(sc.ID)
	if !on {
		setErr("Agent „" + sc.Name + "“ ist offline")
		return
	}
	if st != nil && newerVersion("1.6.4", st.version) {
		setErr("Agent „" + sc.Name + "“ braucht Version 1.6.4 (aktualisiert sich automatisch)")
		return
	}
	d, err := m.loadMap(mp.ID)
	if err != nil {
		setErr(err.Error())
		return
	}
	seen := map[string]bool{}
	var ips []string
	for _, n := range d.Nodes {
		ip := net.ParseIP(strings.TrimSpace(n.Addr)).To4()
		if ip == nil || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		ips = append(ips, ip.String())
	}
	if len(ips) == 0 {
		return
	}
	for i := 0; i < len(ips); i += 1500 { // in Paketen
		end := min(i+1500, len(ips))
		j := agentJob{ID: randHex(8), Kind: "ping", Params: map[string]string{"ips": strings.Join(ips[i:end], ",")}}
		jobs.Lock()
		jobs.byID[j.ID] = &jobState{agentJob: j, Org: mp.Org, Site: sc.ID, Status: "queued", Created: time.Now().UnixMilli()}
		jobs.queue[sc.ID] = append(jobs.queue[sc.ID], j)
		jobs.Unlock()
		mapPing.Lock()
		mapPing.jobs[j.ID] = mp.ID
		mapPing.ips[j.ID] = end - i
		if r := mapPing.reach[mp.ID]; r != nil {
			r.Wait = true
		} else {
			mapPing.reach[mp.ID] = &mapReach{RTT: map[string]int32{}, Wait: true}
		}
		mapPing.Unlock()
	}
}

// mapPingResult: Ergebnis eines Ping-Auftrags übernehmen (aus handleAgentJob).
func (m *Monitor) mapPingResult(jobID, errText string, result json.RawMessage) {
	mapPing.Lock()
	done := ""
	defer func() {
		mapPing.Unlock()
		if done != "" {
			go m.evalMapRules(done)
		}
	}()
	mid, ok := mapPing.jobs[jobID]
	if !ok {
		return
	}
	n := mapPing.ips[jobID]
	delete(mapPing.jobs, jobID)
	delete(mapPing.ips, jobID)
	r := mapPing.reach[mid]
	if r == nil {
		r = &mapReach{RTT: map[string]int32{}}
		mapPing.reach[mid] = r
	}
	busy := false
	for _, m := range mapPing.jobs {
		if m == mid {
			busy = true
		}
	}
	if errText != "" {
		r.Err, r.Wait = errText, busy
		return
	}
	var sr scanResult
	if json.Unmarshal(result, &sr) != nil {
		return
	}
	// neues Ergebnis: bei mehreren Paketen sammeln, beim ersten Paket einer Runde zurücksetzen
	if !r.Wait || r.At < time.Now().Add(-mapPingEvery/2).UnixMilli() {
		r.RTT, r.IPs = map[string]int32{}, 0
	}
	for _, h := range sr.Hits {
		r.RTT[h.IP] = h.RTT
	}
	r.IPs += n
	r.At, r.Err, r.Wait = time.Now().UnixMilli(), "", busy
	if !busy {
		done = mid
	}
}

func mapReachView(id string) *mapReach {
	mapPing.Lock()
	defer mapPing.Unlock()
	r := mapPing.reach[id]
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

func (m *Monitor) siteByIDLocked(id string) (SiteCfg, bool) {
	for _, sc := range m.cfg.Sites {
		if sc.ID == id {
			return sc, true
		}
	}
	return SiteCfg{}, false
}

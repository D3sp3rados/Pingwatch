package main

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Mehrere Agenten an einem Standort (z. B. Raspberry Pi + Büro-PC als Reserve):
// Genau einer ist aktiv und bekommt Hosts, UniFi, Sicherungen und Aufträge; die anderen
// stehen in Bereitschaft und übernehmen automatisch, sobald der aktive nicht mehr meldet.
// Den RADIUS-Server betreiben alle (zweiter RADIUS-Server in den Geräten = Ausfallsicherheit).

const agentAliveMs = 70_000 // aktiver Agent gilt nach 70 s ohne Meldung als ausgefallen

type agentMachine struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	OS       string `json:"os"`
	IP       string `json:"ip"`
	LAN      string `json:"lan,omitempty"`
	Version  string `json:"version"`
	LastSeen int64  `json:"lastSeen"`
	Active   bool   `json:"active"`
	Primary  bool   `json:"primary"`
}

// agentRank: ohne festgelegten Haupt-Agenten wird ein Linux-Gerät (Raspberry Pi) bevorzugt.
func agentRank(a *agentMachine) int {
	if strings.HasPrefix(a.OS, "linux") {
		return 1
	}
	return 0
}

// agentStandby meldet den Agenten am Standort an und sagt, ob er in Bereitschaft bleiben soll.
func (m *Monitor) agentStandby(site SiteCfg, r *http.Request) bool {
	mid := r.Header.Get("X-Machine")
	if mid == "" {
		return false // ältere Agenten ohne Kennung: wie bisher immer aktiv
	}
	m.mu.Lock()
	st := m.sites[site.ID]
	if st == nil {
		st = &siteState{}
		m.sites[site.ID] = st
	}
	m.mu.Unlock()
	now := time.Now().UnixMilli()
	st.mu.Lock()
	if st.agents == nil {
		st.agents = map[string]*agentMachine{}
	}
	am := st.agents[mid]
	if am == nil {
		am = &agentMachine{ID: mid}
		st.agents[mid] = am
	}
	am.LastSeen, am.IP, am.Version = now, clientIP(r), r.Header.Get("X-Agent-Version")
	if h := r.Header.Get("X-Host"); h != "" {
		am.Host = h
	}
	if l := r.Header.Get("X-LAN"); l != "" {
		am.LAN = l
	}
	if o := r.Header.Get("X-OS"); o != "" {
		am.OS = o
	}
	alive := func(id string) bool {
		a := st.agents[id]
		return a != nil && now-a.LastSeen < agentAliveMs
	}
	prev := st.active
	switch {
	case site.PrimaryAgent != "" && alive(site.PrimaryAgent):
		st.active = site.PrimaryAgent
	case alive(st.active):
		// ohne Festlegung: Linux-Gerät (Pi) übernimmt von einem Windows-PC
		if site.PrimaryAgent == "" && agentRank(am) > agentRank(st.agents[st.active]) {
			st.active = mid
		}
	default:
		st.active = mid
	}
	for id, a := range st.agents {
		if now-a.LastSeen > 7*24*3600_000 && id != st.active {
			delete(st.agents, id)
		}
	}
	cur := st.agents[st.active]
	changed := prev != "" && prev != st.active
	var from string
	if changed && st.agents[prev] != nil {
		from = agentLabel(st.agents[prev])
	}
	to := agentLabel(cur)
	prevAlive := alive(prev)
	standby := st.active != mid
	st.mu.Unlock()
	if changed {
		why := "Haupt-Agent ist wieder da"
		if from != "" && !prevAlive {
			why = from + " meldet sich nicht mehr"
		}
		log.Printf("Standort %s: Agent-Wechsel → %s (%s)", site.Name, to, why)
		m.closeSiteEvent(m.newSiteEvent(site, "action", "Agent-Wechsel: "+to+" übernimmt ("+why+")", now, true), now)
	}
	return standby
}

// agentTouch: Lebenszeichen über die Messwert-Übertragung (häufiger als die Konfig-Abfrage).
func (m *Monitor) agentTouch(siteID, mid string) {
	if mid == "" {
		return
	}
	m.mu.RLock()
	st := m.sites[siteID]
	m.mu.RUnlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	if a := st.agents[mid]; a != nil {
		a.LastSeen = time.Now().UnixMilli()
	}
	st.mu.Unlock()
}

func agentLabel(a *agentMachine) string {
	if a == nil {
		return "?"
	}
	if a.Host != "" {
		return a.Host
	}
	return "Agent " + a.ID[:min(6, len(a.ID))]
}

// siteAgents: Agenten eines Standorts für die Oberfläche (aktiver zuerst).
func (m *Monitor) siteAgents(site SiteCfg) []agentMachine {
	m.mu.RLock()
	st := m.sites[site.ID]
	m.mu.RUnlock()
	if st == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []agentMachine
	for _, a := range st.agents {
		c := *a
		c.Active = a.ID == st.active && now-a.LastSeen < agentAliveMs
		c.Primary = a.ID == site.PrimaryAgent
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].LastSeen > out[j].LastSeen
	})
	return out
}

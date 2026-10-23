package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Meldungen auf der Netzkarte: einzelne Geräte, Sektoren (optional mit angeschlossenen Kunden),
// Orte oder die ganze Karte. Ausgelöst, sobald mindestens Min Geräte im Bereich nicht erreichbar
// sind (nach Confirm Prüfungen in Folge); Entwarnung, wenn es wieder weniger sind.
// Grundlage ist die Erreichbarkeitsprüfung der Karte (Agent, alle 5 Minuten).

type MapRule struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scope     string   `json:"scope"` // node, sector, zone, all
	Target    string   `json:"target,omitempty"`
	Customers bool     `json:"customers,omitempty"` // Sektor: angeschlossene Kunden (Endgeräte) mitzählen
	Min       int      `json:"min"`                 // ab so vielen nicht erreichbaren Geräten
	Confirm   int      `json:"confirm"`             // Prüfungen in Folge (1–3)
	Channels  []string `json:"channels,omitempty"`  // Alarmwege des Kontos
	Push      bool     `json:"push,omitempty"`      // zusätzlich Push an die Apps/Browser des Kontos
	Disabled  bool     `json:"disabled,omitempty"`
}

type ruleState struct {
	streak  int
	firing  bool
	since   int64
	checked bool // erste Auswertung nach dem Start (nur Ausgangslage merken)
}

var mapRuleState = struct {
	sync.Mutex
	m map[string]*ruleState // Karte|Regel
}{m: map[string]*ruleState{}}

// ruleMembers: Knoten-IDs im Bereich einer Regel.
func ruleMembers(d *mapData, r MapRule) []int {
	idx := map[string]int{}
	for i, n := range d.Nodes {
		idx[n.ID] = i
	}
	var out []int
	switch r.Scope {
	case "node":
		if i, ok := idx[r.Target]; ok {
			out = append(out, i)
		}
	case "sector":
		i, ok := idx[r.Target]
		if !ok {
			return nil
		}
		out = append(out, i)
		if r.Customers {
			deg := map[string]int{}
			for _, l := range d.Links {
				deg[l.A]++
				deg[l.B]++
			}
			for _, l := range d.Links {
				o := ""
				if l.A == r.Target {
					o = l.B
				} else if l.B == r.Target {
					o = l.A
				}
				if o != "" && deg[o] == 1 { // Endgerät, hängt nur an diesem Sektor
					out = append(out, idx[o])
				}
			}
		}
	case "zone":
		for i, n := range d.Nodes {
			if n.Zone == r.Target {
				out = append(out, i)
			}
		}
	case "all":
		for i := range d.Nodes {
			out = append(out, i)
		}
	}
	// Geräte mit „Ping aus“ zählen nicht mit
	k := 0
	for _, i := range out {
		if !d.Nodes[i].Off {
			out[k] = i
			k++
		}
	}
	return out[:k]
}

// evalMapRules: nach einer vollständigen Erreichbarkeitsrunde.
func (m *Monitor) evalMapRules(mapID string) {
	m.mu.RLock()
	var mp NetMap
	found := false
	for _, x := range m.cfg.Maps {
		if x.ID == mapID {
			mp, found = x, true
		}
	}
	m.mu.RUnlock()
	if !found || len(mp.Rules) == 0 {
		return
	}
	rc := mapReachView(mapID)
	if rc == nil || rc.At == 0 || rc.IPs == 0 {
		return
	}
	d, err := m.loadMap(mapID)
	if err != nil {
		return
	}
	for _, r := range mp.Rules {
		if r.Disabled {
			continue
		}
		mem := ruleMembers(d, r)
		var down []string
		total := 0
		for _, i := range mem {
			n := d.Nodes[i]
			ip := net.ParseIP(strings.TrimSpace(n.Addr)).To4()
			if ip == nil {
				continue
			}
			total++
			if _, ok := rc.RTT[ip.String()]; !ok {
				down = append(down, n.Name)
			}
		}
		min := r.Min
		if min < 1 {
			min = 1
		}
		conf := r.Confirm
		if conf < 1 || conf > 3 {
			conf = 1
		}
		bad := total > 0 && len(down) >= min
		k := mapID + "|" + r.ID
		mapRuleState.Lock()
		st := mapRuleState.m[k]
		if st == nil {
			st = &ruleState{}
			mapRuleState.m[k] = st
		}
		first := !st.checked
		st.checked = true
		var send *notice
		label := r.Name
		if label == "" {
			label = r.Target
		}
		switch {
		case bad && first:
			st.firing, st.streak, st.since = true, conf, time.Now().UnixMilli() // Ausgangslage, keine Meldung
		case bad && !st.firing:
			st.streak++
			if st.streak >= conf {
				st.firing, st.since = true, time.Now().UnixMilli()
				title := fmt.Sprintf("🔴 %s: %d von %d Geräten nicht erreichbar", label, len(down), total)
				if total == 1 {
					title = "🔴 " + label + " nicht erreichbar"
				}
				send = &notice{"mapdown", "", title, mapList(down) + "\nNetzkarte „" + mp.Name + "“", mp.Org, ""}
			}
		case !bad:
			st.streak = 0
			if st.firing && !first {
				dur := fmtDurShort(time.Now().UnixMilli() - st.since)
				body := "Störung dauerte " + dur
				if len(down) > 0 {
					body += fmt.Sprintf(" · noch %d nicht erreichbar:\n%s", len(down), mapList(down))
				}
				title := "🟢 " + label + " wieder erreichbar"
				send = &notice{"mapup", "", title, body + "\nNetzkarte „" + mp.Name + "“", mp.Org, ""}
			}
			st.firing = false
		}
		mapRuleState.Unlock()
		if send != nil {
			m.deliverDirect(*send, r.Channels, r.Push)
		}
	}
}

func mapList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	n := len(names)
	if n > 15 {
		names = names[:15]
	}
	s := "· " + strings.Join(names, "\n· ")
	if n > 15 {
		s += fmt.Sprintf("\n… und %d weitere", n-15)
	}
	return s
}

// deliverDirect: Meldung an bestimmte Alarmwege und optional Push an alle Geräte des Kontos.
func (m *Monitor) deliverDirect(n notice, chanIDs []string, push bool) {
	o := m.orgByID(n.org)
	if o == nil || o.Disabled {
		return
	}
	want := map[string]bool{}
	for _, id := range chanIDs {
		want[id] = true
	}
	m.mu.RLock()
	var chans []Channel
	for _, c := range m.cfg.Channels {
		if c.Org == n.org && want[c.ID] && !c.Disabled {
			chans = append(chans, c)
		}
	}
	devs := append([]Device(nil), m.cfg.Devices...)
	users := map[string]User{}
	for _, u := range m.cfg.Users {
		users[u.ID] = u
	}
	m.mu.RUnlock()
	at := time.Now()
	for _, c := range chans {
		go func(c Channel) {
			for try := 0; try < 3; try++ {
				err := sendAlert(c, n, at, o.Name)
				if err == nil {
					return
				}
				log.Printf("Netzkarten-Meldung an %s: %v", c.Name, err)
				time.Sleep(time.Duration(5*(try+1)) * time.Second)
			}
		}(c)
	}
	if !push {
		return
	}
	for _, d := range devs {
		if d.Org != n.org {
			continue
		}
		if u, ok := users[d.User]; ok {
			if u.Notify != nil && u.Notify.NoPush {
				continue
			}
			if role, access, _ := u.membership(n.org); len(access) > 0 && role != "owner" {
				continue // eingeschränkte Benutzer: Kartenmeldungen nicht
			}
		}
		var err error
		if d.Kind == "web" {
			err = m.sendWebPush(d, n)
		} else if m.pusher != nil {
			err = m.pusher.send(d, n)
		}
		if err != nil {
			log.Printf("Push an %s: %v", d.Name, err)
		}
	}
}

// POST /api/maps/rules {id, rules:[...]} – Meldungen einer Karte ersetzen.
func (s *Server) handleMapRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	p := principal(r)
	var req struct {
		ID    string
		Rules []MapRule
	}
	if !readJSON(w, r, &req) {
		return
	}
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	_, i, ok := m.mapByID(p.Org, req.ID)
	if !ok {
		http.Error(w, "Karte unbekannt", 404)
		return
	}
	chOK := map[string]bool{}
	for _, c := range m.cfg.Channels {
		if c.Org == p.Org {
			chOK[c.ID] = true
		}
	}
	var out []MapRule
	for _, x := range req.Rules {
		switch x.Scope {
		case "node", "sector", "zone", "all":
		default:
			continue
		}
		if x.ID == "" {
			x.ID = randHex(5)
		}
		x.Name, x.Target = clipStr(strings.TrimSpace(x.Name), 80), clipStr(strings.TrimSpace(x.Target), 100)
		if x.Min < 1 {
			x.Min = 1
		}
		if x.Confirm < 1 || x.Confirm > 3 {
			x.Confirm = 1
		}
		var ch []string
		for _, c := range x.Channels {
			if chOK[c] {
				ch = append(ch, c)
			}
		}
		x.Channels = ch
		out = append(out, x)
		if len(out) >= 200 {
			break
		}
	}
	m.cfg.Maps[i].Rules = out
	m.saveConfigLocked()
	writeJSON(w, map[string]any{"ok": true, "rules": out})
}

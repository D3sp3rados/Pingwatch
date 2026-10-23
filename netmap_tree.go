package main

import (
	"net"
	"net/http"
	"sort"
	"strings"
)

// GET /api/maps/tree?id= – Netzkarte als Liste für die App: Orte → Sektoren/Masten → Kunden.
// Sektor = Gerät mit mehreren Verbindungen, Kunde = Endgerät, das nur an einem Sektor hängt.
// Status je Gerät: Host-Status (wenn überwacht), sonst Erreichbarkeit der Karte.

type treeNode struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Addr  string     `json:"addr,omitempty"`
	St    string     `json:"st"`             // up, down, warn, paused, unknown
	RTT   int32      `json:"rtt,omitempty"`  // Zehntel-ms (Erreichbarkeit)
	Host  string     `json:"host,omitempty"` // überwacht als Host
	Total int        `json:"total,omitempty"`
	Down  int        `json:"down,omitempty"`
	Kids  []treeNode `json:"kids,omitempty"`
}

type treeZone struct {
	Name    string     `json:"name"`
	Total   int        `json:"total"`
	Down    int        `json:"down"`
	Sectors []treeNode `json:"sectors"`
}

func (s *Server) handleMapTree(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	id := r.URL.Query().Get("id")
	m.mu.RLock()
	if id == "" {
		for _, x := range m.cfg.Maps {
			if x.Org == p.Org {
				id = x.ID
				break
			}
		}
	}
	mp, _, ok := m.mapByID(p.Org, id)
	m.mu.RUnlock()
	if !ok {
		writeJSON(w, map[string]any{"zones": []treeZone{}})
		return
	}
	d, err := m.loadMap(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Hosts des Kontos: Status je ID und Adresse
	type hst struct{ id, st string }
	byID, byAddr := map[string]hst{}, map[string]hst{}
	for _, h := range m.hostsOf(p.Org) {
		h.mu.Lock()
		cfg, consec, lastR, lastT := h.cfg, h.consec, h.lastR, h.lastT
		h.mu.Unlock()
		st := "up"
		switch {
		case cfg.Paused:
			st = "paused"
		case lastT == 0:
			st = "unknown"
		case consec >= cfg.OutageCount:
			st = "down"
		case lastR < 0:
			st = "warn"
		}
		x := hst{cfg.ID, st}
		byID[cfg.ID] = x
		if a := strings.ToLower(strings.TrimSpace(cfg.Addr)); a != "" {
			if _, dup := byAddr[a]; !dup {
				byAddr[a] = x
			}
		}
	}
	rc := mapReachView(id)
	status := func(n mapNode) treeNode {
		t := treeNode{ID: n.ID, Name: n.Name, Addr: n.Addr, St: "unknown"}
		var h hst
		var has bool
		if n.Host != "" && n.Host != "-" {
			h, has = byID[n.Host]
		} else if n.Host == "" && n.Addr != "" {
			h, has = byAddr[strings.ToLower(n.Addr)]
		}
		if has && h.st != "paused" {
			t.St, t.Host = h.st, h.id
			return t
		}
		if n.Off {
			t.St = "off" // Ping aus
			return t
		}
		if has {
			t.St, t.Host = "paused", h.id // pausiert: Erreichbarkeit der Karte zeigen, falls vorhanden
		}
		if rc != nil && rc.At > 0 && rc.IPs > 0 {
			if ip := net.ParseIP(strings.TrimSpace(n.Addr)).To4(); ip != nil {
				if v, ok := rc.RTT[ip.String()]; ok {
					t.St, t.RTT = "up", v
				} else {
					t.St = "down"
				}
			}
		}
		return t
	}
	deg := map[string]int{}
	adj := map[string][]string{}
	for _, l := range d.Links {
		deg[l.A]++
		deg[l.B]++
		adj[l.A] = append(adj[l.A], l.B)
		adj[l.B] = append(adj[l.B], l.A)
	}
	idx := map[string]int{}
	for i, n := range d.Nodes {
		idx[n.ID] = i
	}
	hideOld := r.URL.Query().Get("old") == "0"
	old := func(n mapNode) bool {
		l := strings.ToLower(n.Name)
		return strings.Contains(l, "gekündigt") || strings.Contains(l, "gekuendigt") || strings.Contains(l, "abgebaut") ||
			strings.Contains(l, "abgeschaltet") || strings.Contains(l, "weggezogen") || strings.Contains(l, "aus)")
	}
	zones := map[string]*treeZone{}
	used := map[string]bool{}
	zoneOf := func(n mapNode) *treeZone {
		zn := n.Zone
		if zn == "" {
			zn = "Ohne Ort"
		}
		z := zones[zn]
		if z == nil {
			z = &treeZone{Name: zn, Sectors: []treeNode{}}
			zones[zn] = z
		}
		return z
	}
	bad := func(st string) bool { return st == "down" }
	// Sektoren: Geräte mit mehr als einer Verbindung; ihre Endgeräte als Kunden
	for _, n := range d.Nodes {
		if deg[n.ID] < 2 || (hideOld && old(n)) {
			continue
		}
		t := status(n)
		for _, o := range adj[n.ID] {
			i, ok := idx[o]
			if !ok || deg[o] != 1 {
				continue
			}
			c := d.Nodes[i]
			used[c.ID] = true
			if hideOld && old(c) {
				continue
			}
			ct := status(c)
			t.Kids = append(t.Kids, ct)
			t.Total++
			if bad(ct.St) {
				t.Down++
			}
		}
		sortTree(t.Kids)
		used[n.ID] = true
		z := zoneOf(n)
		z.Sectors = append(z.Sectors, t)
		z.Total += 1 + t.Total
		if bad(t.St) {
			z.Down++
		}
		z.Down += t.Down
	}
	// übrige Geräte (ohne Verbindungen) als Einzelgeräte
	for _, n := range d.Nodes {
		if used[n.ID] || (hideOld && old(n)) {
			continue
		}
		t := status(n)
		z := zoneOf(n)
		z.Sectors = append(z.Sectors, t)
		z.Total++
		if bad(t.St) {
			z.Down++
		}
	}
	out := make([]treeZone, 0, len(zones))
	for _, z := range zones {
		sort.SliceStable(z.Sectors, func(i, j int) bool {
			a, b := z.Sectors[i], z.Sectors[j]
			if (a.Down > 0 || bad(a.St)) != (b.Down > 0 || bad(b.St)) {
				return a.Down > 0 || bad(a.St)
			}
			if (len(a.Kids) > 0) != (len(b.Kids) > 0) {
				return len(a.Kids) > 0
			}
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		})
		out = append(out, *z)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name == "Ohne Ort") != (out[j].Name == "Ohne Ort") {
			return out[j].Name == "Ohne Ort"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	var at int64
	checked := false
	if rc != nil {
		at, checked = rc.At, rc.At > 0
	}
	maps := []map[string]string{}
	m.mu.RLock()
	for _, x := range m.cfg.Maps {
		if x.Org == p.Org {
			maps = append(maps, map[string]string{"id": x.ID, "name": x.Name})
		}
	}
	m.mu.RUnlock()
	writeJSON(w, map[string]any{"map": map[string]string{"id": mp.ID, "name": mp.Name}, "maps": maps, "at": at, "checked": checked, "zones": out})
}

func sortTree(l []treeNode) {
	rank := map[string]int{"down": 0, "warn": 1, "unknown": 2, "paused": 3, "up": 4}
	sort.SliceStable(l, func(i, j int) bool {
		if rank[l[i].St] != rank[l[j].St] {
			return rank[l[i].St] < rank[l[j].St]
		}
		return strings.ToLower(l[i].Name) < strings.ToLower(l[j].Name)
	})
}

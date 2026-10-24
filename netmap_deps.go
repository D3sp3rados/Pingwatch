package main

// Abhängigkeiten aus der Netzkarte: Von der Zentrale aus (Standard: Mitte des Netzes,
// also das Gerät mit den kürzesten Wegen zu allen anderen) wird der Weg zu jedem Gerät
// verfolgt. Ein überwachter Host hängt vom nächsten überwachten (nicht pausierten) Host
// auf dem Weg zur Zentrale ab. Vorschläge werden erst nach Bestätigung übernommen.

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

var coreName = regexp.MustCompile(`(?i)core|router|gateway|uplink|bgp|ccr|zentrale|backbone|\brz\b|rechenzentrum`)

type depItem struct {
	Host       string `json:"host"`
	HostName   string `json:"hostName"`
	Zone       string `json:"zone,omitempty"`
	Parent     string `json:"parent"`
	ParentName string `json:"parentName"`
	Hops       int    `json:"hops"` // Geräte ohne Überwachung dazwischen
	Cur        string `json:"cur,omitempty"`
	CurName    string `json:"curName,omitempty"`
}

type depRoot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Deg  int    `json:"deg"`
}

func (s *Server) handleMapDeps(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	if r.Method == http.MethodPost {
		if !p.canEdit() {
			http.Error(w, "keine Berechtigung", 403)
			return
		}
		var req struct {
			Map, Root string
			Set       []struct{ Host, Parent string }
		}
		if !readJSON(w, r, &req) {
			return
		}
		n, errs := 0, 0
		for _, x := range req.Set {
			if !p.canHost(x.Host, true) || m.validParent(p.Org, x.Host, x.Parent) != nil {
				errs++
				continue
			}
			m.mu.Lock()
			for i := range m.cfg.Hosts {
				if m.cfg.Hosts[i].ID == x.Host && m.cfg.Hosts[i].Org == p.Org {
					m.cfg.Hosts[i].Parent = x.Parent
					if h := m.hosts[x.Host]; h != nil {
						h.mu.Lock()
						h.cfg.Parent = x.Parent
						h.mu.Unlock()
					}
					n++
				}
			}
			m.mu.Unlock()
		}
		if req.Map != "" && req.Root != "" {
			m.mu.Lock()
			if _, i, ok := m.mapByID(p.Org, req.Map); ok {
				m.cfg.Maps[i].DepRoot = req.Root
				m.saveConfigLocked()
			}
			m.mu.Unlock()
		}
		if n > 0 {
			m.mu.Lock()
			m.saveConfigLocked()
			m.mu.Unlock()
		}
		writeJSON(w, map[string]int{"set": n, "skipped": errs})
		return
	}
	id, root := r.URL.Query().Get("id"), r.URL.Query().Get("root")
	m.mu.RLock()
	mp, _, ok := m.mapByID(p.Org, id)
	m.mu.RUnlock()
	saved := mp.DepRoot
	if !ok {
		http.Error(w, "unbekannte Karte", 404)
		return
	}
	d, err := m.loadMap(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Hosts des Kontos
	type hst struct {
		id, name, parent string
		paused           bool
	}
	byID, byAddr := map[string]hst{}, map[string]hst{}
	for _, h := range m.hostsOf(p.Org) {
		h.mu.Lock()
		c := h.cfg
		h.mu.Unlock()
		if p.Scope != nil && !p.canHost(c.ID, false) {
			continue
		}
		x := hst{c.ID, c.Name, c.Parent, c.Paused}
		byID[c.ID] = x
		if a := strings.ToLower(strings.TrimSpace(c.Addr)); a != "" {
			if _, dup := byAddr[a]; !dup {
				byAddr[a] = x
			}
		}
	}
	nodeHost := map[string]hst{}
	name := map[string]string{}
	zone := map[string]string{}
	for _, n := range d.Nodes {
		name[n.ID], zone[n.ID] = n.Name, n.Zone
		var h hst
		var has bool
		if n.Host != "" && n.Host != "-" {
			h, has = byID[n.Host]
		} else if n.Host == "" && n.Addr != "" {
			h, has = byAddr[strings.ToLower(strings.TrimSpace(n.Addr))]
		}
		if has {
			nodeHost[n.ID] = h
		}
	}
	adj := map[string][]string{}
	for _, l := range d.Links {
		if l.A == l.B || name[l.A] == "" && name[l.B] == "" {
			continue
		}
		adj[l.A] = append(adj[l.A], l.B)
		adj[l.B] = append(adj[l.B], l.A)
	}
	bfs := func(start string) (map[string]string, map[string]int) {
		par, dist := map[string]string{start: ""}, map[string]int{start: 0}
		q := []string{start}
		for len(q) > 0 {
			c := q[0]
			q = q[1:]
			for _, x := range adj[c] {
				if _, seen := dist[x]; !seen {
					dist[x], par[x] = dist[c]+1, c
					q = append(q, x)
				}
			}
		}
		return par, dist
	}
	for k := range adj {
		sort.Strings(adj[k])
	}
	// Zusammenhängende Teile (größter zuerst); je Teil die Mitte (kleinster größter Abstand) als Zentrale
	ids := make([]string, 0, len(adj))
	for k := range adj {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	done := map[string]bool{}
	var comps []map[string]int
	for _, st := range ids {
		if done[st] {
			continue
		}
		_, comp := bfs(st)
		for c := range comp {
			done[c] = true
		}
		if len(comp) >= 3 {
			comps = append(comps, comp)
		}
	}
	sort.SliceStable(comps, func(i, j int) bool { return len(comps[i]) > len(comps[j]) })
	var roots []string
	var cands []depRoot
	for _, comp := range comps {
		best, bestE := "", 1<<30
		type ce struct {
			id string
			e  int
		}
		var all []ce
		// Zentrale möglichst ein überwachter Host (Router/Uplink), nicht ein Zwischengerät
		mon := func(c string) bool { h, ok := nodeHost[c]; return ok && !h.paused }
		anyMon := false
		for c := range comp {
			if mon(c) {
				anyMon = true
				break
			}
		}
		for c := range comp {
			if anyMon && !mon(c) {
				continue
			}
			_, dd := bfs(c)
			e := 0
			for _, v := range dd {
				if v > e {
					e = v
				}
			}
			if coreName.MatchString(name[c]) {
				e-- // Hauptrouter/Uplink bevorzugen
			}
			all = append(all, ce{c, e})
			if e < bestE || e == bestE && (len(adj[c]) < len(adj[best]) || len(adj[c]) == len(adj[best]) && c < best) {
				best, bestE = c, e
			}
		}
		if _, inComp := comp[saved]; root == "" && saved != "" && inComp {
			best = saved
		}
		if _, inComp := comp[root]; root != "" && inComp {
			best = root
		}
		roots = append(roots, best)
		if len(cands) == 0 { // Auswahl für den größten Teil
			sort.Slice(all, func(i, j int) bool {
				if all[i].e != all[j].e {
					return all[i].e < all[j].e
				}
				return len(adj[all[i].id]) < len(adj[all[j].id])
			})
			for _, c := range all {
				if len(cands) >= 40 {
					break
				}
				cands = append(cands, depRoot{c.id, name[c.id], len(adj[c.id])})
			}
		}
	}
	items := []depItem{}
	same := 0
	seenHost := map[string]bool{}
	for _, rt := range roots {
		par, _ := bfs(rt)
		nids := make([]string, 0, len(nodeHost))
		for nid := range nodeHost {
			nids = append(nids, nid)
		}
		sort.Strings(nids)
		for _, nid := range nids {
			h := nodeHost[nid]
			if _, in := par[nid]; !in || seenHost[h.id] {
				continue
			}
			seenHost[h.id] = true
			hops := 0
			for cur := par[nid]; cur != ""; cur = par[cur] {
				ph, ok := nodeHost[cur]
				if !ok || ph.paused || ph.id == h.id {
					hops++
					continue
				}
				if ph.id == h.parent {
					same++
					break
				}
				it := depItem{Host: h.id, HostName: h.name, Zone: zone[nid], Parent: ph.id, ParentName: ph.name, Hops: hops, Cur: h.parent}
				if h.parent != "" {
					it.CurName = byID[h.parent].name
				}
				items = append(items, it)
				break
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ParentName != items[j].ParentName {
			return items[i].ParentName < items[j].ParentName
		}
		return items[i].HostName < items[j].HostName
	})
	rootName := ""
	if len(roots) > 0 {
		rootName = name[roots[0]]
	}
	rid := ""
	if len(roots) > 0 {
		rid = roots[0]
	}
	writeJSON(w, map[string]any{"root": rid, "rootName": rootName, "roots": cands, "items": items, "same": same})
}

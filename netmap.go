package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var mapEditMu sync.Mutex

// Netzkarte: Geräte (mit oder ohne PingWacht-Host) und ihre Verbindungen auf einer frei
// anordenbaren Fläche. Die Liste der Karten liegt in der Konfiguration, die Inhalte (bis zu
// mehrere tausend Knoten) als Blob „map:<id>“, damit die Konfiguration klein bleibt.

type NetMap struct {
	ID      string `json:"id"`
	Org     string `json:"org"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated,omitempty"`
	Nodes   int    `json:"nodes"`
	Links   int    `json:"links"`
	Source  string `json:"source,omitempty"` // z. B. „The Dude“
	// Erreichbarkeit aller Geräte über den Agenten dieses Standorts prüfen (nur Anzeige)
	PingSite string `json:"pingSite,omitempty"`
	// Traffic per SNMP: Communities (Komma, werden der Reihe nach probiert) und Umfang
	SnmpComm  string `json:"snmpComm,omitempty"`
	SnmpScope string `json:"snmpScope,omitempty"` // "" = Sektoren + markierte, "all", "off"
	// Meldungen (einzelne Geräte, Sektoren, Orte, ganze Karte)
	Rules []MapRule `json:"rules,omitempty"`
}

type mapNode struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Addr string  `json:"addr,omitempty"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Host string  `json:"host,omitempty"` // verknüpfter PingWacht-Host (leer = automatisch über die Adresse)
	Zone string  `json:"zone,omitempty"` // Ort/Zone zum Filtern (z. B. „Laufenburg“)
	Snmp bool    `json:"snmp,omitempty"` // Traffic per SNMP abfragen (zusätzlich zu Sektoren)
}

type mapLink struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Type string `json:"type,omitempty"`
}

type mapData struct {
	Nodes []mapNode `json:"nodes"`
	Links []mapLink `json:"links"`
}

func mapKey(id string) string { return "map:" + id }

func (m *Monitor) loadMap(id string) (*mapData, error) {
	bs, ok := m.store.(blobStore)
	if !ok {
		return nil, errString("Speicher unterstützt keine Karten")
	}
	b, err := bs.LoadBlob(mapKey(id))
	if err != nil || len(b) == 0 {
		return &mapData{Nodes: []mapNode{}, Links: []mapLink{}}, nil
	}
	var d mapData
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	if d.Nodes == nil {
		d.Nodes = []mapNode{}
	}
	if d.Links == nil {
		d.Links = []mapLink{}
	}
	return &d, nil
}

func (m *Monitor) saveMap(id string, d *mapData) error {
	bs, ok := m.store.(blobStore)
	if !ok {
		return errString("Speicher unterstützt keine Karten")
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return bs.SaveBlob(mapKey(id), b)
}

func (m *Monitor) mapByID(org, id string) (NetMap, int, bool) {
	for i, x := range m.cfg.Maps {
		if x.ID == id && x.Org == org {
			return x, i, true
		}
	}
	return NetMap{}, -1, false
}

// cleanMap: Knoten mit eindeutigen IDs, Links nur zwischen vorhandenen Knoten, Längen begrenzt.
func cleanMap(d *mapData) {
	seen := map[string]bool{}
	nodes := d.Nodes[:0]
	for _, n := range d.Nodes {
		n.ID = strings.TrimSpace(n.ID)
		if n.ID == "" || seen[n.ID] || len(nodes) >= 20000 {
			continue
		}
		seen[n.ID] = true
		n.Name, n.Addr, n.Zone = clipStr(strings.TrimSpace(n.Name), 200), clipStr(strings.TrimSpace(n.Addr), 200), clipStr(strings.TrimSpace(n.Zone), 60)
		nodes = append(nodes, n)
	}
	d.Nodes = nodes
	ls := map[string]bool{}
	links := d.Links[:0]
	for _, l := range d.Links {
		if !seen[l.A] || !seen[l.B] || l.A == l.B || len(links) >= 40000 {
			continue
		}
		k := l.A + "|" + l.B
		if l.B < l.A {
			k = l.B + "|" + l.A
		}
		if ls[k] {
			continue
		}
		ls[k] = true
		l.Type = clipStr(l.Type, 60)
		links = append(links, l)
	}
	d.Links = links
}

// GET    /api/maps            → Liste
// GET    /api/maps?id=        → Inhalt (Knoten ohne Host-Verknüpfung werden über die Adresse zugeordnet)
// POST   /api/maps            {id?, name, nodes?, links?, source?}  anlegen/umbenennen/ersetzen
// POST   /api/maps/edit       {id, moves:[{id,x,y}], set:[{id,host,name,addr}], add:[node], del:[id], link:[{a,b,type}], unlink:[{a,b}]}
// DELETE /api/maps?id=
func (s *Server) handleMaps(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		if id == "" {
			m.mu.RLock()
			out := []NetMap{}
			for _, x := range m.cfg.Maps {
				if x.Org == p.Org {
					if !p.canEdit() {
						x.SnmpComm = ""
					}
					out = append(out, x)
				}
			}
			m.mu.RUnlock()
			sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
			writeJSON(w, out)
			return
		}
		m.mu.RLock()
		mp, _, ok := m.mapByID(p.Org, id)
		byAddr := map[string]string{}
		hostOK := map[string]bool{}
		for _, hc := range m.cfg.Hosts {
			if hc.Org == p.Org {
				hostOK[hc.ID] = true
				if a := strings.ToLower(strings.TrimSpace(hc.Addr)); a != "" {
					if _, dup := byAddr[a]; !dup {
						byAddr[a] = hc.ID
					}
				}
			}
		}
		m.mu.RUnlock()
		if !ok {
			http.Error(w, "Karte unbekannt", 404)
			return
		}
		d, err := m.loadMap(id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type nodeOut struct {
			mapNode
			Auto   bool `json:"auto,omitempty"`   // Host über die Adresse gefunden
			NoAuto bool `json:"noAuto,omitempty"` // Verknüpfung bewusst gelöst
		}
		nodes := make([]nodeOut, 0, len(d.Nodes))
		for _, n := range d.Nodes {
			o := nodeOut{mapNode: n}
			if n.Host == "-" {
				o.Host, o.NoAuto = "", true // Verknüpfung bewusst entfernt
				nodes = append(nodes, o)
				continue
			} else if n.Host != "" && !hostOK[n.Host] {
				o.Host = ""
			}
			if n.Host == "" && n.Addr != "" {
				if hid := byAddr[strings.ToLower(n.Addr)]; hid != "" {
					o.Host, o.Auto = hid, true
				}
			}
			nodes = append(nodes, o)
		}
		if !p.canEdit() {
			mp.SnmpComm = ""
		}
		mapStats.Lock()
		snmp := map[string]any{"at": mapStats.snmpLast[id], "ok": mapStats.snmpOK[id]}
		mapStats.Unlock()
		writeJSON(w, map[string]any{"map": mp, "nodes": nodes, "links": d.Links, "reach": mapReachView(id), "snmp": snmp})
	case http.MethodPost:
		var req struct {
			ID, Name, Source string
			PingSite         *string
			SnmpComm         *string
			SnmpScope        *string
			Nodes            *[]mapNode
			Links            *[]mapLink
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
			http.Error(w, "ungültige Anfrage", 400)
			return
		}
		name := clipStr(strings.TrimSpace(req.Name), 80)
		m.mu.Lock()
		var mp NetMap
		if req.ID == "" {
			if name == "" {
				name = "Netzkarte"
			}
			mp = NetMap{ID: randHex(5), Org: p.Org, Name: name, Created: time.Now().UnixMilli(), Source: clipStr(req.Source, 40)}
			m.cfg.Maps = append(m.cfg.Maps, mp)
		} else {
			x, i, ok := m.mapByID(p.Org, req.ID)
			if !ok {
				m.mu.Unlock()
				http.Error(w, "Karte unbekannt", 404)
				return
			}
			if name != "" {
				m.cfg.Maps[i].Name = name
			}
			if req.SnmpComm != nil {
				m.cfg.Maps[i].SnmpComm = clipStr(strings.TrimSpace(*req.SnmpComm), 200)
			}
			if req.SnmpScope != nil && (*req.SnmpScope == "" || *req.SnmpScope == "all" || *req.SnmpScope == "off") {
				m.cfg.Maps[i].SnmpScope = *req.SnmpScope
			}
			if req.PingSite != nil {
				ps := *req.PingSite
				if ps != "" {
					if sc, ok := m.siteByIDLocked(ps); !ok || sc.Org != p.Org {
						m.mu.Unlock()
						http.Error(w, "Standort unbekannt", 400)
						return
					}
				}
				m.cfg.Maps[i].PingSite = ps
				mapPing.Lock()
				delete(mapPing.last, req.ID) // gleich prüfen
				delete(mapPing.reach, req.ID)
				mapPing.Unlock()
			}
			mp = m.cfg.Maps[i]
			_ = x
		}
		m.mu.Unlock()
		if req.Nodes != nil {
			d := &mapData{Nodes: *req.Nodes, Links: []mapLink{}}
			if req.Links != nil {
				d.Links = *req.Links
			}
			cleanMap(d)
			if err := m.saveMap(mp.ID, d); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			s.touchMap(p.Org, mp.ID, d)
		}
		m.mu.Lock()
		m.saveConfigLocked()
		mp, _, _ = m.mapByID(p.Org, mp.ID)
		m.mu.Unlock()
		writeJSON(w, mp)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		keep := m.cfg.Maps[:0]
		for _, x := range m.cfg.Maps {
			if !(x.ID == id && x.Org == p.Org) {
				keep = append(keep, x)
			}
		}
		m.cfg.Maps = keep
		m.saveConfigLocked()
		m.mu.Unlock()
		if bs, ok := m.store.(blobStore); ok {
			bs.DeleteBlob(mapKey(id))
		}
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

// touchMap aktualisiert Zähler und Zeitstempel in der Liste.
func (s *Server) touchMap(org, id string, d *mapData) {
	m := s.m
	m.mu.Lock()
	if _, i, ok := m.mapByID(org, id); ok {
		m.cfg.Maps[i].Nodes, m.cfg.Maps[i].Links, m.cfg.Maps[i].Updated = len(d.Nodes), len(d.Links), time.Now().UnixMilli()
	}
	m.saveConfigLocked()
	m.mu.Unlock()
}

func (s *Server) handleMapEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	p := principal(r)
	var req struct {
		ID    string
		Moves []struct {
			ID   string
			X, Y float64
		}
		Set []struct {
			ID                     string
			Host, Name, Addr, Zone *string
			Snmp                   *bool
		}
		Add    []mapNode
		Del    []string
		Link   []mapLink
		Unlink []mapLink
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültige Anfrage", 400)
		return
	}
	m := s.m
	m.mu.RLock()
	_, _, ok := m.mapByID(p.Org, req.ID)
	m.mu.RUnlock()
	if !ok {
		http.Error(w, "Karte unbekannt", 404)
		return
	}
	mapEditMu.Lock()
	defer mapEditMu.Unlock()
	d, err := m.loadMap(req.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	idx := map[string]int{}
	for i, n := range d.Nodes {
		idx[n.ID] = i
	}
	for _, mv := range req.Moves {
		if i, ok := idx[mv.ID]; ok {
			d.Nodes[i].X, d.Nodes[i].Y = mv.X, mv.Y
		}
	}
	for _, st := range req.Set {
		i, ok := idx[st.ID]
		if !ok {
			continue
		}
		if st.Host != nil {
			h := strings.TrimSpace(*st.Host)
			if h != "" && h != "-" && s.hostFor(r, h) == nil {
				continue
			}
			d.Nodes[i].Host = h
		}
		if st.Name != nil {
			d.Nodes[i].Name = *st.Name
		}
		if st.Addr != nil {
			d.Nodes[i].Addr = *st.Addr
		}
		if st.Snmp != nil {
			d.Nodes[i].Snmp = *st.Snmp
		}
		if st.Zone != nil {
			d.Nodes[i].Zone = clipStr(strings.TrimSpace(*st.Zone), 60)
		}
	}
	for _, n := range req.Add {
		if n.ID == "" {
			n.ID = "n" + randHex(5)
		}
		d.Nodes = append(d.Nodes, n)
	}
	if len(req.Del) > 0 {
		del := map[string]bool{}
		for _, id := range req.Del {
			del[id] = true
		}
		keep := d.Nodes[:0]
		for _, n := range d.Nodes {
			if !del[n.ID] {
				keep = append(keep, n)
			}
		}
		d.Nodes = keep
	}
	d.Links = append(d.Links, req.Link...)
	if len(req.Unlink) > 0 {
		un := map[string]bool{}
		for _, l := range req.Unlink {
			un[l.A+"|"+l.B], un[l.B+"|"+l.A] = true, true
		}
		keep := d.Links[:0]
		for _, l := range d.Links {
			if !un[l.A+"|"+l.B] {
				keep = append(keep, l)
			}
		}
		d.Links = keep
	}
	cleanMap(d)
	if err := m.saveMap(req.ID, d); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.touchMap(p.Org, req.ID, d)
	writeJSON(w, map[string]any{"ok": true, "nodes": len(d.Nodes), "links": len(d.Links)})
}

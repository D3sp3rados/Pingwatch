package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Import von Hosts (z. B. aus Livewatch oder einer Excel-/CSV-Liste).
// POST /api/import {rows:[{name,addr,type,port,expect,group,channel,paused}], site, dryRun}

type importRow struct {
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	Type    string `json:"type"` // ping, http, tcp (auch Livewatch: PING, CONT, SCKT)
	Port    string `json:"port"`
	Expect  string `json:"expect"`
	Group   string `json:"group"`   // Kunde (Gruppe) – wird angelegt, falls nicht vorhanden
	Channel string `json:"channel"` // Name eines vorhandenen Alarmwegs
	Paused  bool   `json:"paused"`
}

type importResult struct {
	Line    int    `json:"line"`
	Name    string `json:"name"`
	Status  string `json:"status"` // neu, vorhanden, fehler
	Message string `json:"message,omitempty"`
	Method  string `json:"method"`
	Addr    string `json:"addr"`
}

func normImportType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "ping", "icmp":
		return ""
	case "cont", "http", "https", "web", "webseite", "content":
		return "http"
	case "sckt", "tcp", "socket", "port":
		return "tcp"
	case "dns":
		return "dns"
	}
	return "?"
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	p := principal(r)
	var req struct {
		Rows   []importRow `json:"rows"`
		Site   string      `json:"site"`
		DryRun bool        `json:"dryRun"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Rows) == 0 || len(req.Rows) > 1000 {
		http.Error(w, "Keine oder zu viele Zeilen (max. 1000)", 400)
		return
	}
	site := req.Site
	if site == "" {
		site = "cloud"
	}
	if site != "cloud" && s.siteFor(r, site) == nil {
		http.Error(w, "unbekannter Standort", 400)
		return
	}
	m := s.m
	o := m.orgByID(p.Org)
	if o == nil {
		http.Error(w, "Kunde unbekannt", 404)
		return
	}
	checks := planHas(o.Plan, "checks")
	minIv := planByID(o.Plan).MinInterval

	// vorhandene Hosts, Gruppen und Alarmwege
	m.mu.RLock()
	existing := map[string]bool{}
	for _, h := range m.cfg.Hosts {
		if h.Org == p.Org {
			hp := 0
			if h.Method == "tcp" {
				hp = h.Port
			}
			existing[strings.ToLower(normImportType(h.Method)+"|"+h.Addr+"|"+strconv.Itoa(hp))] = true
			if h.Addr == "" {
				existing[strings.ToLower("name|"+h.Name)] = true
			}
		}
	}
	groups := map[string]string{}
	for _, g := range m.cfg.Groups {
		if g.Org == p.Org {
			groups[strings.ToLower(g.Name)] = g.ID
		}
	}
	chans := map[string]string{}
	for _, c := range m.cfg.Channels {
		if c.Org == p.Org {
			chans[strings.ToLower(c.Name)] = c.ID
		}
	}
	m.mu.RUnlock()

	var out []importResult
	newCount := 0
	chanHosts := map[string][]string{}
	for i, row := range req.Rows {
		res := importResult{Line: i + 1, Name: strings.TrimSpace(row.Name)}
		addr := strings.TrimSpace(row.Addr)
		method := normImportType(row.Type)
		port, _ := strconv.Atoi(strings.TrimSpace(row.Port))
		res.Method, res.Addr = method, addr
		switch {
		case addr == "" && (!row.Paused || res.Name == ""):
			res.Status, res.Message = "fehler", "Adresse fehlt"
		case method == "?":
			res.Status, res.Message = "fehler", "Prüfart unbekannt: "+row.Type
		case method == "tcp" && addr != "" && (port < 1 || port > 65535):
			res.Status, res.Message = "fehler", "Port fehlt"
		case isCheckMethod(method) && !checks:
			res.Status, res.Message = "fehler", "Webseiten-/DNS-Prüfungen gibt es im Paket Pro"
		}
		if method == "http" && addr != "" && !strings.Contains(addr, "://") {
			addr = "http://" + addr
			res.Addr = addr
		}
		if res.Name == "" {
			res.Name = addr
		}
		kp := 0
		if method == "tcp" {
			kp = port
		}
		key := strings.ToLower(method + "|" + addr + "|" + strconv.Itoa(kp))
		if addr == "" { // Platzhalter ohne Adresse: am Namen erkennen
			key = strings.ToLower("name|" + res.Name)
		}
		if res.Status == "" && existing[key] {
			res.Status, res.Message = "vorhanden", "gibt es schon – übersprungen"
		}
		if res.Status == "" {
			res.Status = "neu"
			if ch := strings.TrimSpace(row.Channel); ch != "" && chans[strings.ToLower(ch)] == "" {
				res.Message = "Alarmweg „" + ch + "“ nicht gefunden – bitte danach zuordnen"
			}
			if !req.DryRun {
				if err := s.checkLimits(p.Org, 0, 1); err != nil {
					res.Status, res.Message = "fehler", err.Error()
					out = append(out, res)
					continue
				}
				gid := ""
				if gn := strings.TrimSpace(row.Group); gn != "" {
					gid = groups[strings.ToLower(gn)]
					if gid == "" {
						g := Group{ID: randHex(5), Org: p.Org, Name: gn, Created: time.Now().UnixMilli()}
						m.mu.Lock()
						m.cfg.Groups = append(m.cfg.Groups, g)
						m.saveConfigLocked()
						m.mu.Unlock()
						gid = g.ID
						groups[strings.ToLower(gn)] = gid
					}
				}
				hc := HostCfg{Name: res.Name, Addr: addr, Site: site, Method: method, Port: port, Expect: strings.TrimSpace(row.Expect),
					Paused: row.Paused, Org: p.Org, Group: gid, IntervalMs: 60000}
				if hc.IntervalMs < minIv {
					hc.IntervalMs = minIv
				}
				hc = m.UpsertHost(hc)
				m.auditHostSave(p, nil, hc, "Import")
				if cid := chans[strings.ToLower(strings.TrimSpace(row.Channel))]; cid != "" {
					chanHosts[cid] = append(chanHosts[cid], hc.ID)
				}
			}
			existing[key] = true
			newCount++
		}
		out = append(out, res)
	}
	if !req.DryRun && len(chanHosts) > 0 {
		m.mu.Lock()
		for i := range m.cfg.Channels {
			c := &m.cfg.Channels[i]
			// Die importierten Hosts werden dem Alarmweg zugeordnet (wie ein Livewatch-Benachrichtigungsset).
			if hs := chanHosts[c.ID]; hs != nil && c.Org == p.Org {
				c.Hosts = append(c.Hosts, hs...)
			}
		}
		m.saveConfigLocked()
		m.mu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true, "new": newCount, "rows": out, "dryRun": req.DryRun})
}

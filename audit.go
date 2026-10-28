package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Änderungsprotokoll: wer hat wann Hosts, Standorte und Gruppen angelegt, geändert, pausiert oder gelöscht.
// Einsehbar nur für den Betreiber (Super-Admin), über alle Konten.

const auditMax = 3000

type AuditEntry struct {
	T      int64  `json:"t"`
	Org    string `json:"org"`
	Who    string `json:"who"`
	Act    string `json:"act"`  // angelegt, geändert, gelöscht, pausiert, fortgesetzt, stummgeschaltet
	Kind   string `json:"kind"` // Host, Standort, Gruppe
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

// whoOf: Anzeige des handelnden Benutzers.
func whoOf(p *Principal) string {
	if p == nil {
		return "System"
	}
	if p.User != nil {
		if p.User.Name != "" && p.User.Email != "" {
			return p.User.Name + " (" + p.User.Email + ")"
		}
		if p.User.Email != "" {
			return p.User.Email
		}
		return p.User.Name
	}
	return "Betreiber (Admin-Login)"
}

func (m *Monitor) audit(org, who, act, kind, name, detail string) {
	m.mu.Lock()
	m.auditLocked(org, who, act, kind, name, detail)
	m.saveConfigLocked()
	m.mu.Unlock()
}

// auditLocked: Eintrag ergänzen, wenn m.mu schon gehalten wird (Speichern übernimmt der Aufrufer).
func (m *Monitor) auditLocked(org, who, act, kind, name, detail string) {
	m.cfg.Audit = append(m.cfg.Audit, AuditEntry{T: time.Now().UnixMilli(), Org: org, Who: who, Act: act, Kind: kind, Name: name, Detail: detail})
	if n := len(m.cfg.Audit); n > auditMax {
		m.cfg.Audit = append([]AuditEntry(nil), m.cfg.Audit[n-auditMax:]...)
	}
}

// hostWhere: Adresse und Prüfort für die Detailspalte.
func (m *Monitor) hostWhere(hc HostCfg) string {
	where := "Cloud"
	if hc.Site != "" && hc.Site != "cloud" {
		if sc, ok := m.siteByID(hc.Site); ok {
			where = sc.Name
		}
	}
	return hc.Addr + " · " + where
}

// hostDiff: lesbare Liste der geänderten Felder.
func (m *Monitor) hostDiff(a, b HostCfg) string {
	var d []string
	add := func(label, x, y string) {
		if x != y {
			d = append(d, label+": „"+x+"“ → „"+y+"“")
		}
	}
	add("Name", a.Name, b.Name)
	add("Adresse", a.Addr, b.Addr)
	add("Methode", a.Method, b.Method)
	if a.Site != b.Site {
		d = append(d, "Prüfort geändert")
	}
	if a.Group != b.Group {
		gn := func(id string) string {
			if id == "" {
				return "ohne Gruppe"
			}
			m.mu.RLock()
			defer m.mu.RUnlock()
			for _, g := range m.cfg.Groups {
				if g.ID == id {
					return g.Name
				}
			}
			return id
		}
		d = append(d, "Gruppe: „"+gn(a.Group)+"“ → „"+gn(b.Group)+"“")
	}
	if a.IntervalMs != b.IntervalMs {
		d = append(d, "Intervall geändert")
	}
	if a.Port != b.Port {
		d = append(d, "Port geändert")
	}
	if a.ThresholdMs != b.ThresholdMs || a.OutageCount != b.OutageCount {
		d = append(d, "Alarm-Schwellen geändert")
	}
	if len(d) == 0 && a != b {
		d = append(d, "Einstellungen geändert")
	}
	return strings.Join(d, " · ")
}

// auditHostSave protokolliert Anlegen/Ändern/Pausieren eines Hosts (old == nil: neu).
func (m *Monitor) auditHostSave(p *Principal, old *HostCfg, nw HostCfg, via string) {
	who := whoOf(p)
	if via != "" {
		who += " · " + via
	}
	if old == nil {
		det := m.hostWhere(nw)
		if nw.Paused {
			det += " · pausiert angelegt"
		}
		m.audit(nw.Org, who, "angelegt", "Host", nw.Name, det)
		return
	}
	o := *old
	if o.Paused != nw.Paused {
		act := "fortgesetzt"
		if nw.Paused {
			act = "pausiert"
		}
		m.audit(nw.Org, who, act, "Host", nw.Name, nw.Addr)
		o.Paused, o.PauseUntil = nw.Paused, nw.PauseUntil
	}
	// Passwörter und vom Server gepflegte Felder nicht vergleichen
	o.FritzPass, o.MtPass, o.MtActPass, o.NxPass = nw.FritzPass, nw.MtPass, nw.MtActPass, nw.NxPass
	o.Watch = nw.Watch
	if o != nw {
		m.audit(nw.Org, who, "geändert", "Host", nw.Name, m.hostDiff(o, nw))
	}
}

// GET /api/audit – nur Betreiber
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p == nil || !p.Super {
		http.Error(w, "nur für den Betreiber", 403)
		return
	}
	s.m.mu.RLock()
	names := map[string]string{}
	for _, o := range s.m.cfg.Orgs {
		names[o.ID] = o.Name
	}
	out := make([]map[string]any, 0, len(s.m.cfg.Audit))
	for _, e := range s.m.cfg.Audit {
		out = append(out, map[string]any{"t": e.T, "org": e.Org, "orgName": names[e.Org], "who": e.Who, "act": e.Act, "kind": e.Kind, "name": e.Name, "detail": e.Detail})
	}
	s.m.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i]["t"].(int64) > out[j]["t"].(int64) })
	writeJSON(w, out)
}

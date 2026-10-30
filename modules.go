package main

import (
	"net/http"
	"strings"
)

// Freischaltbare Bereiche je Konto (setzt nur der Betreiber). Konten von vor dieser Einführung
// (ModulesSet=false) behalten alles; neue Konten starten ohne diese Bereiche.
var allModules = []struct{ ID, Name string }{
	{"netmap", "Netzkarte"},
	{"radius", "RADIUS (Netonix-Logins)"},
	{"mtnx", "MikroTik/Netonix-Abfrage"},
	{"sysact", "Rechner-Updates, Neustarts und Scripts"},
	{"pi", "Raspberry-Pi-Einrichtung"},
}

func validModule(id string) bool {
	for _, m := range allModules {
		if m.ID == id {
			return true
		}
	}
	return false
}

func (o *Org) has(mod string) bool {
	if o == nil {
		return false
	}
	if !o.ModulesSet {
		return true
	}
	for _, x := range o.Modules {
		if x == mod {
			return true
		}
	}
	return false
}

// modules: Liste der freigeschalteten Bereiche (für die Oberfläche)
func (o *Org) modules() []string {
	out := []string{}
	for _, m := range allModules {
		if o.has(m.ID) {
			out = append(out, m.ID)
		}
	}
	return out
}

func (m *Monitor) orgHas(orgID, mod string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.cfg.Orgs {
		if m.cfg.Orgs[i].ID == orgID {
			return m.cfg.Orgs[i].has(mod)
		}
	}
	return false
}

// needMod sperrt eine Schnittstelle, wenn der Bereich für das aktuelle Konto nicht freigeschaltet ist.
func (s *Server) needMod(mod string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		if p == nil || !s.m.orgHas(p.Org, mod) {
			name := mod
			for _, x := range allModules {
				if x.ID == mod {
					name = x.Name
				}
			}
			http.Error(w, "„"+name+"“ ist für dieses Konto nicht freigeschaltet", 403)
			return
		}
		h(w, r)
	}
}

func cleanModules(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, x := range in {
		x = strings.TrimSpace(x)
		if validModule(x) && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

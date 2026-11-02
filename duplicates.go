package main

import "strings"

// Doppelte Einträge verhindern: gleicher Host (Adresse, Methode, Port, Prüfort) bzw. gleiche
// UniFi-Konsole/-Site oder gleicher Standortname im selben Konto.

func dupAddr(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	a = strings.TrimSuffix(a, "/")
	for _, p := range []string{"https://", "http://"} {
		a = strings.TrimPrefix(a, p)
	}
	return a
}

// duplicateHost: Name eines anderen Hosts mit gleicher Prüfung, sonst "".
func (m *Monitor) duplicateHost(hc HostCfg) string {
	if strings.TrimSpace(hc.Addr) == "" || isUniFiWanHost(hc) {
		return ""
	}
	site := hc.Site
	if site == "" {
		site = "cloud"
	}
	a := dupAddr(hc.Addr)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, h := range m.hosts {
		if id == hc.ID {
			continue
		}
		h.mu.Lock()
		c := h.cfg
		h.mu.Unlock()
		cs := c.Site
		if cs == "" {
			cs = "cloud"
		}
		if c.Org != hc.Org || cs != site || c.Method != hc.Method || dupAddr(c.Addr) != a {
			continue
		}
		if c.Method == "tcp" && c.Port != hc.Port {
			continue
		}
		if c.Method == "http" && (c.HTTPVerb != hc.HTTPVerb || c.HTTPBody != hc.HTTPBody || c.Expect != hc.Expect) {
			continue
		}
		return c.Name
	}
	return ""
}

// duplicateSite: Meldung, wenn es den Standort im Konto schon gibt, sonst "".
func (m *Monitor) duplicateSite(org string, sc SiteCfg) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, o := range m.cfg.Sites {
		if o.Org != org || o.ID == sc.ID {
			continue
		}
		if sc.Name != "" && strings.EqualFold(strings.TrimSpace(o.Name), sc.Name) {
			return "Einen Standort „" + o.Name + "“ gibt es schon"
		}
		if sc.UniFi != nil && sc.UniFi.On && sc.UniFi.isCloud() && sc.UniFi.Console != "" && o.UniFi != nil && o.UniFi.On && o.UniFi.isCloud() &&
			o.UniFi.Console == sc.UniFi.Console && strings.EqualFold(o.UniFi.Site, sc.UniFi.Site) {
			return "Diese UniFi-Konsole ist schon als Standort „" + o.Name + "“ angelegt"
		}
	}
	return ""
}

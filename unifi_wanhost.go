package main

// Internet-Leitung je UniFi-Gateway als eigener Host („unifiwan“): Die Konsole misst ihre
// WAN-Latenz selbst (UniFi-Internetüberwachung). Diesen Wert holen wir ohnehin jede Minute ab
// (Agent oder UniFi-Cloud) und speichern ihn wie einen Ping – mit Zeitleiste, Ausreißern,
// Statistik nach Tag/Uhrzeit. Pro Leitung wird der Host einmal automatisch angelegt; wird er
// gelöscht, kommt er nicht wieder (UniFiCfg.WanHosts merkt sich die angelegten Leitungen).
// Ausfälle der Leitung meldet weiterhin die UniFi-Überwachung („WAN offline“) – der Host
// selbst meldet nicht doppelt.

import (
	"math"
	"strings"
)

const methodUniFiWAN = "unifiwan"

func unifiWanAddr(siteID, devKey, wan string) string {
	return "unifi:" + siteID + "/" + devKey + "/" + wan
}

func isUniFiWanHost(hc HostCfg) bool { return hc.Method == methodUniFiWAN }

// unifiWanSamples: nach jedem UniFi-Abruf je WAN-Leitung einen Messwert eintragen.
func (m *Monitor) unifiWanSamples(site SiteCfg, devs []unifiDevice, at int64) {
	if site.UniFi == nil {
		return
	}
	for _, d := range devs {
		key := d.MAC
		if key == "" {
			key = d.Name
		}
		for _, w := range d.WAN {
			if site.UniFi.ignoresWAN(w.Name) {
				continue
			}
			addr := unifiWanAddr(site.ID, key, w.Name)
			h := m.unifiWanHost(site, d, w, addr)
			if h == nil {
				continue
			}
			h.mu.Lock()
			paused := h.cfg.Paused
			h.mu.Unlock()
			if paused {
				continue
			}
			switch {
			case !w.Up:
				m.Ingest(h, at, -1, w.IP, "Leitung "+w.Name+" laut UniFi getrennt")
			case w.Latency > 0:
				r := int32(math.Round(w.Latency * 10))
				m.Ingest(h, at, r, w.IP, "")
			}
		}
	}
}

// unifiWanHost sucht den Host einer Leitung oder legt ihn (einmalig) an.
func (m *Monitor) unifiWanHost(site SiteCfg, d unifiDevice, w unifiWAN, addr string) *Host {
	m.mu.RLock()
	for _, hc := range m.cfg.Hosts {
		if hc.Method == methodUniFiWAN && hc.Addr == addr {
			h := m.hosts[hc.ID]
			m.mu.RUnlock()
			return h
		}
	}
	m.mu.RUnlock()
	if !w.Up || w.Latency <= 0 {
		return nil // erst anlegen, wenn die Leitung einmal mit Messwert da war
	}
	m.mu.Lock()
	idx := -1
	for i := range m.cfg.Sites {
		if m.cfg.Sites[i].ID == site.ID {
			idx = i
		}
	}
	if idx < 0 || m.cfg.Sites[idx].UniFi == nil {
		m.mu.Unlock()
		return nil
	}
	for _, k := range m.cfg.Sites[idx].UniFi.WanHosts {
		if k == addr { // schon einmal angelegt und gelöscht
			m.mu.Unlock()
			return nil
		}
	}
	m.cfg.Sites[idx].UniFi.WanHosts = append(m.cfg.Sites[idx].UniFi.WanHosts, addr)
	m.mu.Unlock()
	name := "Internet " + w.Name + " · " + site.Name
	if n := m.unifiGatewayCount(site.ID); n > 1 && strings.TrimSpace(d.Name) != "" {
		name += " (" + strings.TrimSpace(d.Name) + ")"
	}
	hc := m.UpsertHost(HostCfg{Name: name, Addr: addr, Site: site.ID, Method: methodUniFiWAN, IntervalMs: 60000, OutageCount: 2, Org: site.Org})
	return m.host(hc.ID)
}

func (m *Monitor) unifiGatewayCount(siteID string) int {
	unifiLiveState.Lock()
	defer unifiLiveState.Unlock()
	n := 0
	if st := unifiLiveState.bySite[siteID]; st != nil {
		for _, d := range st.Devices {
			if len(d.WAN) > 0 {
				n++
			}
		}
	}
	return n
}

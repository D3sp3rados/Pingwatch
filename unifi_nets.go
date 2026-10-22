package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// Netze (VLANs) einer UniFi-Konsole für den Netzwerk-Scan: erst über die Integration-API,
// sonst über die klassische Network-Schnittstelle (rest/networkconf). Liefert private Netze,
// höchstens /22 (größere werden auf das /24 um die Gateway-Adresse begrenzt).

const maxScanNets = 16

func (u *unifiAPI) networks(siteFilter string) ([]string, error) {
	var sites struct {
		Data []struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			InternalReference string `json:"internalReference"`
		} `json:"data"`
	}
	if err := u.get(unifiIntAPI+"/sites?limit=200", &sites); err != nil {
		return nil, err
	}
	if siteFilter == "" && len(sites.Data) > unifiCloudMaxAll {
		return nil, fmt.Errorf("Die Konsole verwaltet %d Sites – bitte im UniFi-Livestatus die UniFi-Site wählen", len(sites.Data))
	}
	var cidrs []string
	matched := false
	for _, st := range sites.Data {
		if siteFilter != "" && !strings.EqualFold(siteFilter, st.Name) && siteFilter != st.ID && siteFilter != st.InternalReference {
			continue
		}
		matched = true
		got := u.intNetworks(st.ID)
		if len(got) == 0 {
			legacy := st.InternalReference
			if legacy == "" {
				legacy = "default"
			}
			got = u.legacyNetworks(legacy)
		}
		cidrs = append(cidrs, got...)
	}
	if siteFilter != "" && !matched {
		return nil, fmt.Errorf("UniFi-Site „%s“ auf der Konsole nicht gefunden", siteFilter)
	}
	out := normScanNets(cidrs)
	if len(out) == 0 {
		return nil, errors.New("Die UniFi-Konsole hat keine Netze geliefert (Netzwerk-Liste über die API nicht verfügbar)")
	}
	return out, nil
}

// intNetworks: Integration-API (UniFi Network ab 9.x/10.x). Das Format ist nicht überall gleich,
// daher werden Adresse und Präfix in der Antwort gesucht.
func (u *unifiAPI) intNetworks(siteID string) []string {
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if u.get(fmt.Sprintf("%s/sites/%s/networks?limit=200", unifiIntAPI, siteID), &list) != nil {
		return nil
	}
	var out []string
	for _, n := range list.Data {
		if b, ok := n["enabled"].(bool); ok && !b {
			continue
		}
		found := findCIDRs(n)
		if len(found) == 0 {
			if id, _ := n["id"].(string); id != "" {
				var det map[string]any
				if u.get(fmt.Sprintf("%s/sites/%s/networks/%s", unifiIntAPI, siteID, id), &det) == nil {
					found = findCIDRs(det)
				}
			}
		}
		out = append(out, found...)
	}
	return out
}

// legacyNetworks: klassische Schnittstelle, ip_subnet = „192.168.10.1/24“.
func (u *unifiAPI) legacyNetworks(site string) []string {
	code, b, err := u.raw("/proxy/network/api/s/" + site + "/rest/networkconf")
	if err != nil || code != 200 {
		return nil
	}
	var res struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(b, &res) != nil {
		return nil
	}
	var out []string
	for _, n := range res.Data {
		if p, _ := n["purpose"].(string); p == "wan" || p == "vpn-client" || p == "remote-user-vpn" {
			continue
		}
		if e, ok := n["enabled"].(bool); ok && !e {
			continue
		}
		if s, _ := n["ip_subnet"].(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// findCIDRs sucht in einer JSON-Struktur nach „a.b.c.d/nn“ oder Adresse + prefixLength.
func findCIDRs(v any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			var ip string
			pfx := -1
			for k, val := range x {
				lk := strings.ToLower(k)
				switch t := val.(type) {
				case string:
					if _, _, err := net.ParseCIDR(t); err == nil {
						out = append(out, t)
					} else if p := net.ParseIP(t); p != nil && p.To4() != nil && (strings.Contains(lk, "host") || strings.Contains(lk, "gateway") || lk == "ipaddress" || lk == "address") {
						ip = t
					}
				case float64:
					if strings.Contains(lk, "prefix") && t >= 8 && t <= 32 {
						pfx = int(t)
					}
				default:
					walk(val)
				}
			}
			if ip != "" && pfx > 0 {
				out = append(out, fmt.Sprintf("%s/%d", ip, pfx))
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// normScanNets: Netzadressen bilden, nur private Netze, zu große auf /24 begrenzen, doppelte entfernen.
func normScanNets(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range in {
		ip, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil || ip.To4() == nil || !isPrivateNet(n) {
			continue
		}
		ones, _ := n.Mask.Size()
		if ones < 22 {
			n = &net.IPNet{IP: ip.To4().Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}
		}
		if s := n.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _, _ := net.ParseCIDR(out[i])
		b, _, _ := net.ParseCIDR(out[j])
		return binary.BigEndian.Uint32(a.To4()) < binary.BigEndian.Uint32(b.To4())
	})
	if len(out) > maxScanNets {
		out = out[:maxScanNets]
	}
	return out
}

// clientNames: IP → Name der verbundenen Clients (Integration-API), für Namen im Scan-Ergebnis.
func (u *unifiAPI) clientNames(siteFilter string) map[string]string {
	out := map[string]string{}
	var sites struct {
		Data []struct {
			ID, Name, InternalReference string
		} `json:"data"`
	}
	if u.get(unifiIntAPI+"/sites?limit=200", &sites) != nil {
		return out
	}
	if siteFilter == "" && len(sites.Data) > unifiCloudMaxAll {
		return out
	}
	for _, st := range sites.Data {
		if siteFilter != "" && !strings.EqualFold(siteFilter, st.Name) && siteFilter != st.ID && siteFilter != st.InternalReference {
			continue
		}
		for offset := 0; offset < 2000; offset += 200 {
			var page struct {
				Data []struct {
					Name      string `json:"name"`
					IPAddress string `json:"ipAddress"`
				} `json:"data"`
				TotalCount int `json:"totalCount"`
			}
			if u.get(fmt.Sprintf("%s/sites/%s/clients?offset=%d&limit=200", unifiIntAPI, st.ID, offset), &page) != nil {
				break
			}
			for _, c := range page.Data {
				if c.IPAddress != "" && c.Name != "" {
					out[c.IPAddress] = c.Name
				}
			}
			if len(page.Data) < 200 || offset+200 >= page.TotalCount {
				break
			}
		}
	}
	return out
}

// nameScanHits: leere Namen aus UniFi (Geräte und Clients) ergänzen.
func nameScanHits(res *scanResult, names map[string]string) {
	for i := range res.Hits {
		if res.Hits[i].Name == "" {
			if n := names[res.Hits[i].IP]; n != "" {
				res.Hits[i].Name = n
			}
		}
	}
}

// unifiScanNames (Server): Namen aus dem UniFi-Livestatus des Standorts, bei Cloud zusätzlich die Clients.
func (m *Monitor) unifiScanNames(site SiteCfg) map[string]string {
	names := map[string]string{}
	if site.UniFi == nil || !site.UniFi.On {
		return names
	}
	if site.UniFi.isCloud() {
		if key := m.unifiCloudKey(site); key != "" && site.UniFi.Console != "" {
			names = newUniFiCloudAPI(site.UniFi.Console, key).clientNames(site.UniFi.Site)
		}
	}
	unifiLiveState.Lock()
	if st := unifiLiveState.bySite[site.ID]; st != nil {
		for _, d := range st.Devices {
			if d.IP != "" && d.Name != "" {
				names[d.IP] = d.Name
			}
		}
	}
	unifiLiveState.Unlock()
	return names
}

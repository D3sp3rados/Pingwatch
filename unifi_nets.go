package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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
		for _, l := range u.lans(st.ID, st.InternalReference) {
			cidrs = append(cidrs, l.IP)
		}
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

// unifiLAN: ein Netz (VLAN) der Konsole mit Gateway-Adresse, z. B. „Gäste“ · 192.168.20.1/24 · VLAN 20.
type unifiLAN struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	VLAN int    `json:"vlan,omitempty"`
}

func lanFromMap(n map[string]any) (unifiLAN, bool) {
	name, _ := n["name"].(string)
	l := unifiLAN{Name: name}
	for _, k := range []string{"vlanId", "vlan"} {
		if l.VLAN > 0 {
			break
		}
		switch v := n[k].(type) {
		case float64:
			l.VLAN = int(v)
		case string:
			l.VLAN, _ = strconv.Atoi(v)
		}
	}
	if s, _ := n["ip_subnet"].(string); s != "" {
		l.IP = s
	} else if c := findCIDRs(n); len(c) > 0 {
		l.IP = c[0]
	}
	return l, l.IP != ""
}

// intNetworks: Integration-API (UniFi Network ab 9.x/10.x). Das Format ist nicht überall gleich,
// daher werden Adresse und Präfix in der Antwort gesucht.
func (u *unifiAPI) intNetworks(siteID string) []unifiLAN {
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if u.get(fmt.Sprintf("%s/sites/%s/networks?limit=200", unifiIntAPI, siteID), &list) != nil {
		return nil
	}
	var out []unifiLAN
	for _, n := range list.Data {
		if b, ok := n["enabled"].(bool); ok && !b {
			continue
		}
		l, ok := lanFromMap(n)
		if !ok {
			if id, _ := n["id"].(string); id != "" {
				var det map[string]any
				if u.get(fmt.Sprintf("%s/sites/%s/networks/%s", unifiIntAPI, siteID, id), &det) == nil {
					if d, ok2 := lanFromMap(det); ok2 {
						if d.Name == "" {
							d.Name = l.Name
						}
						if d.VLAN == 0 {
							d.VLAN = l.VLAN
						}
						l, ok = d, true
					}
				}
			}
		}
		if ok {
			out = append(out, l)
		}
	}
	return out
}

// legacyNetworks: klassische Schnittstelle, ip_subnet = „192.168.10.1/24“.
func (u *unifiAPI) legacyNetworks(site string) []unifiLAN {
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
	var out []unifiLAN
	for _, n := range res.Data {
		if p, _ := n["purpose"].(string); p == "wan" || p == "vpn-client" || p == "remote-user-vpn" {
			continue
		}
		if e, ok := n["enabled"].(bool); ok && !e {
			continue
		}
		if l, ok := lanFromMap(n); ok {
			out = append(out, l)
		}
	}
	return out
}

// lans: Netze einer Site (Integration-API, sonst klassisch), sortiert nach VLAN.
func (u *unifiAPI) lans(siteID, legacy string) []unifiLAN {
	got := u.intNetworks(siteID)
	if len(got) == 0 {
		if legacy == "" {
			legacy = "default"
		}
		got = u.legacyNetworks(legacy)
	}
	sort.SliceStable(got, func(i, j int) bool {
		vi, vj := got[i].VLAN, got[j].VLAN
		if vi == 0 {
			vi = 1
		}
		if vj == 0 {
			vj = 1
		}
		return vi < vj
	})
	return got
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

// gwExtra: LAN-Netze und WAN-Adresse des Gateways – ändern sich selten, daher 10 Minuten zwischengespeichert.
var gwExtraCache = struct {
	sync.Mutex
	m map[string]gwExtra
}{m: map[string]gwExtra{}}

type gwExtra struct {
	at    time.Time
	lans  []unifiLAN
	wanIP string
}

func (u *unifiAPI) gatewayExtras(siteID, legacy string, d *unifiDevice) {
	key := u.base + "|" + siteID + "|" + d.ID
	gwExtraCache.Lock()
	x, ok := gwExtraCache.m[key]
	gwExtraCache.Unlock()
	if !ok || time.Since(x.at) > 10*time.Minute {
		x = gwExtra{at: time.Now(), lans: u.lans(siteID, legacy)}
		if d.ID != "" {
			var det map[string]any
			if u.get(fmt.Sprintf("%s/sites/%s/devices/%s", unifiIntAPI, siteID, d.ID), &det) == nil {
				x.wanIP = findWANIP(det)
			}
		}
		gwExtraCache.Lock()
		gwExtraCache.m[key] = x
		gwExtraCache.Unlock()
	}
	d.LANs = x.lans
	if len(d.WAN) == 0 {
		d.WANIP = x.wanIP
	}
}

// findWANIP sucht in den Gerätedetails eine IPv4-Adresse unter einem Schlüssel mit „wan“.
func findWANIP(v any) string {
	var hit string
	var walk func(any, bool)
	walk = func(v any, inWAN bool) {
		if hit != "" {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			w := inWAN
			if n, _ := x["name"].(string); strings.Contains(strings.ToLower(n), "wan") {
				w = true
			}
			if t, _ := x["type"].(string); strings.Contains(strings.ToLower(t), "wan") {
				w = true
			}
			for k, val := range x {
				lk := strings.ToLower(k)
				kw := w || strings.Contains(lk, "wan")
				if s, ok := val.(string); ok && kw && strings.Contains(lk, "ip") {
					ip := s
					if i := strings.Index(ip, "/"); i > 0 {
						ip = ip[:i]
					}
					if p := net.ParseIP(ip); p != nil && p.To4() != nil && !p.IsUnspecified() {
						hit = ip
						return
					}
				}
				walk(val, kw)
			}
		case []any:
			for _, e := range x {
				walk(e, inWAN)
			}
		}
	}
	walk(v, false)
	return hit
}

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UniFi-Details auf Abruf (Ports, WLAN, Clients, Firmware), Aktionen (Neustart, PoE-Neustart) und
// Protect-Vorschaubilder. Cloud-Standorte fragen direkt über den Cloud-Proxy, Agent-Standorte über
// einen Auftrag „unifiapi“ an den Agenten (der erreicht die Konsole im lokalen Netz).

// req: beliebige Anfrage an die Konsole (GET/POST), liefert Status, Inhalt und Content-Type.
func (u *unifiAPI) req(method, path string, body []byte) (int, []byte, string, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, _ := http.NewRequest(method, u.base+path, rd)
	r.Header.Set("X-API-KEY", u.key)
	r.Header.Set("Accept", "application/json, image/jpeg, */*")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.cl.Do(r)
	if err != nil {
		if u.cloud {
			return 0, nil, "", fmt.Errorf("UniFi-Cloud nicht erreichbar: %v", shortErr(err))
		}
		return 0, nil, "", fmt.Errorf("UniFi-Konsole nicht erreichbar: %v", shortErr(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if resp.StatusCode == 429 {
		return 429, b, "", &unifiRateErr{retry: retryAfter(resp.Header.Get("Retry-After"))}
	}
	return resp.StatusCode, b, resp.Header.Get("Content-Type"), nil
}

// unifiPathOK: nur die offiziellen Schnittstellen (Network/Protect) und die Geräteliste – nichts anderes.
func unifiPathOK(method, p string) bool {
	if strings.Contains(p, "..") {
		return false
	}
	if strings.HasPrefix(p, unifiIntAPI+"/") || strings.HasPrefix(p, protectAPI+"/") {
		return method == "GET" || (method == "POST" && strings.HasSuffix(p, "/actions"))
	}
	return method == "GET" && strings.HasPrefix(p, "/proxy/network/api/s/") && strings.Contains(p, "/stat/device")
}

type unifiAPIResult struct {
	Code int    `json:"code"`
	CT   string `json:"ct,omitempty"`
	B64  string `json:"b64,omitempty"`
}

// Agent: Auftrag „unifiapi“ ausführen.
func unifiAPIJob(p map[string]string) (*unifiAPIResult, error) {
	method, path := strings.ToUpper(p["method"]), p["path"]
	if !unifiPathOK(method, path) {
		return nil, errors.New("Anfrage nicht erlaubt")
	}
	var body []byte
	if p["body"] != "" {
		body = []byte(p["body"])
	}
	code, b, ct, err := newUniFiAPI(p["url"], p["key"]).req(method, path, body)
	if err != nil {
		return nil, err
	}
	return &unifiAPIResult{Code: code, CT: ct, B64: base64.StdEncoding.EncodeToString(b)}, nil
}

// agentJobWait: Auftrag an den Agenten eines Standorts geben und auf das Ergebnis warten.
func (m *Monitor) agentJobWait(site SiteCfg, kind string, params map[string]string, timeout time.Duration) (json.RawMessage, error) {
	if on, _ := m.siteOnline(site.ID); !on {
		return nil, errors.New("Der Agent dieses Standorts ist gerade nicht verbunden")
	}
	j := agentJob{ID: randHex(8), Kind: kind, Params: params}
	jobs.Lock()
	jobs.byID[j.ID] = &jobState{agentJob: j, Org: site.Org, Site: site.ID, Status: "queued", Created: time.Now().UnixMilli()}
	jobs.queue[site.ID] = append(jobs.queue[site.ID], j)
	jobs.Unlock()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		time.Sleep(250 * time.Millisecond)
		jobs.Lock()
		st := jobs.byID[j.ID]
		var status, e string
		var res json.RawMessage
		if st != nil {
			status, e, res = st.Status, st.Error, st.Result
			if status == "done" || status == "error" {
				delete(jobs.byID, j.ID)
			}
		}
		jobs.Unlock()
		switch {
		case st == nil:
			return nil, errors.New("Auftrag verloren")
		case status == "error":
			if strings.Contains(e, "unbekannt") {
				return nil, errors.New("Der Agent ist zu alt für diese Funktion – bitte unter Standorte aktualisieren")
			}
			return nil, errors.New(e)
		case status == "done":
			return res, nil
		}
	}
	return nil, errors.New("Der Agent hat nicht rechtzeitig geantwortet")
}

// unifiCall: Anfrage an die UniFi-Konsole eines Standorts – über die Cloud oder den Agenten.
func (m *Monitor) unifiCall(site SiteCfg, method, path string, body []byte) (int, []byte, string, error) {
	if site.UniFi == nil || !site.UniFi.On {
		return 0, nil, "", errors.New("UniFi ist an diesem Standort nicht eingerichtet")
	}
	if !unifiPathOK(method, path) {
		return 0, nil, "", errors.New("Anfrage nicht erlaubt")
	}
	if site.UniFi.isCloud() {
		key := m.unifiCloudKey(site)
		if key == "" {
			return 0, nil, "", errors.New("Kein UniFi-Cloud-Key hinterlegt")
		}
		return newUniFiCloudAPI(site.UniFi.Console, key).req(method, path, body)
	}
	raw, err := m.agentJobWait(site, "unifiapi", map[string]string{"url": site.UniFi.URL, "key": site.UniFi.Key, "method": method, "path": path, "body": string(body)}, 45*time.Second)
	if err != nil {
		return 0, nil, "", err
	}
	var r unifiAPIResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, nil, "", errors.New("Antwort des Agenten nicht lesbar")
	}
	b, _ := base64.StdEncoding.DecodeString(r.B64)
	return r.Code, b, r.CT, nil
}

func (m *Monitor) unifiGet(site SiteCfg, path string, v any) error {
	code, b, _, err := m.unifiCall(site, "GET", path, nil)
	if err != nil {
		return err
	}
	if code >= 300 {
		return (&unifiAPI{cloud: site.UniFi.isCloud()}).httpErr(code)
	}
	return json.Unmarshal(b, v)
}

// ---------- UniFi-Sites (ID ↔ Name) ----------

type unifiSiteRef struct{ ID, Name, Legacy string }

var unifiSiteCache = struct {
	sync.Mutex
	m map[string]struct {
		at   time.Time
		list []unifiSiteRef
	}
}{m: map[string]struct {
	at   time.Time
	list []unifiSiteRef
}{}}

func (m *Monitor) unifiSites(site SiteCfg) ([]unifiSiteRef, error) {
	unifiSiteCache.Lock()
	e, ok := unifiSiteCache.m[site.ID]
	unifiSiteCache.Unlock()
	if ok && time.Since(e.at) < 10*time.Minute {
		return e.list, nil
	}
	var res struct {
		Data []struct {
			ID, Name, InternalReference string
		} `json:"data"`
	}
	if err := m.unifiGet(site, unifiIntAPI+"/sites?limit=200", &res); err != nil {
		return nil, err
	}
	var out []unifiSiteRef
	for _, s := range res.Data {
		l := s.InternalReference
		if l == "" {
			l = "default"
		}
		out = append(out, unifiSiteRef{ID: s.ID, Name: s.Name, Legacy: l})
	}
	unifiSiteCache.Lock()
	unifiSiteCache.m[site.ID] = struct {
		at   time.Time
		list []unifiSiteRef
	}{time.Now(), out}
	unifiSiteCache.Unlock()
	return out, nil
}

// unifiSiteRefs: die UniFi-Sites, die zu diesem PingWacht-Standort gehören (Filter oder Name).
func (m *Monitor) unifiSiteRefs(site SiteCfg, name string) ([]unifiSiteRef, error) {
	all, err := m.unifiSites(site)
	if err != nil {
		return nil, err
	}
	want := name
	if want == "" {
		want = site.UniFi.Site
	}
	var out []unifiSiteRef
	for _, s := range all {
		if want == "" || strings.EqualFold(want, s.Name) || want == s.ID || want == s.Legacy {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("UniFi-Site nicht gefunden")
	}
	return out, nil
}

// ---------- Details eines Geräts ----------

type uxPort struct {
	Idx      int     `json:"idx"`
	Name     string  `json:"name,omitempty"`
	Up       bool    `json:"up"`
	Speed    int     `json:"speed,omitempty"` // Mbit/s
	Max      int     `json:"max,omitempty"`
	Conn     string  `json:"conn,omitempty"` // RJ45, SFP+ …
	PoE      string  `json:"poe,omitempty"`  // Standard/Typ, leer = kein PoE
	PoEOn    bool    `json:"poeOn,omitempty"`
	PoEState string  `json:"poeState,omitempty"`
	PoEW     float64 `json:"poeW,omitempty"`
	Uplink   bool    `json:"uplink,omitempty"`
	Client   string  `json:"client,omitempty"` // angeschlossenes Gerät
}

type uxRadio struct {
	Band    string  `json:"band"`
	Std     string  `json:"std,omitempty"`
	Channel int     `json:"channel,omitempty"`
	Width   int     `json:"width,omitempty"`
	Util    float64 `json:"util,omitempty"` // Kanalauslastung %
	Clients int     `json:"clients"`
	Retries float64 `json:"retries,omitempty"`
	TxPower int     `json:"txPower,omitempty"`
}

type uxClient struct {
	Name     string `json:"name"`
	IP       string `json:"ip,omitempty"`
	MAC      string `json:"mac,omitempty"`
	Type     string `json:"type,omitempty"` // WIRED, WIRELESS, VPN …
	Since    int64  `json:"since,omitempty"`
	Uplink   string `json:"uplink,omitempty"` // Gerät, an dem er hängt
	UplinkID string `json:"-"`
}

type uxDetail struct {
	Name      string     `json:"name"`
	Model     string     `json:"model,omitempty"`
	FW        string     `json:"fw,omitempty"`
	Upgrade   string     `json:"upgrade,omitempty"` // verfügbare Firmware ("ja" = Version unbekannt)
	Uplink    string     `json:"uplink,omitempty"`
	Ports     []uxPort   `json:"ports,omitempty"`
	Radios    []uxRadio  `json:"radios,omitempty"`
	Clients   []uxClient `json:"clients,omitempty"`
	CanReboot bool       `json:"canReboot"`
	Warn      []string   `json:"warn,omitempty"`
}

func atoiAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

func floatAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
}

func bandName(f float64, radio string) string {
	switch {
	case f >= 5.9 || radio == "6e":
		return "6 GHz"
	case f >= 4.9 || radio == "na":
		return "5 GHz"
	case f > 0 || radio == "ng":
		return "2,4 GHz"
	}
	return radio
}

// unifiClients: alle Clients einer UniFi-Site (höchstens 2000).
func (m *Monitor) unifiClients(site SiteCfg, ref unifiSiteRef) ([]uxClient, error) {
	var out []uxClient
	for off := 0; off < 2000; off += 200 {
		var page struct {
			Data []struct {
				Type, ID, Name, IPAddress, MacAddress, UplinkDeviceID, ConnectedAt string
			} `json:"data"`
			TotalCount int `json:"totalCount"`
		}
		if err := m.unifiGet(site, fmt.Sprintf("%s/sites/%s/clients?offset=%d&limit=200", unifiIntAPI, ref.ID, off), &page); err != nil {
			return out, err
		}
		for _, c := range page.Data {
			x := uxClient{Name: c.Name, IP: c.IPAddress, MAC: strings.ToLower(c.MacAddress), Type: c.Type, UplinkID: c.UplinkDeviceID}
			if t, err := time.Parse(time.RFC3339, c.ConnectedAt); err == nil {
				x.Since = t.UnixMilli()
			}
			if x.Name == "" {
				x.Name = x.MAC
			}
			out = append(out, x)
		}
		if len(page.Data) < 200 || off+200 >= page.TotalCount {
			break
		}
	}
	return out, nil
}

// unifiDeviceDetail: Ports, Funk, Clients und Firmware eines Geräts.
func (m *Monitor) unifiDeviceDetail(site SiteCfg, usite, id, mac string) (*uxDetail, error) {
	refs, err := m.unifiSiteRefs(site, usite)
	if err != nil {
		return nil, err
	}
	ref := refs[0]
	var dev map[string]any
	if err := m.unifiGet(site, fmt.Sprintf("%s/sites/%s/devices/%s", unifiIntAPI, ref.ID, id), &dev); err != nil {
		return nil, err
	}
	d := &uxDetail{Name: anyStr(dev["name"]), Model: anyStr(dev["model"]), FW: anyStr(dev["firmwareVersion"]), CanReboot: true}
	if up, _ := dev["firmwareUpdatable"].(bool); up {
		d.Upgrade = "ja"
	}
	names := map[string]string{} // Geräte-ID → Name (für Uplink und Clients)
	if ifs, ok := dev["interfaces"].(map[string]any); ok {
		if ports, ok := ifs["ports"].([]any); ok {
			for _, x := range ports {
				p, _ := x.(map[string]any)
				if p == nil {
					continue
				}
				up := uxPort{Idx: atoiAny(p["idx"]), Up: strings.EqualFold(anyStr(p["state"]), "UP"), Speed: atoiAny(p["speedMbps"]), Max: atoiAny(p["maxSpeedMbps"]), Conn: anyStr(p["connector"])}
				if poe, ok := p["poe"].(map[string]any); ok {
					up.PoE = strings.TrimSpace(anyStr(poe["standard"]) + " " + anyStr(poe["type"]))
					if up.PoE == "" {
						up.PoE = "PoE"
					}
					up.PoEOn, _ = poe["enabled"].(bool)
					up.PoEState = anyStr(poe["state"])
				}
				d.Ports = append(d.Ports, up)
			}
		}
		if radios, ok := ifs["radios"].([]any); ok {
			for _, x := range radios {
				r, _ := x.(map[string]any)
				if r == nil {
					continue
				}
				d.Radios = append(d.Radios, uxRadio{Band: bandName(floatAny(r["frequencyGHz"]), ""), Std: anyStr(r["wlanStandard"]), Channel: atoiAny(r["channel"]), Width: atoiAny(r["channelWidthMHz"])})
			}
		}
	}
	// Statistik: Funk-Wiederholungen
	var st map[string]any
	if m.unifiGet(site, fmt.Sprintf("%s/sites/%s/devices/%s/statistics/latest", unifiIntAPI, ref.ID, id), &st) == nil {
		if ifs, ok := st["interfaces"].(map[string]any); ok {
			if radios, ok := ifs["radios"].([]any); ok {
				for _, x := range radios {
					r, _ := x.(map[string]any)
					b := bandName(floatAny(r["frequencyGHz"]), "")
					for i := range d.Radios {
						if d.Radios[i].Band == b {
							d.Radios[i].Retries = floatAny(r["txRetriesPct"])
						}
					}
				}
			}
		}
	}
	// klassische Schnittstelle (falls der Key sie darf): PoE-Leistung, Portnamen, Auslastung, Firmware-Version
	if mac != "" {
		if code, b, _, err := m.unifiCall(site, "GET", "/proxy/network/api/s/"+ref.Legacy+"/stat/device/"+strings.ToLower(mac), nil); err == nil && code == 200 {
			var res struct {
				Data []map[string]any `json:"data"`
			}
			if json.Unmarshal(b, &res) == nil && len(res.Data) > 0 {
				mergeLegacyDevice(d, res.Data[0])
			}
		}
	}
	// Clients: alle der Site holen, die an diesem Gerät zählen
	cl, cerr := m.unifiClients(site, ref)
	if cerr != nil {
		d.Warn = append(d.Warn, "Clients: "+shortErr(cerr))
	}
	for _, c := range cl {
		if c.UplinkID == id {
			d.Clients = append(d.Clients, c)
		}
	}
	// Uplink-Gerät benennen
	if ul, ok := dev["uplink"].(map[string]any); ok {
		if uid := anyStr(ul["deviceId"]); uid != "" {
			var u map[string]any
			if m.unifiGet(site, fmt.Sprintf("%s/sites/%s/devices/%s", unifiIntAPI, ref.ID, uid), &u) == nil {
				names[uid] = anyStr(u["name"])
				d.Uplink = names[uid]
			}
		}
	}
	if len(d.Radios) == 1 && !d.legacyRadio() {
		for _, c := range d.Clients {
			if strings.EqualFold(c.Type, "WIRELESS") {
				d.Radios[0].Clients++
			}
		}
	}
	return d, nil
}

func (d *uxDetail) legacyRadio() bool {
	for _, r := range d.Radios {
		if r.Util > 0 || r.TxPower > 0 {
			return true
		}
	}
	return false
}

// mergeLegacyDevice ergänzt Daten aus stat/device (PoE-Watt, Portnamen, Kanalauslastung, Clients je Band, Update-Version).
func mergeLegacyDevice(d *uxDetail, g map[string]any) {
	if up, _ := g["upgradable"].(bool); up {
		d.Upgrade = anyStr(g["upgrade_to_firmware"])
		if d.Upgrade == "" {
			d.Upgrade = "ja"
		}
	}
	if pt, ok := g["port_table"].([]any); ok {
		for _, x := range pt {
			p, _ := x.(map[string]any)
			idx := atoiAny(p["port_idx"])
			for i := range d.Ports {
				if d.Ports[i].Idx != idx {
					continue
				}
				if n := anyStr(p["name"]); n != "" && !strings.HasPrefix(n, "Port ") {
					d.Ports[i].Name = n
				}
				d.Ports[i].PoEW = floatAny(p["poe_power"])
				d.Ports[i].Uplink, _ = p["is_uplink"].(bool)
				if lu, ok := p["last_connection"].(map[string]any); ok && d.Ports[i].Up {
					d.Ports[i].Client = anyStr(lu["mac"])
				}
			}
		}
	}
	if rs, ok := g["radio_table_stats"].([]any); ok {
		for _, x := range rs {
			r, _ := x.(map[string]any)
			b := bandName(0, anyStr(r["radio"]))
			found := false
			for i := range d.Radios {
				if d.Radios[i].Band == b {
					d.Radios[i].Util = floatAny(r["cu_total"])
					d.Radios[i].Clients = atoiAny(r["num_sta"])
					d.Radios[i].TxPower = atoiAny(r["tx_power"])
					if d.Radios[i].Channel == 0 {
						d.Radios[i].Channel = atoiAny(r["channel"])
					}
					found = true
				}
			}
			if !found && b != "" {
				d.Radios = append(d.Radios, uxRadio{Band: b, Channel: atoiAny(r["channel"]), Util: floatAny(r["cu_total"]), Clients: atoiAny(r["num_sta"]), TxPower: atoiAny(r["tx_power"])})
			}
		}
	}
}

// ---------- Firmware-Updates im Livestatus (alle 30 min eine Abfrage je Site) ----------

var unifiUpgCache = struct {
	sync.Mutex
	m map[string]struct {
		at  time.Time
		upg map[string]string
	}
}{m: map[string]struct {
	at  time.Time
	upg map[string]string
}{}}

// upgrades: MAC → verfügbare Firmware (klassische Schnittstelle; ohne Rechte bleibt es leer).
func (u *unifiAPI) upgrades(legacy string) map[string]string {
	k := u.base + "|" + legacy
	unifiUpgCache.Lock()
	e, ok := unifiUpgCache.m[k]
	unifiUpgCache.Unlock()
	if ok && time.Since(e.at) < 30*time.Minute {
		return e.upg
	}
	upg := map[string]string{}
	if code, b, err := u.raw("/proxy/network/api/s/" + legacy + "/stat/device"); err == nil && code == 200 {
		var res struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, g := range res.Data {
				if up, _ := g["upgradable"].(bool); up {
					v := anyStr(g["upgrade_to_firmware"])
					if v == "" {
						v = "ja"
					}
					upg[strings.ToLower(anyStr(g["mac"]))] = v
				}
			}
		}
	}
	unifiUpgCache.Lock()
	unifiUpgCache.m[k] = struct {
		at  time.Time
		upg map[string]string
	}{time.Now(), upg}
	unifiUpgCache.Unlock()
	return upg
}

// ---------- HTTP ----------

func (s *Server) unifiSiteReq(w http.ResponseWriter, r *http.Request, id string) (SiteCfg, bool) {
	site := s.siteFor(r, id)
	if site == nil || site.UniFi == nil || !site.UniFi.On {
		http.Error(w, "Standort ohne UniFi", 404)
		return SiteCfg{}, false
	}
	return *site, true
}

// GET /api/unifi/device?site=&id=&usite=&mac=
func (s *Server) handleUniFiDevice(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	site, ok := s.unifiSiteReq(w, r, q.Get("site"))
	if !ok {
		return
	}
	if q.Get("id") == "" {
		http.Error(w, "Gerät fehlt", 400)
		return
	}
	d, err := s.m.unifiDeviceDetail(site, q.Get("usite"), q.Get("id"), q.Get("mac"))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	d.CanReboot = principal(r).canEdit()
	writeJSON(w, d)
}

// GET /api/unifi/clients?site= → alle Clients des Standorts mit Namen des Uplink-Geräts
func (s *Server) handleUniFiClients(w http.ResponseWriter, r *http.Request) {
	site, ok := s.unifiSiteReq(w, r, r.URL.Query().Get("site"))
	if !ok {
		return
	}
	refs, err := s.m.unifiSiteRefs(site, "")
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	names := map[string]string{}
	unifiLiveState.Lock()
	if st := unifiLiveState.bySite[site.ID]; st != nil {
		for _, d := range st.Devices {
			names[d.ID] = devLabel(d.unifiDevice)
		}
	}
	unifiLiveState.Unlock()
	out := []uxClient{}
	for _, ref := range refs {
		cl, err := s.m.unifiClients(site, ref)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		for _, c := range cl {
			c.Uplink = names[c.UplinkID]
			out = append(out, c)
		}
	}
	writeJSON(w, out)
}

// POST /api/unifi/devaction {site, usite, id, name, action: restart|poe, port}
func (s *Server) handleUniFiDevAction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.canEdit() {
		http.Error(w, "Keine Berechtigung", 403)
		return
	}
	var req struct {
		Site, USite, ID, Name, Action string
		Port                          int
	}
	if !readJSON(w, r, &req) {
		return
	}
	site, ok := s.unifiSiteReq(w, r, req.Site)
	if !ok {
		return
	}
	refs, err := s.m.unifiSiteRefs(site, req.USite)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	var path, body, what string
	switch req.Action {
	case "restart":
		path = fmt.Sprintf("%s/sites/%s/devices/%s/actions", unifiIntAPI, refs[0].ID, req.ID)
		body, what = `{"action":"RESTART"}`, "Neustart"
	case "poe":
		if req.Port <= 0 {
			http.Error(w, "Port fehlt", 400)
			return
		}
		path = fmt.Sprintf("%s/sites/%s/devices/%s/interfaces/ports/%d/actions", unifiIntAPI, refs[0].ID, req.ID, req.Port)
		body, what = `{"action":"POWER_CYCLE"}`, fmt.Sprintf("PoE-Neustart Port %d", req.Port)
	default:
		http.Error(w, "Aktion unbekannt", 400)
		return
	}
	code, b, _, err := s.m.unifiCall(site, "POST", path, []byte(body))
	if err == nil && code >= 300 {
		msg := strings.TrimSpace(string(b))
		var e struct{ Message string }
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		err = fmt.Errorf("UniFi lehnt ab (HTTP %d) %s", code, msg)
		if code == 401 || code == 403 {
			err = errors.New("Der API-Key darf keine Aktionen ausführen (HTTP " + strconv.Itoa(code) + ")")
		}
	}
	who := whoOf(p)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	now := time.Now().UnixMilli()
	label := req.Name + " · " + what
	s.m.closeSiteEvent(s.m.newSiteEvent(site, "action", "UniFi · "+label+" (von "+who+")", now, true), now)
	s.m.audit(site.Org, who, "ausgeführt", "UniFi", req.Name, what+" · "+site.Name)
	writeJSON(w, map[string]any{"ok": true, "msg": what + " ausgelöst"})
}

// GET /api/unifi/snapshot?site=&cam= → JPEG der Kamera (Protect)
func (s *Server) handleUniFiSnapshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	site, ok := s.unifiSiteReq(w, r, q.Get("site"))
	if !ok {
		return
	}
	cam := q.Get("cam")
	if cam == "" || strings.ContainsAny(cam, "/?&#.") {
		http.Error(w, "Kamera fehlt", 400)
		return
	}
	code, b, ct, err := s.m.unifiCall(site, "GET", protectAPI+"/cameras/"+cam+"/snapshot", nil)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if code != 200 || !strings.HasPrefix(ct, "image/") {
		http.Error(w, fmt.Sprintf("Kein Bild (HTTP %d)", code), 502)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Zugriff auf die UniFi-Konsole: lokal im Agenten vor Ort (https://<konsole>) oder zentral im Server
// über den Cloud-Proxy von Ubiquiti (https://api.ui.com/v1/connector/consoles/<id>). Beide Wege nutzen
// dieselben Pfade (/proxy/network/integration/v1 …) und denselben Code.

type unifiWAN struct {
	Name    string  `json:"name"`              // WAN, WAN2 …
	Up      bool    `json:"up"`                // Leitung verbunden
	IP      string  `json:"ip,omitempty"`      // öffentliche bzw. WAN-Adresse
	Latency float64 `json:"latency,omitempty"` // ms (Durchschnitt laut UDM)
	Avail   float64 `json:"avail,omitempty"`   // Verfügbarkeit in % laut UDM
	Active  bool    `json:"active,omitempty"`  // aktuell genutzte Leitung
}

type unifiPing struct {
	N    int     `json:"n"`
	Lost int     `json:"lost"`
	Avg  float64 `json:"avg"` // ms
	Max  float64 `json:"max"`
}

type unifiDevice struct {
	ID       string     `json:"id,omitempty"`
	Name     string     `json:"name"`
	IP       string     `json:"ip"`
	MAC      string     `json:"mac"`
	Model    string     `json:"model"`
	State    string     `json:"state"`
	Site     string     `json:"site"`
	Features []string   `json:"features,omitempty"`
	Firmware string     `json:"fw,omitempty"`
	Uptime   int64      `json:"uptime,omitempty"` // Sekunden
	CPU      float64    `json:"cpu,omitempty"`    // %
	Mem      float64    `json:"mem,omitempty"`    // %
	Tx       float64    `json:"tx,omitempty"`     // Uplink bit/s
	Rx       float64    `json:"rx,omitempty"`
	Ping     *unifiPing `json:"ping,omitempty"`
	WAN      []unifiWAN `json:"wan,omitempty"`
}

type unifiAPI struct {
	base, key string
	cl        *http.Client
	cloud     bool // über den Cloud-Proxy von Ubiquiti (Server) statt lokal (Agent)
	noPing    bool // eigener Ping nicht möglich (Server erreicht das Kundennetz nicht)
}

func newUniFiAPI(base, key string) *unifiAPI {
	return &unifiAPI{base: strings.TrimRight(strings.TrimSpace(base), "/"), key: key, cl: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // UniFi-Konsolen haben ein selbst signiertes Zertifikat
	}}}
}

// unifiCloudBase: Site-Manager-/Connector-API von Ubiquiti (für Tests per UNIFI_CLOUD_BASE änderbar).
var unifiCloudBase = func() string {
	if b := strings.TrimRight(strings.TrimSpace(os.Getenv("UNIFI_CLOUD_BASE")), "/"); b != "" {
		return b
	}
	return "https://api.ui.com"
}()

var unifiCloudClient = &http.Client{Timeout: 30 * time.Second} // normale Zertifikatsprüfung

// newUniFiCloudAPI: Zugriff auf eine Konsole über den Cloud-Proxy (ohne Agent vor Ort).
func newUniFiCloudAPI(consoleID, key string) *unifiAPI {
	return &unifiAPI{base: unifiCloudBase + "/v1/connector/consoles/" + url.PathEscape(strings.TrimSpace(consoleID)),
		key: strings.TrimSpace(key), cl: unifiCloudClient, cloud: true, noPing: true}
}

// raw: GET mit API-Schlüssel; bei 429 (zu viele Anfragen) einmal nach Retry-After (max. 10 s) wiederholen.
func (u *unifiAPI) raw(path string) (int, []byte, error) {
	for try := 0; ; try++ {
		req, _ := http.NewRequest("GET", u.base+path, nil)
		req.Header.Set("X-API-KEY", u.key)
		req.Header.Set("Accept", "application/json")
		resp, err := u.cl.Do(req)
		if err != nil {
			if u.cloud {
				return 0, nil, fmt.Errorf("UniFi-Cloud nicht erreichbar: %v", shortErr(err))
			}
			return 0, nil, fmt.Errorf("UniFi-Konsole nicht erreichbar: %v", shortErr(err))
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 {
			ra := retryAfter(resp.Header.Get("Retry-After"))
			if try == 0 && ra <= 10*time.Second {
				time.Sleep(ra)
				continue
			}
			return resp.StatusCode, b, &unifiRateErr{retry: ra}
		}
		return resp.StatusCode, b, nil
	}
}

// unifiRateErr: Ubiquiti drosselt (HTTP 429) – der Server-Poller setzt dann aus.
type unifiRateErr struct{ retry time.Duration }

func (e *unifiRateErr) Error() string {
	return "UniFi-Cloud: zu viele Anfragen (HTTP 429) – neuer Versuch in " + fmtDurShort(e.retry.Milliseconds())
}

func retryAfter(h string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 5 * time.Second
}

// httpErr: verständliche Meldung zu einem HTTP-Status (lokal bzw. über die Cloud).
func (u *unifiAPI) httpErr(code int) error {
	switch {
	case code == 401:
		if u.cloud {
			return errors.New("UniFi-Cloud-Key ungültig (HTTP 401) – bitte in unifi.ui.com einen neuen API-Key erstellen")
		}
		return errors.New("API-Schlüssel abgelehnt")
	case code == 403:
		if u.cloud {
			return errors.New("Keine Rechte für diese Konsole (HTTP 403) – der Key gehört zu einem Konto ohne Zugriff darauf")
		}
		return errors.New("API-Schlüssel abgelehnt")
	case code == 404:
		if u.cloud {
			return errors.New("Konsole über die UniFi-Cloud nicht erreichbar (HTTP 404) – nötig sind Konsole ab Firmware 5.0.3 und UniFi Network ab 9.3")
		}
		return errors.New("Integration-API nicht gefunden – bitte UniFi Network auf die aktuelle Version bringen")
	case code == 429:
		return errors.New("UniFi-Cloud: zu viele Anfragen (HTTP 429) – bitte später erneut versuchen")
	case code == 502 || code == 503 || code == 504:
		if u.cloud {
			return fmt.Errorf("Konsole antwortet nicht über die UniFi-Cloud (HTTP %d) – offline oder ohne Internet?", code)
		}
	}
	return fmt.Errorf("UniFi: HTTP %d", code)
}

func (u *unifiAPI) get(path string, v any) error {
	code, b, err := u.raw(path)
	if err != nil {
		return err
	}
	if code >= 300 {
		return u.httpErr(code)
	}
	return json.Unmarshal(b, v)
}

const unifiIntAPI = "/proxy/network/integration/v1"

func fnum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

// unifiFetch holt die Geräte; all = auch Geräte ohne IP plus Statistiken, Ping und WAN (Livestatus).
func unifiFetch(base, key, siteFilter string, all bool) ([]unifiDevice, error) {
	return newUniFiAPI(base, key).fetch(siteFilter, all)
}

func (u *unifiAPI) fetch(siteFilter string, all bool) ([]unifiDevice, error) {
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
	out := []unifiDevice{}
	for _, st := range sites.Data {
		if siteFilter != "" && !strings.EqualFold(siteFilter, st.Name) && siteFilter != st.ID && siteFilter != st.InternalReference {
			continue
		}
		var devs []unifiDevice
		for offset := 0; offset < 5000; offset += 200 {
			var page struct {
				Data []struct {
					ID              string   `json:"id"`
					Name            string   `json:"name"`
					Model           string   `json:"model"`
					MacAddress      string   `json:"macAddress"`
					IPAddress       string   `json:"ipAddress"`
					State           string   `json:"state"`
					Features        []string `json:"features"`
					FirmwareVersion string   `json:"firmwareVersion"`
				} `json:"data"`
				TotalCount int `json:"totalCount"`
			}
			if err := u.get(fmt.Sprintf("%s/sites/%s/devices?offset=%d&limit=200", unifiIntAPI, st.ID, offset), &page); err != nil {
				return nil, err
			}
			for _, d := range page.Data {
				if d.IPAddress == "" && !all {
					continue
				}
				devs = append(devs, unifiDevice{ID: d.ID, Name: d.Name, IP: d.IPAddress, MAC: strings.ToLower(d.MacAddress), Model: d.Model,
					State: d.State, Site: st.Name, Features: d.Features, Firmware: d.FirmwareVersion})
			}
			if len(page.Data) < 200 || offset+200 >= page.TotalCount {
				break
			}
		}
		if all {
			u.enrich(st.ID, devs)
			legacy := st.InternalReference
			if legacy == "" {
				legacy = "default"
			}
			u.wans(legacy, devs)
		}
		out = append(out, devs...)
	}
	return out, nil
}

// enrich: Statistiken je Gerät (Laufzeit, CPU, RAM, Uplink) und eigener Ping vom Agenten.
func (u *unifiAPI) enrich(siteID string, devs []unifiDevice) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range devs {
		d := &devs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if d.ID != "" && strings.EqualFold(d.State, "ONLINE") {
				var st map[string]any
				if u.get(fmt.Sprintf("%s/sites/%s/devices/%s/statistics/latest", unifiIntAPI, siteID, d.ID), &st) == nil {
					d.Uptime = int64(fnum(st["uptimeSec"]))
					d.CPU = fnum(st["cpuUtilizationPct"])
					d.Mem = fnum(st["memoryUtilizationPct"])
					if up, ok := st["uplink"].(map[string]any); ok {
						d.Tx, d.Rx = fnum(up["txRateBps"]), fnum(up["rxRateBps"])
					}
				}
			}
			if !u.noPing {
				d.Ping = pingDevice(d.IP)
			}
		}()
	}
	wg.Wait()
}

// pingDevice: 3 Pings im Abstand von 1 s (nur private Adressen).
func pingDevice(addr string) *unifiPing {
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() {
		return nil
	}
	p := &unifiPing{}
	var sum float64
	for k := 0; k < 3; k++ {
		if k > 0 {
			time.Sleep(time.Second)
		}
		p.N++
		var r int32
		var err error
		if simMode {
			r = simPing(addr)
			if r < 0 {
				err = errTimeout
			}
		} else {
			r, err = icmpPing(ip, time.Second)
		}
		if err != nil {
			p.Lost++
			continue
		}
		ms := float64(r) / 10
		sum += ms
		if ms > p.Max {
			p.Max = ms
		}
	}
	if g := p.N - p.Lost; g > 0 {
		p.Avg = sum / float64(g)
	}
	return p
}

// wans liest den WAN-Zustand des Gateways über die klassische Network-Schnittstelle (falls der Schlüssel dafür gilt).
func (u *unifiAPI) wans(site string, devs []unifiDevice) {
	for i := range devs {
		d := &devs[i]
		if !isGateway(*d) || d.MAC == "" {
			continue
		}
		code, b, err := u.raw("/proxy/network/api/s/" + site + "/stat/device/" + d.MAC)
		if err != nil || code != 200 {
			continue
		}
		var res struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(b, &res) != nil || len(res.Data) == 0 {
			continue
		}
		d.WAN = parseWANs(res.Data[0])
	}
}

func parseWANs(g map[string]any) []unifiWAN {
	stats, _ := g["uptime_stats"].(map[string]any)
	var out []unifiWAN
	for _, k := range []string{"wan1", "wan2", "wan3", "wan4"} {
		w, ok := g[k].(map[string]any)
		if !ok {
			continue
		}
		up, _ := w["up"].(bool)
		ip, _ := w["ip"].(string)
		name := "WAN"
		if k != "wan1" {
			name = "WAN" + k[3:]
		}
		x := unifiWAN{Name: name, Up: up, IP: ip}
		s, hasStats := stats[strings.ToUpper(strings.Replace(name, "WAN1", "WAN", 1))].(map[string]any)
		if hasStats {
			x.Latency = fnum(s["latency_average"])
			x.Avail = fnum(s["availability"])
		}
		if !up && ip == "" && !hasStats {
			continue // Anschluss nicht eingerichtet
		}
		if en, ok := w["enable"].(bool); ok && !en {
			continue // nicht konfigurierte Leitung
		}
		out = append(out, x)
	}
	if aw, ok := g["active_wan"].(string); ok {
		for i := range out {
			out[i].Active = strings.EqualFold(strings.ReplaceAll(out[i].Name, "WAN", "wan"), aw) ||
				(out[i].Name == "WAN" && strings.EqualFold(aw, "wan1"))
		}
	}
	return out
}

func isGateway(d unifiDevice) bool {
	for _, f := range d.Features {
		if strings.EqualFold(f, "gateway") || strings.EqualFold(f, "routing") {
			return true
		}
	}
	m := strings.ToUpper(d.Model)
	for _, p := range []string{"UDM", "UDR", "UXG", "UCG", "USG", "UDW", "UX7", "UCK"} {
		if strings.HasPrefix(m, p) && p != "UCK" {
			return true
		}
	}
	return false
}

// unifiCategory ordnet ein Gerät für die Anzeige ein.
func unifiCategory(d unifiDevice) string {
	if isGateway(d) {
		return "gateway"
	}
	has := func(x string) bool {
		for _, f := range d.Features {
			if strings.EqualFold(f, x) {
				return true
			}
		}
		return false
	}
	if has("accessPoint") { // APs mit LAN-Ports melden zusätzlich „switching“
		return "ap"
	}
	if has("switching") {
		return "switch"
	}
	m := strings.ToUpper(d.Model)
	switch {
	case strings.HasPrefix(m, "USW") || strings.HasPrefix(m, "US-") || strings.HasPrefix(m, "US "):
		return "switch"
	case strings.HasPrefix(m, "U6") || strings.HasPrefix(m, "U7") || strings.HasPrefix(m, "UAP") || strings.HasPrefix(m, "UAL") ||
		strings.HasPrefix(m, "E7") || strings.HasPrefix(m, "UWB") || strings.HasPrefix(m, "UBB"):
		return "ap"
	}
	return "other"
}

// unifiProbe (Diagnose): liefert Status und Anfang der Antwort für die angegebenen Pfade.
func unifiProbe(base, key string, paths []string) (map[string]string, error) {
	u := newUniFiAPI(base, key)
	out := map[string]string{}
	sort.Strings(paths)
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/proxy/network/") {
			continue
		}
		code, b, err := u.raw(p)
		if err != nil {
			out[p] = err.Error()
			continue
		}
		s := string(b)
		if len(s) > 3000 {
			s = s[:3000] + "…"
		}
		out[p] = fmt.Sprintf("HTTP %d: %s", code, s)
	}
	return out, nil
}

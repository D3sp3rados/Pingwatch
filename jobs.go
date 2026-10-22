package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Aufträge an Agenten (Netzwerk-Scan, UniFi-Import). Ablauf:
// Dashboard → POST /api/jobs → Warteschlange je Standort → Agent holt sie mit der nächsten
// Messwert-Übertragung ab → Agent schickt das Ergebnis an /api/agent/job → Dashboard fragt /api/jobs?id= ab.

type agentJob struct {
	ID     string            `json:"id"`
	Kind   string            `json:"kind"` // scan, unifi
	Params map[string]string `json:"params"`
}

type jobState struct {
	agentJob
	Org     string          `json:"-"`
	Site    string          `json:"site"`
	Status  string          `json:"status"` // queued, running, done, error
	Error   string          `json:"error,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Created int64           `json:"created"`
}

var jobs = struct {
	sync.Mutex
	byID  map[string]*jobState
	queue map[string][]agentJob
}{byID: map[string]*jobState{}, queue: map[string][]agentJob{}}

func takeJobs(site string) []agentJob {
	jobs.Lock()
	defer jobs.Unlock()
	q := jobs.queue[site]
	delete(jobs.queue, site)
	for _, j := range q {
		if st := jobs.byID[j.ID]; st != nil {
			st.Status = "running"
		}
	}
	// alte Aufträge aufräumen (Updates/Neustarts warten länger auf ausgeschaltete Rechner)
	cut := time.Now().Add(-30 * time.Minute).UnixMilli()
	cutA := time.Now().Add(-actionJobTTL).UnixMilli()
	for id, st := range jobs.byID {
		if st.Created < cutA || (st.Created < cut && actionKinds[st.Kind] == "") {
			delete(jobs.byID, id)
		}
	}
	for k, list := range jobs.queue {
		keep := list[:0]
		for _, j := range list {
			if jobs.byID[j.ID] != nil {
				keep = append(keep, j)
			}
		}
		if len(keep) == 0 {
			delete(jobs.queue, k)
		} else {
			jobs.queue[k] = keep
		}
	}
	return q
}

// POST /api/jobs {site, kind, params} · GET /api/jobs?id=
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if r.Method == http.MethodGet {
		jobs.Lock()
		st := jobs.byID[r.URL.Query().Get("id")]
		var cp jobState
		if st != nil {
			cp = *st
		}
		jobs.Unlock()
		if st == nil || cp.Org != p.Org {
			http.Error(w, "Auftrag unbekannt", 404)
			return
		}
		cp.Params = nil // UniFi-Schlüssel nicht zurückgeben
		writeJSON(w, cp)
		return
	}
	var req struct {
		Site   string            `json:"site"`
		Kind   string            `json:"kind"`
		Params map[string]string `json:"params"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Kind != "scan" && req.Kind != "unifi" && req.Kind != "unifiprobe" && req.Kind != "update" && req.Kind != "inventory" {
		http.Error(w, "Auftrag unbekannt", 400)
		return
	}
	if req.Kind == "inventory" {
		// Inventur eines Rechners: an den Agenten dieses Rechners (Standort-Agent oder „nur Rechner“)
		mc, ok := s.m.machineByID(req.Params["machine"])
		if !ok || mc.Org != p.Org {
			http.Error(w, "Rechner unbekannt", 404)
			return
		}
		if !s.m.machineOnline(mc) {
			http.Error(w, "Der Rechner meldet sich gerade nicht", 409)
			return
		}
		j := agentJob{ID: randHex(8), Kind: "inventory", Params: map[string]string{"machine": mc.ID}}
		q := mc.Site
		if mc.SysOnly {
			q = "m:" + mc.ID
		}
		jobs.Lock()
		jobs.byID[j.ID] = &jobState{agentJob: j, Org: p.Org, Site: mc.Site, Status: "queued", Created: time.Now().UnixMilli()}
		jobs.queue[q] = append(jobs.queue[q], j)
		jobs.Unlock()
		writeJSON(w, map[string]string{"id": j.ID})
		return
	}
	site := s.siteFor(r, req.Site)
	if site == nil {
		http.Error(w, "Standort unbekannt", 404)
		return
	}
	if on, _ := s.m.siteOnline(site.ID); !on {
		http.Error(w, "Der Agent an diesem Standort ist gerade offline", 409)
		return
	}
	if req.Params == nil {
		req.Params = map[string]string{}
	}
	if req.Kind == "scan" {
		var list []string
		for _, c := range strings.FieldsFunc(req.Params["cidr"], func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
			if _, n, err := net.ParseCIDR(c); err != nil || !isPrivateNet(n) {
				http.Error(w, "Bitte private Netze angeben, z. B. 192.168.1.0/24 (mehrere mit Komma)", 400)
				return
			} else if ones, bits := n.Mask.Size(); bits-ones > 10 {
				http.Error(w, "Netz "+c+" zu groß – höchstens /22 (1024 Adressen)", 400)
				return
			}
			list = append(list, c)
		}
		_, sst := s.m.siteOnline(site.ID)
		agentVer := ""
		if sst != nil {
			agentVer = sst.version
		}
		multiOK := !newerVersion("1.5.2", agentVer)
		if len(list) > 1 && !multiOK {
			http.Error(w, "Mehrere Netze auf einmal: bitte den Agenten auf Version 1.5.2 aktualisieren (Standorte → Jetzt aktualisieren)", 400)
			return
		}
		if len(list) > maxScanNets {
			http.Error(w, fmt.Sprintf("Höchstens %d Netze auf einmal", maxScanNets), 400)
			return
		}
		params := map[string]string{"cidr": strings.Join(list, ",")}
		if req.Params["vlans"] == "1" {
			u := site.UniFi
			switch {
			case u == nil || !u.On:
				http.Error(w, "Für „Alle VLANs aus UniFi“ bitte zuerst den UniFi-Livestatus am Standort einrichten", 400)
				return
			case !multiOK:
				http.Error(w, "Für „Alle VLANs aus UniFi“ bitte den Agenten auf Version 1.5.2 aktualisieren (Standorte → Jetzt aktualisieren)", 400)
				return
			case u.isCloud():
				key := s.m.unifiCloudKey(*site)
				if key == "" || u.Console == "" {
					http.Error(w, "UniFi-Cloud-Key oder Konsole fehlt (UniFi-Livestatus)", 400)
					return
				}
				nets, err := newUniFiCloudAPI(u.Console, key).networks(u.Site)
				if err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				params["cidr"] = strings.Join(normScanNets(append(list, nets...)), ",")
				params["own"] = "1" // zusätzlich die eigenen Netze des Agenten
			default:
				params["vlans"], params["url"], params["key"], params["usite"], params["own"] = "1", u.URL, u.Key, u.Site, "1"
			}
		}
		req.Params = params
	} else if req.Kind == "update" {
		req.Params = map[string]string{"version": version}
	} else if req.Kind == "unifiprobe" {
		// Diagnose: nutzt die gespeicherte Konfiguration des Standorts
		if site.UniFi == nil || site.UniFi.Key == "" || !principal(r).Super {
			http.Error(w, "UniFi-Livestatus ist für diesen Standort nicht eingerichtet", 400)
			return
		}
		req.Params["url"], req.Params["key"] = site.UniFi.URL, site.UniFi.Key
	} else {
		if !strings.HasPrefix(req.Params["url"], "https://") || req.Params["key"] == "" {
			http.Error(w, "Adresse der UniFi-Konsole (https://…) und API-Schlüssel angeben", 400)
			return
		}
	}
	j := agentJob{ID: randHex(8), Kind: req.Kind, Params: req.Params}
	jobs.Lock()
	jobs.byID[j.ID] = &jobState{agentJob: j, Org: p.Org, Site: site.ID, Status: "queued", Created: time.Now().UnixMilli()}
	jobs.queue[site.ID] = append(jobs.queue[site.ID], j)
	jobs.Unlock()
	writeJSON(w, map[string]string{"id": j.ID})
}

// POST /api/agent/job {id, error, result}
func (s *Server) handleAgentJob(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		ID     string          `json:"id"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	jobs.Lock()
	st := jobs.byID[req.ID]
	kind := ""
	if st != nil {
		kind = st.Kind
	}
	jobs.Unlock()
	if st != nil && st.Site == site.ID && kind == "scan" && req.Error == "" {
		var sr scanResult
		if json.Unmarshal(req.Result, &sr) == nil {
			nameScanHits(&sr, s.m.unifiScanNames(site))
			if b, err := json.Marshal(sr); err == nil {
				req.Result = b
			}
		}
	}
	jobs.Lock()
	defer jobs.Unlock()
	if st == nil || st.Site != site.ID {
		http.Error(w, "Auftrag unbekannt", 404)
		return
	}
	st.Params = nil
	if req.Error != "" {
		st.Status, st.Error = "error", req.Error
	} else {
		st.Status, st.Result = "done", req.Result
	}
	if actionKinds[st.Kind] != "" {
		go s.m.actionJobDone(st.ID, st.Kind, req.Error, req.Result)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func isPrivateNet(n *net.IPNet) bool {
	ip := n.IP.To4()
	return ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// ---------- Agent-Seite ----------

func (a *Agent) runJob(j agentJob) {
	var res any
	var err error
	switch j.Kind {
	case "scan":
		cidrs := j.Params["cidr"]
		var names map[string]string
		if j.Params["vlans"] == "1" && j.Params["url"] != "" {
			// UniFi-Konsole im lokalen Netz: Netze und Client-Namen direkt abfragen
			u := newUniFiAPI(j.Params["url"], j.Params["key"])
			nets, e := u.networks(j.Params["usite"])
			if e != nil {
				err = e
				break
			}
			cidrs = strings.Join(normScanNets(append(strings.Split(cidrs, ","), nets...)), ",")
			names = u.clientNames(j.Params["usite"])
		}
		var sr *scanResult
		sr, err = scanNetwork(cidrs, j.Params["own"] == "1")
		if err == nil {
			nameScanHits(sr, names)
			res = sr
		}
	case "unifi":
		res, err = unifiDevices(j.Params["url"], j.Params["key"], j.Params["site"])
	case "unifiprobe":
		res, err = unifiProbe(j.Params["url"], j.Params["key"], strings.Split(j.Params["paths"], ","))
	case "winupdate", "reboot":
		if m := j.Params["machine"]; m == "" || !strings.HasSuffix(m, ":"+a.machine) {
			err = errors.New("Auftrag gilt für einen anderen Rechner")
			break
		}
		a.mu.Lock()
		allowed := a.sysActions
		a.mu.Unlock()
		switch {
		case a.file.NoActions:
			err = errors.New("auf diesem Rechner abgeschaltet (\"noActions\" in der agent.json)")
		case !allowed:
			err = errors.New("für diesen Standort nicht erlaubt")
		case j.Kind == "reboot":
			if err = rebootMachine(60); err == nil {
				res = actionResult{Message: "Neustart in 1 Minute"}
			}
		default:
			res, err = installUpdates(j.Params["cat"], j.Params["reboot"])
		}
	case "inventory":
		if m := j.Params["machine"]; m != "" && !strings.HasSuffix(m, ":"+a.machine) {
			err = errors.New("Auftrag gilt für einen anderen Rechner")
			break
		}
		if err = a.sendInventory(a.machine); err == nil {
			res = map[string]string{"message": "Inventur übertragen"}
		}
	case "update":
		a.mu.Lock()
		latest := a.latest
		a.mu.Unlock()
		if v := j.Params["version"]; v != "" {
			latest = v
		}
		if !newerVersion(latest, version) {
			res = map[string]string{"message": "Agent ist bereits aktuell (" + version + ")"}
			break
		}
		// erst antworten, dann aktualisieren (der Agent startet dabei neu)
		defer func() { go a.selfUpdate(latest, true) }()
		res = map[string]string{"message": "Update von " + version + " auf " + latest + " gestartet"}
	default:
		err = errors.New("unbekannter Auftrag – Agent aktualisieren")
	}
	out := map[string]any{"id": j.ID}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["result"] = res
	}
	b, _ := json.Marshal(out)
	for try := 0; try < 3; try++ {
		resp, e := a.req("POST", "/api/agent/job", bytes.NewReader(b), false)
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(5 * time.Second)
	}
}

type scanHit struct {
	IP   string `json:"ip"`
	RTT  int32  `json:"rtt"` // Zehntel-ms
	Name string `json:"name,omitempty"`
	MAC  string `json:"mac,omitempty"`
	Via  string `json:"via"` // icmp oder tcp/<port>
}

type scanResult struct {
	Nets []string  `json:"nets"`
	Hits []scanHit `json:"hits"`
}

// localNets: eigene private IPv4-Netze des Agenten (höchstens /22, sonst auf /24 um die eigene Adresse begrenzt).
func localNets() []*net.IPNet {
	var out []*net.IPNet
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if ones < 22 {
				ones = 24
			}
			n := &net.IPNet{IP: ipn.IP.Mask(net.CIDRMask(ones, 32)), Mask: net.CIDRMask(ones, 32)}
			out = append(out, n)
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

func hostsIn(n *net.IPNet) []net.IP {
	ip := binary.BigEndian.Uint32(n.IP.To4())
	ones, bits := n.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	var out []net.IP
	for i := uint32(1); i < size-1 || (size <= 2 && i < size); i++ {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, ip+i)
		out = append(out, net.IP(b))
	}
	return out
}

// scanNetwork: angegebene Netze (Komma-getrennt), ohne Angabe bzw. mit own die eigenen Netze des Agenten.
func scanNetwork(cidr string, own bool) (*scanResult, error) {
	var nets []*net.IPNet
	seen := map[string]bool{}
	for _, c := range strings.Split(cidr, ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		if !seen[n.String()] {
			seen[n.String()] = true
			nets = append(nets, n)
		}
	}
	if len(nets) == 0 || own {
		for _, n := range localNets() {
			if !seen[n.String()] {
				seen[n.String()] = true
				nets = append(nets, n)
			}
		}
	}
	if len(nets) == 0 && simMode {
		_, n, _ := net.ParseCIDR("192.168.178.0/24")
		nets = append(nets, n)
	}
	if len(nets) == 0 {
		return nil, errors.New("kein privates Netz am Agenten gefunden – bitte Netz angeben, z. B. 192.168.1.0/24")
	}
	res := &scanResult{}
	var ips []net.IP
	for _, n := range nets {
		res.Nets = append(res.Nets, n.String())
		ips = append(ips, hostsIn(n)...)
	}
	if len(ips) > 4096 {
		ips = ips[:4096]
	}
	var mu sync.Mutex
	par := 64
	if len(ips) > 1024 {
		par = 256 // viele VLANs: sonst dauert der Scan zu lange
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for _, ip := range ips {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip net.IP) {
			defer wg.Done()
			defer func() { <-sem }()
			hit, ok := scanOne(ip)
			if ok {
				mu.Lock()
				res.Hits = append(res.Hits, hit)
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	arp := arpTable()
	var nwg sync.WaitGroup
	for i := range res.Hits {
		res.Hits[i].MAC = arp[res.Hits[i].IP]
		nwg.Add(1)
		go func(h *scanHit) {
			defer nwg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if names, err := net.DefaultResolver.LookupAddr(ctx, h.IP); err == nil && len(names) > 0 {
				h.Name = strings.TrimSuffix(names[0], ".")
			}
		}(&res.Hits[i])
	}
	nwg.Wait()
	sort.Slice(res.Hits, func(i, j int) bool {
		return binary.BigEndian.Uint32(net.ParseIP(res.Hits[i].IP).To4()) < binary.BigEndian.Uint32(net.ParseIP(res.Hits[j].IP).To4())
	})
	return res, nil
}

func scanOne(ip net.IP) (scanHit, bool) {
	if simMode {
		if ip[3]%37 == 1 {
			return scanHit{IP: ip.String(), RTT: 15, Via: "icmp"}, true
		}
		return scanHit{}, false
	}
	if r, err := icmpPing(ip, 700*time.Millisecond); err == nil {
		return scanHit{IP: ip.String(), RTT: r, Via: "icmp"}, true
	}
	// Geräte ohne Ping-Antwort: typische Ports
	for _, port := range []int{80, 443, 22, 445, 8080} {
		if r, err := tcpPing(ip, port, 400*time.Millisecond); err == nil {
			return scanHit{IP: ip.String(), RTT: r, Via: fmt.Sprintf("tcp/%d", port)}, true
		}
	}
	return scanHit{}, false
}

// arpTable liest die ARP-Tabelle (IP → MAC) des Systems.
func arpTable() map[string]string {
	out := map[string]string{}
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/net/arp")
		if err == nil {
			for _, l := range strings.Split(string(b), "\n")[1:] {
				f := strings.Fields(l)
				if len(f) >= 4 && f[3] != "00:00:00:00:00:00" {
					out[f[0]] = strings.ToLower(f[3])
				}
			}
		}
		return out
	}
	cmd := exec.Command("arp", "-a")
	hideWindow(cmd)
	b, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			ip := strings.Trim(f[i], "()")
			if net.ParseIP(ip) != nil {
				mac := strings.ToLower(strings.ReplaceAll(f[i+1], "-", ":"))
				if mac == "at" && i+2 < len(f) {
					mac = strings.ToLower(f[i+2])
				}
				if strings.Count(mac, ":") == 5 && mac != "ff:ff:ff:ff:ff:ff" {
					out[ip] = mac
				}
				break
			}
		}
	}
	return out
}

// ---------- UniFi (Network-Anwendung, Integration-API ab Network 9) ----------

// unifiBase kann in Tests überschrieben werden.
func unifiDevices(base, key, siteFilter string) ([]unifiDevice, error) {
	return unifiFetch(base, key, siteFilter, false)
}

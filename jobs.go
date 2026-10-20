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
	// alte Aufträge aufräumen
	cut := time.Now().Add(-30 * time.Minute).UnixMilli()
	for id, st := range jobs.byID {
		if st.Created < cut {
			delete(jobs.byID, id)
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
	if req.Kind != "scan" && req.Kind != "unifi" && req.Kind != "unifiprobe" {
		http.Error(w, "Auftrag unbekannt", 400)
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
		if c := strings.TrimSpace(req.Params["cidr"]); c != "" {
			if _, n, err := net.ParseCIDR(c); err != nil || !isPrivateNet(n) {
				http.Error(w, "Bitte ein privates Netz angeben, z. B. 192.168.1.0/24", 400)
				return
			} else if ones, bits := n.Mask.Size(); bits-ones > 10 {
				http.Error(w, "Netz zu groß – höchstens /22 (1024 Adressen)", 400)
				return
			}
		}
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
	defer jobs.Unlock()
	st := jobs.byID[req.ID]
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
		res, err = scanNetwork(j.Params["cidr"])
	case "unifi":
		res, err = unifiDevices(j.Params["url"], j.Params["key"], j.Params["site"])
	case "unifiprobe":
		res, err = unifiProbe(j.Params["url"], j.Params["key"], strings.Split(j.Params["paths"], ","))
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

func scanNetwork(cidr string) (*scanResult, error) {
	var nets []*net.IPNet
	if c := strings.TrimSpace(cidr); c != "" {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		nets = append(nets, n)
	} else {
		nets = localNets()
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
	sem := make(chan struct{}, 64)
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


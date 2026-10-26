package main

// MikroTik (RouterOS v7) über die REST-API auslesen – durch den Agenten vor Ort, mit einem
// Read-only-Benutzer. Jede Minute: Zustand (CPU, RAM, Temperatur, Netzteile, Lüfter), Ports
// (Traffic, Link, SFP-Pegel), BGP/OSPF, PPP-/DHCP-Sitzungen, Verbindungen, WireGuard, Log.
// Die Ports laufen in dieselbe Schnittstellen-Statistik wie bei SNMP (ingestIfs).

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- Daten vom Agenten ----------

type mtKV struct {
	Name  string `json:"n"`
	Value string `json:"v"`
	Type  string `json:"t,omitempty"`
}

type mtIf struct {
	Name      string  `json:"n"`
	Type      string  `json:"type,omitempty"`
	Comment   string  `json:"c,omitempty"`
	Running   bool    `json:"up"`
	Disabled  bool    `json:"dis,omitempty"`
	Rate      string  `json:"rate,omitempty"` // z. B. 10Gbps
	LinkDowns int64   `json:"ld,omitempty"`
	SfpRx     float64 `json:"rx,omitempty"` // dBm
	SfpTx     float64 `json:"tx,omitempty"`
	SfpTemp   float64 `json:"st,omitempty"`
	Sfp       bool    `json:"sfp,omitempty"`
}

type mtBGP struct {
	Name     string `json:"n"`
	Remote   string `json:"r,omitempty"`
	AS       string `json:"as,omitempty"`
	Up       bool   `json:"up"`
	Uptime   int64  `json:"ut,omitempty"` // s
	Prefixes int64  `json:"p,omitempty"`
}

type mtWG struct {
	Name     string `json:"n"`
	Iface    string `json:"i,omitempty"`
	Endpoint string `json:"e,omitempty"`
	LastHS   int64  `json:"hs"` // s seit letztem Handshake, -1 = nie
}

type mtLog struct {
	T     string `json:"t"`
	Topic string `json:"topic"`
	Msg   string `json:"msg"`
}

type mtSnap struct {
	T      int64             `json:"t"`
	Err    string            `json:"err,omitempty"`
	Res    map[string]string `json:"res,omitempty"`
	RB     map[string]string `json:"rb,omitempty"`
	Upd    map[string]string `json:"upd,omitempty"`
	Health []mtKV            `json:"health,omitempty"`
	Ifs    []ifSample        `json:"ifs,omitempty"`
	IfX    []mtIf            `json:"ifx,omitempty"`
	BGP    []mtBGP           `json:"bgp,omitempty"`
	OSPF   []mtKV            `json:"ospf,omitempty"`
	PPP    int               `json:"ppp"`   // -1 = nicht abfragbar
	DHCP   int               `json:"dhcp"`  // gebundene Leases, -1 = nicht abfragbar
	Conns  int64             `json:"conns"` // -1 = nicht abfragbar
	WG     []mtWG            `json:"wg,omitempty"`
	Log    []mtLog           `json:"log,omitempty"`
	Miss   []string          `json:"miss,omitempty"` // nicht abfragbare Bereiche
}

// ---------- Agent: REST-Client ----------

type mtClient struct {
	base, user, pass string
	hc               *http.Client
}

func newMtClient(addr string, port int, user, pass string) *mtClient {
	scheme := "https"
	if port == 80 {
		scheme = "http"
	}
	host := strings.TrimSpace(addr)
	if port != 0 && port != 443 && port != 80 {
		host += ":" + strconv.Itoa(port)
	}
	return &mtClient{base: scheme + "://" + host + "/rest", user: user, pass: pass, hc: &http.Client{
		Timeout: 15 * time.Second,
		// RouterOS nutzt meist ein selbst ausgestelltes Zertifikat (internes Netz)
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
}

func (c *mtClient) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 {
		return fmt.Errorf("Anmeldung abgelehnt (Benutzer/Passwort oder erlaubte Adresse prüfen)")
	}
	if resp.StatusCode >= 300 {
		var e struct{ Message, Detail string }
		_ = json.Unmarshal(b, &e)
		msg := strings.TrimSpace(e.Message + " " + e.Detail)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return json.Unmarshal(b, out)
}

func (c *mtClient) list(path string) ([]map[string]any, error) {
	var raw json.RawMessage
	if err := c.do("GET", path, nil, &raw); err != nil {
		return nil, err
	}
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) == nil {
		return arr, nil
	}
	var one map[string]any
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	return []map[string]any{one}, nil
}

func mtStr(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func mtNum(m map[string]any, k string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(mtStr(m, k)), 64)
	return f
}

func mtU64(m map[string]any, k string) uint64 {
	u, _ := strconv.ParseUint(strings.TrimSpace(mtStr(m, k)), 10, 64)
	return u
}

func mtBool(m map[string]any, k string) bool { return mtStr(m, k) == "true" || mtStr(m, k) == "yes" }

var mtDurRe = regexp.MustCompile(`(\d+)([wdhms])`)

// mtDur: RouterOS-Dauer („3w2d04:05:06“, „1d2h3m4s“, „00:01:02“) → Sekunden; -1 bei leer/„never“.
func mtDur(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "never" {
		return -1
	}
	var t int64
	if i := strings.LastIndex(s, ":"); i > 0 { // hh:mm:ss am Ende
		j := strings.LastIndexAny(s[:i], "wd")
		hms := s[j+1:]
		p := strings.Split(hms, ":")
		if len(p) == 3 {
			h, _ := strconv.Atoi(p[0])
			mi, _ := strconv.Atoi(p[1])
			se, _ := strconv.Atoi(strings.SplitN(p[2], ".", 2)[0])
			t += int64(h*3600 + mi*60 + se)
		}
		s = s[:j+1]
	}
	for _, m := range mtDurRe.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		t += n * map[string]int64{"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}[m[2]]
	}
	return t
}

func mtIdx(id string) int {
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "*"), 16, 64)
	if err != nil {
		return 0
	}
	return int(n)
}

// mtRateMbit: „10Gbps“, „1Gbps“, „100Mbps“, „2.5Gbps“ → Mbit/s
func mtRateMbit(r string) int64 {
	r = strings.ToLower(strings.TrimSpace(r))
	mul := 1.0
	switch {
	case strings.HasSuffix(r, "gbps"):
		mul, r = 1000, strings.TrimSuffix(r, "gbps")
	case strings.HasSuffix(r, "mbps"):
		r = strings.TrimSuffix(r, "mbps")
	default:
		return 0
	}
	f, _ := strconv.ParseFloat(r, 64)
	return int64(f * mul)
}

var mtLogNote = regexp.MustCompile(`(?i)critical|error|changed by|added by|removed by|login failure|link down|failure`)

// mtQuery fragt alles ab. Fehlt einer Rolle die Berechtigung für einen Bereich, wird der übersprungen.
func mtQuery(c *mtClient, st *mtAgentState) mtSnap {
	s := mtSnap{T: time.Now().UnixMilli(), PPP: -1, DHCP: -1, Conns: -1}
	res, err := c.list("/system/resource")
	if err != nil {
		s.Err = err.Error()
		return s
	}
	keep := func(m map[string]any, ks ...string) map[string]string {
		out := map[string]string{}
		for _, k := range ks {
			if v := mtStr(m, k); v != "" {
				out[k] = v
			}
		}
		return out
	}
	if len(res) > 0 {
		s.Res = keep(res[0], "uptime", "version", "build-time", "cpu-load", "cpu-count", "cpu", "free-memory", "total-memory", "free-hdd-space", "total-hdd-space", "board-name", "architecture-name", "platform")
	}
	miss := func(what string, e error) { s.Miss = append(s.Miss, what+": "+e.Error()) }
	if l, e := c.list("/system/routerboard"); e == nil && len(l) > 0 {
		s.RB = keep(l[0], "model", "serial-number", "current-firmware", "upgrade-firmware", "firmware-type")
	}
	if l, e := c.list("/system/package/update"); e == nil && len(l) > 0 {
		s.Upd = keep(l[0], "channel", "installed-version", "latest-version", "status")
	}
	if l, e := c.list("/system/identity"); e == nil && len(l) > 0 {
		if s.Res == nil {
			s.Res = map[string]string{}
		}
		s.Res["identity"] = mtStr(l[0], "name")
	}
	if l, e := c.list("/system/health"); e == nil {
		for _, h := range l {
			if n := mtStr(h, "name"); n != "" { // v7: Liste name/value/type
				s.Health = append(s.Health, mtKV{n, mtStr(h, "value"), mtStr(h, "type")})
				continue
			}
			for k, v := range h { // v6: ein Objekt
				if !strings.HasPrefix(k, ".") {
					s.Health = append(s.Health, mtKV{k, fmt.Sprint(v), ""})
				}
			}
		}
		sort.Slice(s.Health, func(i, j int) bool { return s.Health[i].Name < s.Health[j].Name })
	} else {
		miss("Sensoren", e)
	}
	// Schnittstellen
	ifl, e := c.list("/interface")
	if e != nil {
		miss("Schnittstellen", e)
	}
	vlans := 0
	for _, i := range ifl {
		if mtStr(i, "type") == "vlan" {
			vlans++
		}
	}
	var eth []string
	for _, i := range ifl {
		typ := mtStr(i, "type")
		switch typ {
		case "ether", "bond", "pppoe-out", "lte", "wlan", "wifi", "wg", "sfp", "bridge":
		case "vlan":
			if vlans > 32 {
				continue
			}
		default:
			continue
		}
		name := mtStr(i, "name")
		x := mtIf{Name: name, Type: typ, Comment: mtStr(i, "comment"), Running: mtBool(i, "running"), Disabled: mtBool(i, "disabled"), LinkDowns: int64(mtNum(i, "link-downs"))}
		s.IfX = append(s.IfX, x)
		if !x.Disabled {
			s.Ifs = append(s.Ifs, ifSample{Idx: mtIdx(mtStr(i, ".id")), Name: name, Alias: x.Comment, Up: x.Running,
				In: mtU64(i, "rx-byte"), Out: mtU64(i, "tx-byte"), Err: mtU64(i, "rx-error") + mtU64(i, "tx-error"), HC: true})
		}
		if typ == "ether" && x.Running {
			eth = append(eth, name)
		}
	}
	if len(eth) > 0 {
		var mon []map[string]any
		if e := c.do("POST", "/interface/ethernet/monitor", map[string]any{"numbers": strings.Join(eth, ","), "once": ""}, &mon); e == nil {
			byName := map[string]map[string]any{}
			for _, m := range mon {
				byName[mtStr(m, "name")] = m
			}
			for k := range s.IfX {
				m := byName[s.IfX[k].Name]
				if m == nil {
					continue
				}
				s.IfX[k].Rate = mtStr(m, "rate")
				if v := mtStr(m, "sfp-rx-power"); v != "" {
					s.IfX[k].Sfp = true
					s.IfX[k].SfpRx = mtNum(m, "sfp-rx-power")
					s.IfX[k].SfpTx = mtNum(m, "sfp-tx-power")
					s.IfX[k].SfpTemp = mtNum(m, "sfp-temperature")
				}
			}
			for k := range s.Ifs {
				for _, x := range s.IfX {
					if x.Name == s.Ifs[k].Name {
						s.Ifs[k].Speed = mtRateMbit(x.Rate)
					}
				}
			}
		} else {
			miss("Port-Monitor", e)
		}
	}
	// Routing
	if l, e := c.list("/routing/bgp/session"); e == nil {
		for _, b := range l {
			s.BGP = append(s.BGP, mtBGP{Name: mtStr(b, "name"), Remote: mtStr(b, "remote.address"), AS: mtStr(b, "remote.as"),
				Up: mtBool(b, "established"), Uptime: mtDur(mtStr(b, "uptime")), Prefixes: int64(mtNum(b, "prefix-count"))})
		}
	}
	if l, e := c.list("/routing/ospf/neighbor"); e == nil {
		for _, o := range l {
			s.OSPF = append(s.OSPF, mtKV{mtStr(o, "address"), mtStr(o, "state"), mtStr(o, "instance")})
		}
	}
	if l, e := c.list("/ppp/active?.proplist=.id"); e == nil {
		s.PPP = len(l)
	}
	if l, e := c.list("/ip/dhcp-server/lease?status=bound&.proplist=.id"); e == nil {
		s.DHCP = len(l)
	}
	if l, e := c.list("/ip/firewall/connection/tracking"); e == nil && len(l) > 0 {
		if v := mtStr(l[0], "total-entries"); v != "" {
			s.Conns = int64(mtNum(l[0], "total-entries"))
		}
	}
	if l, e := c.list("/interface/wireguard/peers"); e == nil {
		for _, p := range l {
			if mtBool(p, "disabled") {
				continue
			}
			n := mtStr(p, "comment")
			if n == "" {
				n = mtStr(p, "name")
			}
			if n == "" {
				n = mtStr(p, "allowed-address")
			}
			s.WG = append(s.WG, mtWG{Name: n, Iface: mtStr(p, "interface"), Endpoint: mtStr(p, "current-endpoint-address"), LastHS: mtDur(mtStr(p, "last-handshake"))})
		}
	}
	// Log: nur neue, auffällige Einträge (alle 5 Minuten)
	if time.Since(st.logAt) >= 5*time.Minute {
		st.logAt = time.Now()
		if l, e := c.list("/log?.proplist=.id,time,topics,message"); e == nil {
			maxID := st.logID
			for _, x := range l {
				id := mtIdx(mtStr(x, ".id"))
				if id > maxID {
					maxID = id
				}
				if id <= st.logID && st.logID > 0 {
					continue
				}
				msg, top := mtStr(x, "message"), mtStr(x, "topics")
				if mtLogNote.MatchString(top + " " + msg) {
					s.Log = append(s.Log, mtLog{mtStr(x, "time"), top, msg})
				}
			}
			if st.logID == 0 && len(s.Log) > 30 { // erster Abruf: nur die letzten 30
				s.Log = s.Log[len(s.Log)-30:]
			}
			st.logID = maxID
		} else {
			miss("Log", e)
		}
	}
	return s
}

type mtAgentState struct {
	logID int
	logAt time.Time
}

const mtPollEvery = 60 * time.Second

func (a *Agent) mtLoop(ah *agentHost) {
	c := newMtClient(ah.cfg.Addr, ah.cfg.MtPort, ah.cfg.MtUser, ah.cfg.MtPass)
	st := &mtAgentState{}
	var pend []mtSnap
	for {
		s := mtQuery(c, st)
		select {
		case <-ah.stop:
			return
		default:
		}
		pend = append(pend, s)
		if len(pend) > 60 {
			pend = pend[len(pend)-60:]
		}
		b, _ := json.Marshal(map[string]any{"host": ah.cfg.ID, "snaps": pend})
		if resp, e := a.req("POST", "/api/agent/mikrotik", bytes.NewReader(b), false); e == nil {
			if resp.StatusCode == http.StatusOK {
				pend = nil
			}
			resp.Body.Close()
		} else {
			log.Printf("MikroTik %s senden: %v", ah.cfg.Name, e)
		}
		select {
		case <-ah.stop:
			return
		case <-time.After(mtPollEvery):
		}
	}
}

// ---------- Server ----------

type mtPoint struct {
	T     int64   `json:"t"`
	CPU   float32 `json:"cpu"`
	Mem   float32 `json:"mem"`            // % belegt
	Temp  float32 `json:"temp,omitempty"` // °C (höchster Sensor)
	PPP   int32   `json:"ppp"`
	DHCP  int32   `json:"dhcp"`
	Conns int32   `json:"conns"`
}

type mtHost struct {
	mu      sync.Mutex
	at      int64
	err     string
	cur     *mtSnap
	prevUp  int64
	prevT   int64
	hist    []mtPoint
	boots   []int64
	open    map[string]*Event // offene Probleme (hw:…, bgp:…, cpu, temp, ppp)
	cpuN    int
	tempN   int
	bgpSeen map[string]bool // Sitzungen, die schon einmal aufgebaut waren
	logs    []mtLog         // auffällige Log-Einträge (neueste zuerst)
	pppMax  []int32         // letzte 30 Werte
}

var mtData = struct {
	sync.Mutex
	m map[string]*mtHost
}{m: map[string]*mtHost{}}

func getMtHost(id string) *mtHost {
	mtData.Lock()
	defer mtData.Unlock()
	h := mtData.m[id]
	if h == nil {
		h = &mtHost{open: map[string]*Event{}, bgpSeen: map[string]bool{}}
		mtData.m[id] = h
	}
	return h
}

const (
	mtCPUHigh  = 90
	mtTempHigh = 75
)

func mtTemp(hs []mtKV) (float64, bool) {
	best, ok := 0.0, false
	for _, h := range hs {
		if strings.Contains(h.Name, "temperature") {
			if f, err := strconv.ParseFloat(h.Value, 64); err == nil && f > best {
				best, ok = f, true
			}
		}
	}
	return best, ok
}

// mtHW: Zustände von Netzteilen und Lüftern (Name → ok?)
func mtHW(hs []mtKV) map[string]bool {
	out := map[string]bool{}
	for _, h := range hs {
		n := strings.ToLower(h.Name)
		if strings.HasSuffix(n, "-state") && (strings.HasPrefix(n, "psu") || strings.HasPrefix(n, "fan")) {
			out[strings.TrimSuffix(n, "-state")] = strings.EqualFold(h.Value, "ok")
		}
	}
	return out
}

func (m *Monitor) ingestMikrotik(hc HostCfg, snaps []mtSnap) {
	now := time.Now().UnixMilli()
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].T < snaps[j].T })
	mh := getMtHost(hc.ID)
	h := m.host(hc.ID)
	var notes []notice
	name := hc.Name
	mh.mu.Lock()
	for i := range snaps {
		s := &snaps[i]
		if s.T <= mh.at || s.T > now+60000 || s.T < now-24*3600*1000 {
			continue
		}
		mh.at = s.T
		if s.Err != "" {
			mh.err = s.Err
			continue
		}
		mh.err = ""
		recent := s.T >= now-5*60000
		maint := m.inMaint(hc.Org, hc.ID, time.UnixMilli(s.T))
		open := func(key, typ, label, title, body string) {
			if mh.open[key] != nil {
				return
			}
			e := m.newHostEvent(hc, typ, label, s.T, maint)
			mh.open[key] = e
			if !maint && recent {
				notes = append(notes, notice{"devoff", hc.ID, title, m.withInfo(hc.ID, hc.Site, body), hc.Org, ""})
			} else {
				m.evMu.Lock()
				e.Notified = false
				m.evMu.Unlock()
			}
		}
		closeEv := func(key, title string) {
			e := mh.open[key]
			if e == nil {
				return
			}
			delete(mh.open, key)
			ce := m.closeSiteEvent(e, s.T)
			if ce.Notified && recent {
				notes = append(notes, notice{"devon", hc.ID, title, m.withInfo(hc.ID, hc.Site, "Dauer "+fmtDurShort(ce.End-ce.Start)), hc.Org, ""})
			}
		}
		// Neustart
		up := mtDur(s.Res["uptime"])
		if up >= 0 && mh.prevUp > 0 && mh.prevT > 0 && up+120 < mh.prevUp+(s.T-mh.prevT)/1000 {
			at := s.T - up*1000
			mh.boots = append(mh.boots, at)
			m.closeSiteEvent(m.newHostEvent(hc, "reboot", name+" (MikroTik)", at, maint), at)
			if !maint && recent {
				notes = append(notes, notice{"reboot", hc.ID, "🔄 MikroTik neu gestartet: " + name,
					m.withInfo(hc.ID, hc.Site, "Neustart um "+time.UnixMilli(at).Format("15:04")), hc.Org, ""})
			}
		}
		if up >= 0 {
			mh.prevUp, mh.prevT = up, s.T
		}
		// Netzteile, Lüfter
		for part, ok := range mtHW(s.Health) {
			key := "hw:" + part
			label := strings.ToUpper(part)
			if strings.HasPrefix(part, "psu") {
				label = "Netzteil " + strings.TrimPrefix(part, "psu")
			} else if strings.HasPrefix(part, "fan") {
				label = "Lüfter " + strings.TrimPrefix(part, "fan")
			}
			if !ok {
				open(key, "hw", name+" · "+label, "🔴 "+label+" gestört: "+name, label+" meldet nicht „ok“")
			} else {
				closeEv(key, "🟢 "+label+" wieder ok: "+name)
			}
		}
		// CPU dauerhaft hoch
		cpu := mtNum(map[string]any{"x": s.Res["cpu-load"]}, "x")
		if cpu >= mtCPUHigh {
			mh.cpuN++
		} else if cpu < mtCPUHigh-15 {
			mh.cpuN = 0
			closeEv("cpu", "🟢 CPU wieder normal: "+name)
		}
		if mh.cpuN >= 5 {
			open("cpu", "cpu", name+" · CPU", "🔴 CPU dauerhaft hoch: "+name, fmt.Sprintf("CPU-Last seit 5 Minuten über %d %% (aktuell %.0f %%)", mtCPUHigh, cpu))
		}
		// Temperatur
		temp, hasT := mtTemp(s.Health)
		if hasT && temp >= mtTempHigh {
			mh.tempN++
		} else if hasT && temp < mtTempHigh-5 {
			mh.tempN = 0
			closeEv("temp", "🟢 Temperatur wieder normal: "+name)
		}
		if mh.tempN >= 3 {
			open("temp", "hw", name+" · Temperatur", "🔴 Temperatur zu hoch: "+name, fmt.Sprintf("%.0f °C (Grenze %d °C)", temp, mtTempHigh))
		}
		// BGP
		for _, b := range s.BGP {
			key := "bgp:" + b.Name
			if b.Up {
				mh.bgpSeen[b.Name] = true
				closeEv(key, "🟢 BGP wieder aufgebaut: "+b.Name+" ("+name+")")
			} else if mh.bgpSeen[b.Name] {
				open(key, "bgp", name+" · BGP "+b.Name, "🔴 BGP-Sitzung getrennt: "+b.Name+" ("+name+")", "Gegenstelle "+b.Remote+" AS "+b.AS)
			}
		}
		// PPP-Sitzungen eingebrochen
		if s.PPP >= 0 {
			var mx int32
			for _, v := range mh.pppMax {
				mx = max(mx, v)
			}
			cur := int32(s.PPP)
			if mx >= 10 && cur < mx*7/10 && mx-cur >= 10 {
				open("ppp", "ppp", name+" · PPP-Sitzungen", "🔴 Kundensitzungen eingebrochen: "+name, fmt.Sprintf("PPP-Sitzungen %d → %d", mx, cur))
			} else if mh.open["ppp"] != nil && cur >= mx*9/10 {
				closeEv("ppp", "🟢 Kundensitzungen wieder da: "+name)
			}
			if mh.open["ppp"] == nil {
				mh.pppMax = append(mh.pppMax, cur)
				if len(mh.pppMax) > 30 {
					mh.pppMax = mh.pppMax[1:]
				}
			}
		}
		// Log
		for _, l := range s.Log {
			if strings.Contains(l.Msg, "changed by") || strings.Contains(l.Msg, "added by") || strings.Contains(l.Msg, "removed by") {
				m.closeSiteEvent(m.newHostEvent(hc, "config", name+" · Konfiguration: "+trunc(l.Msg, 120), s.T, maint), s.T)
			}
		}
		for j := len(s.Log) - 1; j >= 0; j-- {
			mh.logs = append([]mtLog{s.Log[j]}, mh.logs...)
		}
		if len(mh.logs) > 200 {
			mh.logs = mh.logs[:200]
		}
		// Verlauf
		p := mtPoint{T: s.T, CPU: float32(cpu), Temp: float32(temp), PPP: int32(s.PPP), DHCP: int32(s.DHCP), Conns: int32(s.Conns)}
		if tm := mtNum(map[string]any{"x": s.Res["total-memory"]}, "x"); tm > 0 {
			p.Mem = float32(100 * (1 - mtNum(map[string]any{"x": s.Res["free-memory"]}, "x")/tm))
		}
		mh.hist = append(mh.hist, p)
		mh.cur = s
	}
	cut := now - 48*3600*1000
	if i := sort.Search(len(mh.hist), func(i int) bool { return mh.hist[i].T >= cut }); i > 0 {
		mh.hist = append([]mtPoint(nil), mh.hist[i:]...)
	}
	mh.boots = trimTimes(mh.boots, now-24*3600*1000)
	mh.mu.Unlock()
	// Ports → Schnittstellen-Statistik (Traffic, Port-Alarm)
	if h != nil {
		for _, s := range snaps {
			if s.Err == "" && len(s.Ifs) > 0 {
				m.ingestIfs(h, s.T, s.Ifs, "")
			}
		}
	}
	for _, n := range notes {
		m.notify(n)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

type mtView struct {
	On     bool      `json:"on"`
	At     int64     `json:"at,omitempty"`
	Err    string    `json:"err,omitempty"`
	Cur    *mtSnap   `json:"cur,omitempty"`
	Hist   []mtPoint `json:"hist,omitempty"`
	Boots  int       `json:"boots24"`
	Open   []string  `json:"open,omitempty"`
	Logs   []mtLog   `json:"logs,omitempty"`
	CPUMax float32   `json:"cpuMax24"`
}

func (m *Monitor) mtHostView(hc HostCfg, hours int) mtView {
	v := mtView{On: hc.Mt}
	if !hc.Mt {
		return v
	}
	mh := getMtHost(hc.ID)
	now := time.Now().UnixMilli()
	from := now - int64(hours)*3600000
	mh.mu.Lock()
	defer mh.mu.Unlock()
	v.At, v.Err, v.Boots, v.Logs = mh.at, mh.err, len(mh.boots), mh.logs
	if mh.cur != nil {
		c := *mh.cur
		c.Ifs, c.Log = nil, nil
		v.Cur = &c
	}
	var pts []mtPoint
	for _, p := range mh.hist {
		if p.T >= from {
			pts = append(pts, p)
			if p.T >= now-86400000 && p.CPU > v.CPUMax {
				v.CPUMax = p.CPU
			}
		}
	}
	step := len(pts)/300 + 1
	for i := 0; i < len(pts); i += step {
		v.Hist = append(v.Hist, pts[i])
	}
	for _, e := range mh.open {
		v.Open = append(v.Open, e.HostName)
	}
	sort.Strings(v.Open)
	return v
}

// POST /api/agent/mikrotik {host, snaps}
func (s *Server) handleAgentMikrotik(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Host  string   `json:"host"`
		Snaps []mtSnap `json:"snaps"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	h := s.m.host(req.Host)
	if h == nil {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg.Site == site.ID && cfg.Mt && !cfg.Paused {
		s.m.ingestMikrotik(cfg, req.Snaps)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/mikrotik?host=&hours=
func (s *Server) handleMikrotik(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 48 {
		hours = 24
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	writeJSON(w, s.m.mtHostView(cfg, hours))
}

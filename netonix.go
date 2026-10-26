package main

// Netonix-WISP-Switches über die JSON-Schnittstelle der Weboberfläche (durch den Agenten vor Ort):
// Anmeldung wie im Browser (POST /index.php), dann /api/v1/status/30sec und /api/v1/config.
// Ports laufen in die Schnittstellen-Statistik (ingestIfs, Port-Alarm, „Port langsamer“).
// Aktionen (nur freigegebene Ports): PoE 10 s aus (portbounce), Link neu aushandeln (portbouncelink).

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type nxPort struct {
	N      int     `json:"n"`
	Name   string  `json:"name,omitempty"`
	Link   string  `json:"link"` // 1G, 100M, 10G, Down …
	PoE    string  `json:"poe"`  // Off, 24V, 48V, 24VH, 48VH
	Watt   float64 `json:"w"`
	Enable bool    `json:"en"`
	SFP    string  `json:"sfp,omitempty"`
}

type nxSnap struct {
	T      int64      `json:"t"`
	Err    string     `json:"err,omitempty"`
	Name   string     `json:"name,omitempty"`
	Model  string     `json:"model,omitempty"`
	FW     string     `json:"fw,omitempty"`
	Uptime int64      `json:"up"`
	CPU    float64    `json:"cpu"`
	CPUT   float64    `json:"cpuT"`
	BoardT float64    `json:"boardT"`
	PHYT   float64    `json:"phyT"`
	V48    float64    `json:"v48"`
	V24    float64    `json:"v24"`
	V3     float64    `json:"v3"`
	Fans   []string   `json:"fans,omitempty"`
	Mem    int64      `json:"mem"`
	Base   float64    `json:"base"` // Grundverbrauch W
	Ports  []nxPort   `json:"ports,omitempty"`
	Ifs    []ifSample `json:"ifs,omitempty"`
}

// ---------- Agent ----------

type nxClient struct {
	base, user, pass string
	hc               *http.Client
	in               bool
}

func newNxClient(addr string, port int, user, pass string) *nxClient {
	jar, _ := cookiejar.New(nil)
	host := strings.TrimSpace(addr)
	if port != 0 && port != 443 {
		host += ":" + strconv.Itoa(port)
	}
	return &nxClient{base: "https://" + host, user: user, pass: pass, hc: &http.Client{
		Timeout: 20 * time.Second, Jar: jar,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *nxClient) login() error {
	resp, err := c.hc.PostForm(c.base+"/index.php", url.Values{"username": {c.user}, "password": {c.pass}})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" && !strings.Contains(loc, "main") {
		return errors.New("Anmeldung abgelehnt (Benutzer/Passwort prüfen)")
	}
	c.in = true
	return nil
}

// get holt eine Seite; bei abgelaufener Sitzung einmal neu anmelden.
func (c *nxClient) get(path string, out any) error {
	for try := 0; try < 2; try++ {
		if !c.in {
			if err := c.login(); err != nil {
				return err
			}
		}
		resp, err := c.hc.Get(c.base + path)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == 302 || resp.StatusCode == 401 || resp.StatusCode == 403 || (resp.StatusCode == 200 && len(b) > 0 && b[0] == '<') {
			c.in = false
			continue
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("HTTP %d bei %s", resp.StatusCode, path)
		}
		if s, ok := out.(*string); ok {
			*s = string(b)
			return nil
		}
		return json.Unmarshal(b, out)
	}
	return errors.New("Anmeldung abgelehnt (Benutzer/Passwort prüfen)")
}

func nxF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
}

func nxS(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func nxLinkMbit(l string) int64 {
	l = strings.ToUpper(strings.TrimSpace(l))
	switch {
	case strings.HasPrefix(l, "10G"):
		return 10000
	case strings.HasPrefix(l, "2.5G"):
		return 2500
	case strings.HasPrefix(l, "1G"), strings.HasPrefix(l, "1000"):
		return 1000
	case strings.HasPrefix(l, "100"):
		return 100
	case strings.HasPrefix(l, "10M"), l == "10":
		return 10
	}
	return 0
}

func nxQuery(c *nxClient) nxSnap {
	s := nxSnap{T: time.Now().UnixMilli()}
	var boot string
	if err := c.get("/api/v1/bootid", &boot); err != nil {
		s.Err = err.Error()
		return s
	}
	boot = strings.Trim(strings.TrimSpace(boot), `"`)
	var st map[string]any
	if err := c.get("/api/v1/status/30sec?"+url.QueryEscape(boot), &st); err != nil {
		s.Err = err.Error()
		return s
	}
	s.Model, s.FW = nxS(st["Model"]), nxS(st["Firmware_Version"])
	s.Uptime = int64(nxF(st["Uptime"]))
	s.CPU, s.CPUT, s.BoardT, s.PHYT = nxF(st["CPU_Usage"]), nxF(st["CPU_Temp"]), nxF(st["Board_Temp"]), nxF(st["PHY_Temp"])
	s.V48, s.V24, s.V3 = nxF(st["Board_48V"]), nxF(st["Board_24V"]), nxF(st["Board_3V"])
	s.Mem, s.Base = int64(nxF(st["Memory_Usage"])), nxF(st["BasePowerUsage"])
	if fs, ok := st["Fan_Speeds"].([]any); ok {
		for _, f := range fs {
			s.Fans = append(s.Fans, nxS(f))
		}
	}
	// Port-Namen aus der Konfiguration (nur Name/Aktiv – Zugangsdaten werden nicht übernommen)
	names, enabled := map[int]string{}, map[int]bool{}
	var cfg map[string]any
	if c.get("/api/v1/config", &cfg) == nil {
		s.Name = nxS(cfg["Switch_Name"])
		if ps, ok := cfg["Ports"].([]any); ok {
			for _, p := range ps {
				if m, ok := p.(map[string]any); ok {
					n := int(nxF(m["Number"]))
					names[n] = nxS(m["Name"])
					enabled[n] = m["Enable"] != false
				}
			}
		}
	}
	if ps, ok := st["Ports"].([]any); ok {
		for _, p := range ps {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			n := int(nxF(m["Number"]))
			en, has := enabled[n]
			if !has {
				en = true
			}
			np := nxPort{N: n, Name: names[n], Link: nxS(m["Link"]), PoE: nxS(m["PoE"]), Watt: nxF(m["Power"]), Enable: en, SFP: nxS(m["SFP_Vendor"])}
			s.Ports = append(s.Ports, np)
			up := np.Link != "" && !strings.EqualFold(np.Link, "Down")
			rxE, _ := strconv.ParseUint(nxS(m["RxErrors"]), 10, 64)
			txE, _ := strconv.ParseUint(nxS(m["TxErrors"]), 10, 64)
			in, _ := strconv.ParseUint(nxS(m["RxOctets"]), 10, 64)
			out, _ := strconv.ParseUint(nxS(m["TxOctets"]), 10, 64)
			label := "Port " + strconv.Itoa(n)
			s.Ifs = append(s.Ifs, ifSample{Idx: n, Name: label, Alias: names[n], Up: up, Speed: nxLinkMbit(np.Link), In: in, Out: out, Err: rxE + txE, HC: true})
		}
	}
	return s
}

func (a *Agent) nxLoop(ah *agentHost) {
	c := newNxClient(ah.cfg.Addr, ah.cfg.NxPort, ah.cfg.NxUser, ah.cfg.NxPass)
	var pend []nxSnap
	for {
		s := nxQuery(c)
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
		if resp, e := a.req("POST", "/api/agent/netonix", strings.NewReader(string(b)), false); e == nil {
			if resp.StatusCode == http.StatusOK {
				pend = nil
			}
			resp.Body.Close()
		} else {
			log.Printf("Netonix %s senden: %v", ah.cfg.Name, e)
		}
		select {
		case <-ah.stop:
			return
		case <-time.After(60 * time.Second):
		}
	}
}

func nxActPortOK(hc HostCfg, port string) bool {
	port = strings.TrimSpace(port)
	if _, err := strconv.Atoi(port); err != nil {
		return false
	}
	for _, p := range strings.Split(hc.NxActPorts, ",") {
		p = strings.TrimSpace(p)
		if p == "*" || p == port {
			return true
		}
	}
	return false
}

func (a *Agent) nxActionJob(j agentJob) (any, error) {
	a.mu.Lock()
	ah := a.hosts[j.Params["host"]]
	a.mu.Unlock()
	if ah == nil || !ah.cfg.Nx {
		return nil, errors.New("Host ist am Agenten nicht (mehr) als Netonix eingerichtet")
	}
	hc := ah.cfg
	port := j.Params["port"]
	if !nxActPortOK(hc, port) {
		return nil, errors.New("Port ist für Aktionen nicht freigegeben")
	}
	c := newNxClient(hc.Addr, hc.NxPort, hc.NxUser, hc.NxPass)
	var s string
	switch j.Params["action"] {
	case "poe":
		if err := c.get("/api/v1/portbounce?port="+port, &s); err != nil {
			return nil, err
		}
		return map[string]string{"msg": "PoE an Port " + port + " für 10 s ausgeschaltet"}, nil
	case "link":
		if err := c.get("/api/v1/portbouncelink?port="+port, &s); err != nil {
			return nil, err
		}
		return map[string]string{"msg": "Port " + port + " verbindet sich neu (dauert bis zu 1 Minute)"}, nil
	}
	return nil, errors.New("unbekannte Aktion")
}

// ---------- Server ----------

type nxPoint struct {
	T    int64   `json:"t"`
	CPU  float32 `json:"cpu"`
	Temp float32 `json:"temp"`
	V48  float32 `json:"v48"`
	V24  float32 `json:"v24"`
	Watt float32 `json:"watt"` // PoE gesamt
}

type nxHost struct {
	mu     sync.Mutex
	at     int64
	err    string
	cur    *nxSnap
	prevUp int64
	prevT  int64
	hist   []nxPoint
	boots  []int64
	open   map[string]*Event
	tempN  int
}

var nxData = struct {
	sync.Mutex
	m map[string]*nxHost
}{m: map[string]*nxHost{}}

func getNxHost(id string) *nxHost {
	nxData.Lock()
	defer nxData.Unlock()
	h := nxData.m[id]
	if h == nil {
		h = &nxHost{open: map[string]*Event{}}
		nxData.m[id] = h
	}
	return h
}

const nxTempHigh = 85 // °C CPU (Netonix laufen normal um 60–75 °C)

func (m *Monitor) ingestNetonix(hc HostCfg, snaps []nxSnap) {
	now := time.Now().UnixMilli()
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].T < snaps[j].T })
	xh := getNxHost(hc.ID)
	h := m.host(hc.ID)
	var notes []notice
	name := hc.Name
	xh.mu.Lock()
	for i := range snaps {
		s := &snaps[i]
		if s.T <= xh.at || s.T > now+60000 || s.T < now-24*3600*1000 {
			continue
		}
		xh.at = s.T
		if s.Err != "" {
			xh.err = s.Err
			continue
		}
		xh.err = ""
		recent := s.T >= now-5*60000
		maint := m.inMaint(hc.Org, hc.ID, time.UnixMilli(s.T))
		open := func(key, typ, label, title, body string) {
			if xh.open[key] != nil {
				return
			}
			e := m.newHostEvent(hc, typ, label, s.T, maint)
			xh.open[key] = e
			if !maint && recent {
				notes = append(notes, notice{"devoff", hc.ID, title, m.withInfo(hc.ID, hc.Site, body), hc.Org, ""})
			} else {
				m.evMu.Lock()
				e.Notified = false
				m.evMu.Unlock()
			}
		}
		closeEv := func(key, title string) {
			e := xh.open[key]
			if e == nil {
				return
			}
			delete(xh.open, key)
			ce := m.closeSiteEvent(e, s.T)
			if ce.Notified && recent {
				notes = append(notes, notice{"devon", hc.ID, title, m.withInfo(hc.ID, hc.Site, "Dauer "+fmtDurShort(ce.End-ce.Start)), hc.Org, ""})
			}
		}
		// Neustart
		if s.Uptime > 0 && xh.prevUp > 0 && xh.prevT > 0 && s.Uptime+120 < xh.prevUp+(s.T-xh.prevT)/1000 {
			at := s.T - s.Uptime*1000
			xh.boots = append(xh.boots, at)
			m.closeSiteEvent(m.newHostEvent(hc, "reboot", name+" (Netonix)", at, maint), at)
			if !maint && recent {
				notes = append(notes, notice{"reboot", hc.ID, "🔄 Netonix neu gestartet: " + name,
					m.withInfo(hc.ID, hc.Site, "Neustart um "+time.UnixMilli(at).Format("15:04")), hc.Org, ""})
			}
		}
		if s.Uptime > 0 {
			xh.prevUp, xh.prevT = s.Uptime, s.T
		}
		// Temperatur
		tmax := float64(hc.NxTempMax)
		if tmax == 0 {
			tmax = nxTempHigh
		}
		switch {
		case tmax < 0:
			xh.tempN = 0
			closeEv("temp", "🟢 Temperatur-Alarm ausgeschaltet: "+name)
		case s.CPUT >= tmax:
			xh.tempN++
		case s.CPUT > 0 && s.CPUT < tmax-5:
			xh.tempN = 0
			closeEv("temp", "🟢 CPU-Temperatur wieder normal: "+name)
		}
		if tmax > 0 && xh.tempN >= 3 {
			open("temp", "hw", name+" · CPU-Temperatur", "🔴 CPU-Temperatur zu hoch: "+name, fmt.Sprintf("%.0f °C seit 3 Minuten (Grenze %.0f °C)", s.CPUT, tmax))
		}
		// Versorgungsspannung (nur wenn das Modell sie meldet)
		if s.V48 > 5 {
			if s.V48 < 44 {
				open("v48", "hw", name+" · 48 V", "🔴 48-V-Versorgung zu niedrig: "+name, fmt.Sprintf("%.1f V", s.V48))
			} else if s.V48 >= 46 {
				closeEv("v48", "🟢 48-V-Versorgung wieder normal: "+name)
			}
		}
		if s.V24 > 5 {
			if s.V24 < 22 {
				open("v24", "hw", name+" · 24 V", "🔴 24-V-Versorgung zu niedrig: "+name, fmt.Sprintf("%.1f V", s.V24))
			} else if s.V24 >= 23 {
				closeEv("v24", "🟢 24-V-Versorgung wieder normal: "+name)
			}
		}
		var w float64
		for _, p := range s.Ports {
			w += p.Watt
		}
		xh.hist = append(xh.hist, nxPoint{T: s.T, CPU: float32(s.CPU), Temp: float32(s.CPUT), V48: float32(s.V48), V24: float32(s.V24), Watt: float32(w)})
		xh.cur = s
	}
	cut := now - 48*3600*1000
	if i := sort.Search(len(xh.hist), func(i int) bool { return xh.hist[i].T >= cut }); i > 0 {
		xh.hist = append([]nxPoint(nil), xh.hist[i:]...)
	}
	xh.boots = trimTimes(xh.boots, now-24*3600*1000)
	xh.mu.Unlock()
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

type nxView struct {
	On    bool      `json:"on"`
	At    int64     `json:"at,omitempty"`
	Err   string    `json:"err,omitempty"`
	Cur   *nxSnap   `json:"cur,omitempty"`
	Hist  []nxPoint `json:"hist,omitempty"`
	Boots int       `json:"boots24"`
	Open  []string  `json:"open,omitempty"`
}

func (m *Monitor) nxHostView(hc HostCfg, hours float64) nxView {
	v := nxView{On: hc.Nx}
	if !hc.Nx {
		return v
	}
	xh := getNxHost(hc.ID)
	from := time.Now().UnixMilli() - int64(hours*3600000)
	xh.mu.Lock()
	defer xh.mu.Unlock()
	v.At, v.Err, v.Boots = xh.at, xh.err, len(xh.boots)
	if xh.cur != nil {
		c := *xh.cur
		c.Ifs = nil
		v.Cur = &c
	}
	var pts []nxPoint
	for _, p := range xh.hist {
		if p.T >= from {
			pts = append(pts, p)
		}
	}
	step := len(pts)/300 + 1
	for i := 0; i < len(pts); i += step {
		v.Hist = append(v.Hist, pts[i])
	}
	for _, e := range xh.open {
		v.Open = append(v.Open, e.HostName)
	}
	sort.Strings(v.Open)
	return v
}

// POST /api/agent/netonix {host, snaps}
func (s *Server) handleAgentNetonix(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Host  string   `json:"host"`
		Snaps []nxSnap `json:"snaps"`
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
	if cfg.Site == site.ID && cfg.Nx && !cfg.Paused {
		s.m.ingestNetonix(cfg, req.Snaps)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/netonix?host=&hours=
func (s *Server) handleNetonix(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	hours, _ := strconv.ParseFloat(r.URL.Query().Get("hours"), 64)
	if hours <= 0 || hours > 48 {
		hours = 24
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	writeJSON(w, s.m.nxHostView(cfg, hours))
}

// POST /api/netonix/action {host, action: poe|link, port}
func (s *Server) handleNetonixAction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req struct{ Host, Action, Port string }
	if !readJSON(w, r, &req) {
		return
	}
	if !p.canEdit() {
		http.Error(w, "keine Berechtigung", 403)
		return
	}
	h := s.hostFor(r, req.Host)
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	h.mu.Lock()
	hc := h.cfg
	h.mu.Unlock()
	label := map[string]string{"poe": "PoE-Neustart", "link": "Link neu aushandeln"}[req.Action]
	switch {
	case label == "":
		http.Error(w, "unbekannte Aktion", 400)
		return
	case !hc.Nx:
		http.Error(w, "Host ist nicht als Netonix eingerichtet", 400)
		return
	case !nxActPortOK(hc, req.Port):
		http.Error(w, "Dieser Port ist für Aktionen nicht freigegeben", 403)
		return
	}
	if on, _ := s.m.siteOnline(hc.Site); !on {
		http.Error(w, "Der Agent des Standorts ist gerade nicht verbunden", 409)
		return
	}
	who := "Betreiber"
	if p.User != nil {
		who = p.User.Name
		if who == "" {
			who = p.User.Email
		}
	}
	now := time.Now().UnixMilli()
	s.m.closeSiteEvent(s.m.newHostEvent(hc, "action", hc.Name+" · "+label+" Port "+req.Port+" (von "+who+")", now, true), now)
	j := agentJob{ID: randHex(8), Kind: "nxaction", Params: map[string]string{"host": hc.ID, "action": req.Action, "port": strings.TrimSpace(req.Port)}}
	jobs.Lock()
	jobs.byID[j.ID] = &jobState{agentJob: j, Org: hc.Org, Site: hc.Site, Status: "queued", Created: now}
	jobs.queue[hc.Site] = append(jobs.queue[hc.Site], j)
	jobs.Unlock()
	writeJSON(w, map[string]string{"id": j.ID})
}

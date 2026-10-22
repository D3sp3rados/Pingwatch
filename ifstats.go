package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Schnittstellen-Statistik für SNMP-Hosts: Rohzähler → Raten (bit/s) je Minute,
// 48 h im Speicher, 30 Tage in der Datenbank (falls PostgreSQL).

type ifPoint struct {
	T   int64   `json:"t"`
	In  float64 `json:"in"`  // bit/s
	Out float64 `json:"out"` // bit/s
	Err uint32  `json:"e"`   // neue Fehler seit letzter Messung
	Up  bool    `json:"u"`
}

type ifInfo struct {
	Idx   int    `json:"idx"`
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
	Up    bool   `json:"up"`
	Speed int64  `json:"speed"`
}

type ifHost struct {
	mu     sync.Mutex
	prev   map[int]ifSample
	prevT  int64
	info   map[int]ifInfo
	series map[int][]ifPoint
	since  map[int]int64 // Status seit
	open   map[int]*Event
	loaded bool
	err    string
	at     int64
}

var ifData = struct {
	sync.Mutex
	m map[string]*ifHost
}{m: map[string]*ifHost{}}

const ifMemWindow = 48 * time.Hour

func getIfHost(id string) *ifHost {
	ifData.Lock()
	defer ifData.Unlock()
	h := ifData.m[id]
	if h == nil {
		h = &ifHost{prev: map[int]ifSample{}, info: map[int]ifInfo{}, series: map[int][]ifPoint{}, since: map[int]int64{}, open: map[int]*Event{}}
		ifData.m[id] = h
	}
	return h
}

// ifStore wird von PgStore erfüllt.
type ifStore interface {
	AppendIfs(host string, t int64, pts map[int]ifPoint, info map[int]ifInfo)
	LoadIfs(host string, from, to int64) (map[int][]ifPoint, map[int]ifInfo)
}

func counterDelta(cur, prev uint64, hc bool) (uint64, bool) {
	if cur >= prev {
		return cur - prev, true
	}
	if !hc && prev > 3_000_000_000 { // 32-Bit-Überlauf
		return cur + (1 << 32) - prev, true
	}
	return 0, false // Zähler zurückgesetzt (Neustart)
}

func watchSet(s string) map[int]bool {
	out := map[int]bool{}
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out[n] = true
		}
	}
	return out
}

// ingestIfs verarbeitet eine Abfrage der Schnittstellentabelle.
func (m *Monitor) ingestIfs(h *Host, t int64, ifs []ifSample, errText string) {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	ih := getIfHost(cfg.ID)
	ih.mu.Lock()
	ih.at = t
	if errText != "" {
		ih.err = errText
		ih.mu.Unlock()
		return
	}
	ih.err = ""
	watch := watchSet(cfg.Watch)
	dt := float64(t-ih.prevT) / 1000
	pts := map[int]ifPoint{}
	var starts, ends []Event
	inMaint := m.inMaint(cfg.Org, cfg.ID, time.UnixMilli(t))
	for _, s := range ifs {
		name := s.Name
		if name == "" {
			name = "Port " + strconv.Itoa(s.Idx)
		}
		old, had := ih.info[s.Idx]
		if !had || old.Up != s.Up {
			ih.since[s.Idx] = t
		}
		ih.info[s.Idx] = ifInfo{Idx: s.Idx, Name: name, Alias: s.Alias, Up: s.Up, Speed: s.Speed}
		if p, ok := ih.prev[s.Idx]; ok && ih.prevT > 0 && dt >= 5 && dt < 3600 {
			din, ok1 := counterDelta(s.In, p.In, s.HC)
			dout, ok2 := counterDelta(s.Out, p.Out, s.HC)
			derr, ok3 := counterDelta(s.Err, p.Err, true)
			if ok1 && ok2 {
				pt := ifPoint{T: t, In: float64(din) * 8 / dt, Out: float64(dout) * 8 / dt, Up: s.Up}
				if ok3 {
					pt.Err = uint32(derr)
				}
				pts[s.Idx] = pt
				ih.series[s.Idx] = append(ih.series[s.Idx], pt)
			}
		}
		ih.prev[s.Idx] = s
		// überwachte Ports: Alarm bei „down“
		if watch[s.Idx] && had {
			label := cfg.Name + " · " + name
			if s.Alias != "" {
				label += " (" + s.Alias + ")"
			}
			switch {
			case old.Up && !s.Up && ih.open[s.Idx] == nil:
				e := m.newHostEvent(cfg, "port", label, t, inMaint)
				ih.open[s.Idx] = e
				starts = append(starts, *e)
			case s.Up && ih.open[s.Idx] != nil:
				ends = append(ends, m.closeSiteEvent(ih.open[s.Idx], t))
				delete(ih.open, s.Idx)
			}
		}
	}
	ih.prevT = t
	cut := t - ifMemWindow.Milliseconds()
	for idx, sr := range ih.series {
		i := sort.Search(len(sr), func(i int) bool { return sr[i].T >= cut })
		if i > 0 {
			ih.series[idx] = append([]ifPoint(nil), sr[i:]...)
		}
	}
	info := make(map[int]ifInfo, len(ih.info))
	for k, v := range ih.info {
		info[k] = v
	}
	ih.mu.Unlock()
	if st, ok := m.store.(ifStore); ok && len(pts) > 0 {
		go st.AppendIfs(cfg.ID, t, pts, info)
	}
	for _, e := range starts {
		if !e.Maint {
			m.notify(notice{"portdown", cfg.ID, "🔴 Port down: " + e.HostName, "Schnittstelle meldet „down“ seit " + time.UnixMilli(e.Start).Format("15:04"), cfg.Org, ""})
		}
	}
	for _, e := range ends {
		if e.Notified {
			m.notify(notice{"portup", cfg.ID, "🟢 Port wieder up: " + e.HostName, "War " + fmtDurShort(e.End-e.Start) + " down", cfg.Org, ""})
		}
	}
}

func (m *Monitor) newHostEvent(cfg HostCfg, typ, name string, at int64, maint bool) *Event {
	m.evMu.Lock()
	m.nextEv++
	e := &Event{ID: m.nextEv, HostID: cfg.ID, Site: cfg.Site, HostName: name, Org: cfg.Org, Type: typ, Start: at, Last: at, Maint: maint, Decided: true, Notified: !maint}
	m.events = append(m.events, e)
	m.evMu.Unlock()
	return e
}

// ensureIfLoaded lädt nach einem Neustart die letzten 48 h aus der Datenbank.
func (m *Monitor) ensureIfLoaded(id string) {
	ih := getIfHost(id)
	ih.mu.Lock()
	defer ih.mu.Unlock()
	if ih.loaded {
		return
	}
	ih.loaded = true
	st, ok := m.store.(ifStore)
	if !ok {
		return
	}
	pts, info := st.LoadIfs(id, time.Now().Add(-ifMemWindow).UnixMilli(), 0)
	for idx, sr := range pts {
		if len(ih.series[idx]) > 0 {
			sr = append(sr, ih.series[idx]...)
		}
		ih.series[idx] = sr
	}
	for idx, in := range info {
		if _, ok := ih.info[idx]; !ok {
			ih.info[idx] = in
		}
	}
}

// ---------- API ----------

type ifView struct {
	ifInfo
	Since  int64   `json:"since"`
	In     float64 `json:"in"`
	Out    float64 `json:"out"`
	Err24  uint64  `json:"err24"`
	MaxIn  float64 `json:"maxIn"`
	MaxOut float64 `json:"maxOut"`
	Watch  bool    `json:"watch"`
}

// GET /api/snmp?host=&if=&hours= → Schnittstellen (+ Verlauf einer Schnittstelle)
func (s *Server) handleSNMP(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	s.m.ensureIfLoaded(cfg.ID)
	ih := getIfHost(cfg.ID)
	watch := watchSet(cfg.Watch)
	now := time.Now().UnixMilli()
	ih.mu.Lock()
	out := []ifView{}
	for idx, in := range ih.info {
		v := ifView{ifInfo: in, Since: ih.since[idx], Watch: watch[idx]}
		sr := ih.series[idx]
		if n := len(sr); n > 0 && now-sr[n-1].T < 5*60000 {
			v.In, v.Out = sr[n-1].In, sr[n-1].Out
		}
		for _, p := range sr {
			if p.T >= now-86400000 {
				v.Err24 += uint64(p.Err)
				if p.In > v.MaxIn {
					v.MaxIn = p.In
				}
				if p.Out > v.MaxOut {
					v.MaxOut = p.Out
				}
			}
		}
		out = append(out, v)
	}
	var series []ifPoint
	idx, _ := strconv.Atoi(r.URL.Query().Get("if"))
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 {
		hours = 24
	}
	from := now - int64(hours)*3600000
	if idx > 0 && hours <= 48 {
		for _, p := range ih.series[idx] {
			if p.T >= from {
				series = append(series, p)
			}
		}
	}
	res := map[string]any{"at": ih.at, "err": ih.err}
	ih.mu.Unlock()
	if idx > 0 && hours > 48 {
		if st, ok := s.m.store.(ifStore); ok {
			pts, _ := st.LoadIfs(cfg.ID, from, now)
			series = pts[idx]
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	res["ifs"] = out
	if idx > 0 {
		res["series"] = downsampleIf(series, 400)
	}
	writeJSON(w, res)
}

// downsampleIf verdichtet auf höchstens n Punkte (Mittelwert, Fehler summiert).
func downsampleIf(p []ifPoint, n int) []ifPoint {
	if len(p) <= n {
		return p
	}
	step := (len(p) + n - 1) / n
	var out []ifPoint
	for i := 0; i < len(p); i += step {
		end := min(i+step, len(p))
		a := ifPoint{T: p[i].T, Up: true}
		for _, x := range p[i:end] {
			a.In += x.In
			a.Out += x.Out
			a.Err += x.Err
			a.Up = a.Up && x.Up
		}
		k := float64(end - i)
		a.In /= k
		a.Out /= k
		out = append(out, a)
	}
	return out
}

// POST /api/snmp/watch {host, watch:[idx…]} – Ports mit Alarm bei „down“
func (s *Server) handleSNMPWatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host  string `json:"host"`
		Watch []int  `json:"watch"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	h := s.hostFor(r, req.Host)
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	var parts []string
	for _, i := range req.Watch {
		if i > 0 && len(parts) < 256 {
			parts = append(parts, strconv.Itoa(i))
		}
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	cfg.Watch = strings.Join(parts, ",")
	s.m.UpsertHost(cfg)
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/agent/snmp {host, t, ifs, error}
func (s *Server) handleAgentSNMP(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Host  string     `json:"host"`
		T     int64      `json:"t"`
		Ifs   []ifSample `json:"ifs"`
		Error string     `json:"error"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	h := s.m.host(req.Host)
	if h == nil {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	h.mu.Lock()
	belongs := h.cfg.Site == site.ID
	h.mu.Unlock()
	now := time.Now().UnixMilli()
	if !belongs || req.T > now+60000 || req.T < now-3600000 {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	s.m.ingestIfs(h, req.T, req.Ifs, req.Error)
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- Abfrage-Schleifen ----------

const ifPollEvery = 60 * time.Second

// snmpLoopCloud fragt die Schnittstellen eines Cloud-Hosts jede Minute ab.
func (m *Monitor) snmpLoopCloud(h *Host, cfg HostCfg, stop chan struct{}) {
	for {
		t := time.Now().UnixMilli()
		ifs, err := snmpInterfaces(cfg)
		e := ""
		if err != nil {
			e = err.Error()
		}
		select {
		case <-stop:
			return
		default:
		}
		m.ingestIfs(h, t, ifs, e)
		select {
		case <-stop:
			return
		case <-time.After(ifPollEvery):
		}
	}
}

func (a *Agent) snmpLoop(ah *agentHost) {
	for {
		t := time.Now().UnixMilli()
		ifs, err := snmpInterfaces(ah.cfg)
		body := map[string]any{"host": ah.cfg.ID, "t": t, "ifs": ifs}
		if err != nil {
			body["error"] = err.Error()
		}
		select {
		case <-ah.stop:
			return
		default:
		}
		b, _ := json.Marshal(body)
		if resp, e := a.req("POST", "/api/agent/snmp", bytes.NewReader(b), false); e == nil {
			resp.Body.Close()
		} else {
			log.Printf("SNMP %s senden: %v", ah.cfg.Name, e)
		}
		select {
		case <-ah.stop:
			return
		case <-time.After(ifPollEvery):
		}
	}
}

func fmtBits(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1f Gbit/s", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", v/1e6)
	}
	return fmt.Sprintf("%.0f kbit/s", v/1e3)
}

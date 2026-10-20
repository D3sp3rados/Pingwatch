package main

import (
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	m        *Monitor
	envPass  string
	agentDir string
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	dataDir := fs.String("data", envOr("DATA_DIR", "./data"), "Datenverzeichnis")
	port := fs.String("port", envOr("PORT", "8080"), "HTTP-Port")
	fs.Parse(args)
	os.MkdirAll(*dataDir, 0o755)
	log.SetFlags(log.LstdFlags)
	log.Printf("PingWatch-Server %s startet, Daten in %s, Zeitzone %s", version, *dataDir, time.Local)

	var store Storage
	if dsn, ok := databaseDSN(); ok {
		pg, err := NewPgStore(dsn)
		if err != nil {
			log.Fatalf("Datenbank: %v", err)
		}
		log.Printf("Speicher: PostgreSQL-Datenbank")
		store = pg
	} else {
		log.Printf("Speicher: Dateien in %s (keine DATABASE_URL gesetzt)", *dataDir)
		store = NewFileStore(*dataDir)
	}
	m := NewMonitor(store)
	m.notices = make(chan notice, 1000)
	if p, err := newPusherFromEnv(); err != nil {
		log.Printf("Push-Benachrichtigungen deaktiviert: %v", err)
	} else if p != nil {
		m.pusher = p
		log.Printf("Push-Benachrichtigungen aktiv (Topic %s)", p.topic)
	}
	go m.pushLoop()
	s := &Server{m: m, envPass: os.Getenv("ADMIN_PASSWORD"), agentDir: envOr("AGENT_DIR", "./downloads")}
	if s.envPass == "" && m.cfg.PassHash == "" {
		pw := randHex(5)
		m.mu.Lock()
		m.cfg.PassHash = s.hashPw(pw)
		m.saveConfigLocked()
		m.mu.Unlock()
		os.WriteFile(filepath.Join(*dataDir, "ERSTPASSWORT.txt"), []byte(pw+"\n"), 0o600)
		log.Printf("==============================================")
		log.Printf(" Erstpasswort für das Dashboard: %s", pw)
		log.Printf(" (oder Umgebungsvariable ADMIN_PASSWORD setzen)")
		log.Printf("==============================================")
	}
	if !simMode {
		if fd, raw, err := openICMP(); err == nil {
			closeFD(fd)
			if raw {
				m.icmpOK = "raw"
			} else {
				m.icmpOK = "dgram"
			}
		} else {
			m.icmpOK = "none"
			log.Printf("Hinweis: ICMP im Container nicht erlaubt – Cloud-Hosts mit Methode TCP anlegen")
		}
	} else {
		m.icmpOK = "sim"
		log.Printf("SIMULATIONSMODUS aktiv (PINGWATCH_SIM=1)")
	}
	m.Start()

	handler := s.routes()
	if os.Getenv("ACCESS_LOG") != "0" {
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/agent/config" {
				log.Printf("HTTP %s %s (Host %s, von %s)", r.Method, r.URL.Path, r.Host, clientIP(r))
			}
			inner.ServeHTTP(w, r)
		})
	}
	srv := &http.Server{Addr: "0.0.0.0:" + *port, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Printf("Beende …")
		m.Shutdown()
		os.Exit(0)
	}()
	log.Printf("Dashboard auf Port %s", *port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func (s *Server) hashPw(pw string) string {
	h := sha256.Sum256([]byte(s.m.cfg.Secret + "|" + pw))
	return hex.EncodeToString(h[:])
}

func (s *Server) passwordOK(pw string) bool {
	if s.envPass != "" {
		return subtle.ConstantTimeCompare([]byte(pw), []byte(s.envPass)) == 1
	}
	s.m.mu.RLock()
	defer s.m.mu.RUnlock()
	return subtle.ConstantTimeCompare([]byte(s.hashPw(pw)), []byte(s.m.cfg.PassHash)) == 1
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))

	// öffentlich
	for _, p := range []string{"/manifest.webmanifest", "/icon.svg", "/icon-180.png", "/icon-512.png", "/favicon.ico"} {
		mux.Handle(p, static)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "pw_session", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("/api/agent/config", s.handleAgentConfig)
	mux.HandleFunc("/api/agent/push", s.handleAgentPush)

	// geschützt
	protect := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p := s.authenticate(r)
			if p == nil {
				if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/download/") {
					http.Error(w, "nicht angemeldet", http.StatusUnauthorized)
					return
				}
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			// Nur-Lesen-Benutzer dürfen nichts ändern
			if r.Method != http.MethodGet && !p.canEdit() && !readOnlyOK[r.URL.Path] {
				http.Error(w, "Keine Berechtigung (nur Lesezugriff)", http.StatusForbidden)
				return
			}
			h(w, s.withPrincipal(r, p))
		}
	}
	super := func(h http.HandlerFunc) http.HandlerFunc {
		return protect(func(w http.ResponseWriter, r *http.Request) {
			if !principal(r).Super {
				http.Error(w, "Nur für den Betreiber", http.StatusForbidden)
				return
			}
			h(w, r)
		})
	}
	s.registerTenantRoutes(mux, protect, super)
	mux.HandleFunc("/", protect(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	}))
	mux.HandleFunc("/api/state", protect(s.handleState))
	mux.HandleFunc("/api/overview", protect(s.handleOverview))
	mux.HandleFunc("/api/series", protect(s.handleSeries))
	mux.HandleFunc("/api/stats", protect(s.handleStats))
	mux.HandleFunc("/api/events", protect(s.handleEvents))
	mux.HandleFunc("/api/hosts", protect(s.handleHosts))
	mux.HandleFunc("/api/sites", protect(s.handleSites))
	mux.HandleFunc("/api/settings", protect(s.handleSettings))
	mux.HandleFunc("/api/reset", protect(s.handleReset))
	mux.HandleFunc("/api/app/login", s.handleAppLogin)
	mux.HandleFunc("/api/app/device", protect(s.handleDevice))
	mux.HandleFunc("/api/app/pushtest", protect(s.handlePushTest))
	mux.HandleFunc("/api/export/events.csv", protect(s.exportEvents))
	mux.HandleFunc("/api/export/samples.csv", protect(s.exportSamples))
	mux.HandleFunc("/api/export/stats.csv", protect(s.exportStats))
	mux.HandleFunc("/download/", protect(s.handleDownload))
	return mux
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()
		tok, _, err := s.login(r.FormValue("email"), r.FormValue("password"))
		if err == nil {
			s.setSession(w, r, tok)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		time.Sleep(800 * time.Millisecond)
		http.Redirect(w, r, "/login?f=1", http.StatusFound)
		return
	}
	b, _ := webFS.ReadFile("web/login.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// ---------- Datenabfragen ----------

type HostView struct {
	HostCfg
	IP        string  `json:"ip"`
	Status    string  `json:"status"`
	LastR     int32   `json:"lastR"`
	LastT     int64   `json:"lastT"`
	LastErr   string  `json:"lastErr"`
	Consec    int     `json:"consec"`
	Baseline  int32   `json:"baseline"`
	Threshold int32   `json:"threshold"`
	H1        Agg     `json:"h1"`
	H24       Agg     `json:"h24"`
	Spark     []int32 `json:"spark"`
	Ongoing   []Event `json:"ongoing"`
	LastOut   *Event  `json:"lastOutage"`
}

type SiteView struct {
	SiteCfg
	Online   bool   `json:"online"`
	LastSeen int64  `json:"lastSeen"`
	IP       string `json:"ip"`
	Version  string `json:"version"`
	Hosts    int    `json:"hosts"`
}

func (m *Monitor) hostList() []*Host {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Host, 0, len(m.cfg.Hosts))
	for _, hc := range m.cfg.Hosts {
		if h := m.hosts[hc.ID]; h != nil {
			out = append(out, h)
		}
	}
	return out
}

func (m *Monitor) siteOnline(id string) (bool, *siteState) {
	if id == "cloud" {
		return true, nil
	}
	m.mu.RLock()
	st := m.sites[id]
	m.mu.RUnlock()
	if st == nil {
		return false, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastSeen > 0 && time.Now().UnixMilli()-st.lastSeen < 45_000, st
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	m := s.m
	now := time.Now().UnixMilli()
	h1, h24 := now-3600_000, now-24*3600_000
	p := principal(r)
	hv := []HostView{}
	for _, h := range m.hostsOf(p.Org) {
		siteOn, _ := m.siteOnline(h.cfg.Site)
		h.mu.Lock()
		v := HostView{HostCfg: h.cfg, IP: h.ip, LastR: h.lastR, LastT: h.lastT, LastErr: h.lastErr, Consec: h.consec,
			Threshold: h.effThreshold()}
		if h.baseN >= 30 {
			v.Baseline = h.baseMed
		}
		i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= h24 })
		for ; i < len(h.samples); i++ {
			sm := h.samples[i]
			v.H24.Add(sm)
			if sm.T >= h1 {
				v.H1.Add(sm)
			}
		}
		st := len(h.samples) - 90
		if st < 0 {
			st = 0
		}
		v.Spark = make([]int32, 0, 90)
		for _, sm := range h.samples[st:] {
			if sm.F == 1 {
				v.Spark = append(v.Spark, -sm.R-2) // negativ kodiert = Spitze
			} else {
				v.Spark = append(v.Spark, sm.R)
			}
		}
		stale := h.lastT == 0 || now-h.lastT > int64(h.cfg.IntervalMs)*5+15000
		switch {
		case h.cfg.Paused:
			v.Status = "paused"
		case !siteOn:
			v.Status = "siteoff"
		case h.lastT == 0:
			v.Status = "unknown"
		case stale:
			v.Status = "stale"
		case h.consec >= h.cfg.OutageCount:
			v.Status = "down"
		case h.lastR < 0:
			v.Status = "loss"
		case h.lastF == 1:
			v.Status = "spike"
		default:
			v.Status = "ok"
		}
		m.evMu.Lock()
		for _, e := range []*Event{h.oOut, h.oLoss, h.oSpk} {
			if e != nil {
				v.Ongoing = append(v.Ongoing, *e)
			}
		}
		for j := len(m.events) - 1; j >= 0; j-- {
			if e := m.events[j]; e.HostID == h.cfg.ID && e.Type == "outage" {
				cp := *e
				v.LastOut = &cp
				break
			}
		}
		m.evMu.Unlock()
		h.mu.Unlock()
		hv = append(hv, v)
	}
	m.mu.RLock()
	var sites []SiteCfg
	for _, sc := range m.cfg.Sites {
		if sc.Org == p.Org {
			sites = append(sites, sc)
		}
	}
	cnt := map[string]int{}
	for _, hc := range m.cfg.Hosts {
		if hc.Org == p.Org {
			cnt[hc.Site]++
		}
	}
	ret := m.cfg.RetentionDays
	devs := []map[string]any{}
	for _, d := range m.cfg.Devices {
		if d.Org != p.Org && !(p.Super && d.AllOrgs) {
			continue
		}
		devs = append(devs, map[string]any{"id": d.Token[:min(12, len(d.Token))], "name": d.Name, "added": d.Added,
			"outage": d.Outage, "loss": d.Loss, "spike": d.Spike, "offline": d.Offline, "hosts": len(d.Hosts), "allOrgs": d.AllOrgs})
	}
	m.mu.RUnlock()
	sv := []SiteView{{SiteCfg: SiteCfg{ID: "cloud", Name: "Cloud"}, Online: true, Hosts: cnt["cloud"]}}
	for _, sc := range sites {
		on, st := m.siteOnline(sc.ID)
		v := SiteView{SiteCfg: SiteCfg{ID: sc.ID, Name: sc.Name}, Online: on, Hosts: cnt[sc.ID]}
		if st != nil {
			st.mu.Lock()
			v.LastSeen, v.IP, v.Version = st.lastSeen, st.ip, st.version
			st.mu.Unlock()
		}
		sv = append(sv, v)
	}
	out := map[string]any{
		"now": now, "hosts": hv, "sites": sv, "version": version, "started": m.started,
		"icmp": m.icmpOK, "retentionDays": ret, "envPassword": s.envPass != "", "storage": m.store.Kind(), "push": m.pusher != nil, "devices": devs,
	}
	for k, v := range s.accountInfo(r) {
		out[k] = v
	}
	writeJSON(w, out)
}

func (m *Monitor) host(id string) *Host {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hosts[id]
}

func qInt(r *http.Request, k string, d int64) int64 {
	if v, err := strconv.ParseInt(r.URL.Query().Get(k), 10, 64); err == nil {
		return v
	}
	return d
}

func bucketMem(h *Host, from, to, bucket int64) []Agg {
	cnt := int((to - from + bucket - 1) / bucket)
	if cnt < 1 {
		cnt = 1
	}
	out := make([]Agg, cnt)
	h.mu.Lock()
	i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= from })
	for ; i < len(h.samples) && h.samples[i].T < to; i++ {
		idx := int((h.samples[i].T - from) / bucket)
		if idx < cnt {
			out[idx].Add(h.samples[i])
		}
	}
	h.mu.Unlock()
	return out
}

func (m *Monitor) dayAggs(h *Host, day string) []Agg {
	if day == dayOf(time.Now().UnixMilli()) {
		ds := dayStart(day).UnixMilli()
		h.mu.Lock()
		i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= ds })
		cp := append([]Sample(nil), h.samples[i:]...)
		h.mu.Unlock()
		return minuteAggs(cp, day)
	}
	return m.store.DayAgg(h.cfg.ID, day)
}

func minuteTime(day string, idx int) int64 {
	ds := dayStart(day)
	return time.Date(ds.Year(), ds.Month(), ds.Day(), idx/60, idx%60, 0, 0, time.Local).UnixMilli()
}

func (m *Monitor) series(h *Host, from, to int64, n int) (int64, int64, []Agg) {
	if n < 10 {
		n = 10
	}
	if n > 2000 {
		n = 2000
	}
	if to <= from {
		to = from + 3600_000
	}
	bucket := (to - from) / int64(n)
	if bucket < 500 {
		bucket = 500
	}
	memStart := time.Now().Add(-memWindow).UnixMilli() + 60_000
	if from >= memStart {
		return from, bucket, bucketMem(h, from, to, bucket)
	}
	bucket = ((bucket + 59_999) / 60_000) * 60_000
	from = from - from%60_000
	cnt := int((to - from + bucket - 1) / bucket)
	out := make([]Agg, cnt)
	for d := time.UnixMilli(from); ; d = d.AddDate(0, 0, 1) {
		day := d.Format("2006-01-02")
		if dayStart(day).UnixMilli() >= to {
			break
		}
		for idx, a := range m.dayAggs(h, day) {
			if a.N == 0 {
				continue
			}
			t := minuteTime(day, idx)
			if t < from || t >= to {
				continue
			}
			if k := int((t - from) / bucket); k < cnt {
				out[k].Merge(a)
			}
		}
		if d.Sub(time.UnixMilli(to)) > 48*time.Hour {
			break
		}
	}
	return from, bucket, out
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UnixMilli()
	hours := qInt(r, "hours", 24)
	n := qInt(r, "n", 96)
	if n < 12 || n > 400 {
		n = 96
	}
	span := hours * 3600_000
	bucket := span / n
	from := now - span
	// an Bucket-Grenzen ausrichten, damit Balken nicht "wandern"
	from = from - from%bucket + bucket
	out := map[string][]Agg{}
	for _, h := range s.m.hostsOf(principal(r).Org) {
		out[h.cfg.ID] = bucketMem(h, from, from+bucket*n, bucket)
	}
	writeJSON(w, map[string]any{"from": from, "bucket": bucket, "n": n, "hosts": out})
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	now := time.Now().UnixMilli()
	to := qInt(r, "to", now)
	from := qInt(r, "from", to-3600_000)
	from, bucket, b := s.m.series(h, from, to, int(qInt(r, "n", 600)))
	h.mu.Lock()
	thr := h.effThreshold()
	base := int32(0)
	if h.baseN >= 30 {
		base = h.baseMed
	}
	h.mu.Unlock()
	writeJSON(w, map[string]any{"from": from, "to": to, "bucket": bucket, "b": b, "threshold": thr, "baseline": base})
}

type DayStat struct {
	Day      string `json:"day"`
	Hours    []Agg  `json:"hours"`
	Total    Agg    `json:"total"`
	Outages  int    `json:"outages"`
	Downtime int64  `json:"downtime"`
	Losses   int    `json:"lossEvents"`
	Spikes   int    `json:"spikeEvents"`
}

func (m *Monitor) dayStats(h *Host, days int) []DayStat {
	var out []DayStat
	now := time.Now()
	m.evMu.Lock()
	evs := make([]Event, 0)
	for _, e := range m.events {
		if e.HostID == h.cfg.ID {
			evs = append(evs, *e)
		}
	}
	m.evMu.Unlock()
	for i := 0; i < days; i++ {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		aggs := m.dayAggs(h, day)
		ds := DayStat{Day: day, Hours: make([]Agg, 24)}
		for idx, a := range aggs {
			ds.Hours[idx/60].Merge(a)
			ds.Total.Merge(a)
		}
		d0 := dayStart(day).UnixMilli()
		d1 := dayStart(day).AddDate(0, 0, 1).UnixMilli()
		for _, e := range evs {
			end := e.End
			if end == 0 {
				end = now.UnixMilli()
			}
			if e.Start >= d1 || end < d0 {
				continue
			}
			switch e.Type {
			case "outage":
				if e.Start >= d0 {
					ds.Outages++
				}
				a, b := e.Start, end
				if a < d0 {
					a = d0
				}
				if b > d1 {
					b = d1
				}
				ds.Downtime += b - a
			case "loss":
				if e.Start >= d0 {
					ds.Losses++
				}
			case "spike":
				if e.Start >= d0 {
					ds.Spikes++
				}
			}
		}
		out = append(out, ds)
	}
	return out
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	days := int(qInt(r, "days", 7))
	if days < 1 || days > 366 {
		days = 7
	}
	writeJSON(w, map[string]any{"days": s.m.dayStats(h, days)})
}

func (m *Monitor) queryEvents(org, host, site string, types map[string]bool, from, to int64, limit int) []Event {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	out := []Event{}
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		e := m.events[i]
		if org != "" && e.Org != org {
			continue
		}
		if host != "" && e.HostID != host {
			continue
		}
		if site != "" && e.Site != site {
			continue
		}
		if len(types) > 0 && !types[e.Type] {
			continue
		}
		end := e.End
		if end == 0 {
			end = time.Now().UnixMilli()
		}
		if (from > 0 && end < from) || (to > 0 && e.Start > to) {
			continue
		}
		out = append(out, *e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out
}

func parseTypes(r *http.Request) map[string]bool {
	types := map[string]bool{}
	for _, t := range strings.Split(r.URL.Query().Get("types"), ",") {
		if t != "" {
			types[t] = true
		}
	}
	return types
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := int(qInt(r, "limit", 300))
	if limit < 1 || limit > 5000 {
		limit = 300
	}
	writeJSON(w, s.m.queryEvents(principal(r).Org, q.Get("host"), q.Get("site"), parseTypes(r), qInt(r, "from", 0), qInt(r, "to", 0), limit))
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	switch r.Method {
	case http.MethodPost:
		var hc HostCfg
		if err := json.NewDecoder(r.Body).Decode(&hc); err != nil || strings.TrimSpace(hc.Addr) == "" {
			http.Error(w, "Adresse fehlt", 400)
			return
		}
		hc.Addr = strings.TrimSpace(hc.Addr)
		hc.Name = strings.TrimSpace(hc.Name)
		if hc.Name == "" {
			hc.Name = hc.Addr
		}
		if hc.ID != "" {
			if s.hostFor(r, hc.ID) == nil {
				http.Error(w, "unbekannter Host", 404)
				return
			}
		} else if err := s.checkLimits(p.Org, 0, 1); err != nil {
			http.Error(w, err.Error(), http.StatusPaymentRequired)
			return
		}
		if hc.Site != "" && hc.Site != "cloud" && s.siteFor(r, hc.Site) == nil {
			http.Error(w, "unbekannter Standort", 400)
			return
		}
		if o := s.m.orgByID(p.Org); o != nil {
			if mi := planByID(o.Plan).MinInterval; hc.IntervalMs < mi {
				hc.IntervalMs = mi
			}
		}
		hc.Org = p.Org
		writeJSON(w, s.m.UpsertHost(hc))
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if s.hostFor(r, id) == nil {
			http.Error(w, "unbekannter Host", 404)
			return
		}
		s.m.DeleteHost(id)
		writeJSON(w, map[string]bool{"ok": true})
	default:
		s.m.mu.RLock()
		defer s.m.mu.RUnlock()
		out := []HostCfg{}
		for _, hc := range s.m.cfg.Hosts {
			if hc.Org == p.Org {
				out = append(out, hc)
			}
		}
		writeJSON(w, out)
	}
}

func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	switch r.Method {
	case http.MethodPost:
		var sc SiteCfg
		if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
			http.Error(w, "ungültig", 400)
			return
		}
		sc.Name = strings.TrimSpace(sc.Name)
		if sc.ID == "" {
			if sc.Name == "" {
				http.Error(w, "Name fehlt", 400)
				return
			}
			if err := s.checkLimits(p.Org, 1, 0); err != nil {
				http.Error(w, err.Error(), http.StatusPaymentRequired)
				return
			}
			sc.Org = p.Org
		} else if s.siteFor(r, sc.ID) == nil {
			http.Error(w, "unbekannter Standort", 404)
			return
		}
		writeJSON(w, s.m.UpsertSite(sc))
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if s.siteFor(r, id) == nil {
			http.Error(w, "unbekannter Standort", 404)
			return
		}
		s.m.DeleteSite(id)
		writeJSON(w, map[string]bool{"ok": true})
	default:
		s.m.mu.RLock()
		defer s.m.mu.RUnlock()
		out := []SiteCfg{}
		for _, sc := range s.m.cfg.Sites {
			if sc.Org == p.Org {
				out = append(out, sc)
			}
		}
		writeJSON(w, out)
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	p := principal(r)
	var req struct {
		RetentionDays int    `json:"retentionDays"`
		OldPassword   string `json:"oldPassword"`
		NewPassword   string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	if req.NewPassword != "" && len(req.NewPassword) < 8 {
		http.Error(w, "Mindestens 8 Zeichen", 400)
		return
	}
	if req.NewPassword != "" && p.User != nil {
		// eigenes Benutzerpasswort
		if !checkPassword(p.User.PassHash, req.OldPassword) {
			http.Error(w, "Altes Passwort falsch", 403)
			return
		}
		h := hashPassword(req.NewPassword)
		s.m.mu.Lock()
		for i := range s.m.cfg.Users {
			if s.m.cfg.Users[i].ID == p.User.ID {
				s.m.cfg.Users[i].PassHash = h
			}
		}
		s.m.saveConfigLocked()
		s.m.mu.Unlock()
		u := s.m.userByID(p.User.ID)
		s.setSession(w, r, s.makeToken(u.ID, userRef(u)))
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if !p.Super {
		http.Error(w, "Nur für den Betreiber", 403)
		return
	}
	if req.NewPassword != "" {
		if s.envPass != "" {
			http.Error(w, "Passwort ist per ADMIN_PASSWORD festgelegt", 400)
			return
		}
		if !s.passwordOK(req.OldPassword) {
			http.Error(w, "Altes Passwort falsch", 403)
			return
		}
	}
	s.m.mu.Lock()
	if req.RetentionDays >= 7 && req.RetentionDays <= 730 {
		s.m.cfg.RetentionDays = req.RetentionDays
	}
	if req.NewPassword != "" {
		s.m.cfg.PassHash = s.hashPw(req.NewPassword)
	}
	s.m.saveConfigLocked()
	s.m.mu.Unlock()
	if req.NewPassword != "" {
		s.setSession(w, r, s.makeToken("admin", s.adminRef()))
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/download/")
	if name == "agent.json" {
		site := s.siteFor(r, r.URL.Query().Get("site"))
		if site == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="agent.json"`)
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.MarshalIndent(map[string]string{"server": baseURL(r), "token": site.Token, "site": site.Name}, "", "  ")
		w.Write(b)
		return
	}
	allowed := map[string]bool{"pingwatch-agent.exe": true, "pingwatch-agent-linux-amd64": true, "pingwatch-agent-linux-arm64": true}
	if !allowed[name] {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.agentDir, name)
	if _, err := os.Stat(p); err != nil {
		http.Error(w, "Datei nicht im Image enthalten", 404)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeFile(w, r, p)
}

// ---------- Agent-Schnittstelle ----------

func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	s.m.siteSeen(site.ID, clientIP(r), r.Header.Get("X-Agent-Version"))
	s.m.mu.RLock()
	hosts := []HostCfg{}
	for _, hc := range s.m.cfg.Hosts {
		if hc.Site == site.ID && !hc.Paused {
			hosts = append(hosts, hc)
		}
	}
	ver := s.m.cfg.Version
	s.m.mu.RUnlock()
	writeJSON(w, map[string]any{"site": map[string]string{"id": site.ID, "name": site.Name}, "hosts": hosts, "version": ver})
}

type pushSample struct {
	H  string `json:"h"`
	T  int64  `json:"t"`
	R  int32  `json:"r"`
	E  string `json:"e,omitempty"`
	IP string `json:"ip,omitempty"`
}

func (s *Server) handleAgentPush(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, 64<<20)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "gzip", 400)
			return
		}
		defer gz.Close()
		body = gz
	}
	var req struct {
		Samples []pushSample `json:"s"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	s.m.siteSeen(site.ID, clientIP(r), r.Header.Get("X-Agent-Version"))
	s.m.mu.RLock()
	st := s.m.sites[site.ID]
	s.m.mu.RUnlock()
	if st != nil {
		st.pushMu.Lock()
		defer st.pushMu.Unlock()
	}
	now := time.Now().UnixMilli()
	acc := 0
	for _, ps := range req.Samples {
		h := s.m.host(ps.H)
		if h == nil {
			acc++ // Host gelöscht → verwerfen, aber bestätigen
			continue
		}
		h.mu.Lock()
		belongs := h.cfg.Site == site.ID && !h.cfg.Paused
		h.mu.Unlock()
		if !belongs || ps.T > now+60_000 || ps.T < now-int64(memWindow/time.Millisecond)*15 {
			acc++
			continue
		}
		if ps.R < -1 {
			ps.R = -1
		}
		s.m.Ingest(h, ps.T, ps.R, ps.IP, ps.E)
		acc++
	}
	s.m.mu.RLock()
	ver := s.m.cfg.Version
	s.m.mu.RUnlock()
	writeJSON(w, map[string]any{"accepted": acc, "version": ver})
}

// ---------- Exporte ----------

func deNum(r int32) string {
	if r < 0 {
		return ""
	}
	return strings.Replace(strconv.FormatFloat(float64(r)/10, 'f', 1, 64), ".", ",", 1)
}

var typeNames = map[string]string{"outage": "Ausfall", "loss": "Paketverlust", "spike": "Latenzspitze", "offline": "Standort offline"}

func csvHeader(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Write([]byte("\xEF\xBB\xBF")) // BOM für Excel
}

func (s *Server) exportEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	evs := s.m.queryEvents(principal(r).Org, q.Get("host"), q.Get("site"), parseTypes(r), qInt(r, "from", 0), qInt(r, "to", 0), 100000)
	csvHeader(w, "pingwatch-ereignisse.csv")
	fmt.Fprintln(w, "Beginn;Ende;Dauer (s);Host;Typ;Anzahl;Max (ms);Ø (ms)")
	for _, e := range evs {
		end := ""
		dur := ""
		if e.End > 0 {
			end = time.UnixMilli(e.End).Format("02.01.2006 15:04:05")
			dur = strconv.FormatInt((e.End-e.Start)/1000, 10)
		}
		avg := int32(-1)
		if e.Type == "spike" && e.Count > 0 {
			avg = int32(e.Sum / int64(e.Count))
		}
		mx := e.Max
		if e.Type != "spike" {
			mx = -1
		}
		fmt.Fprintf(w, "%s;%s;%s;%s;%s;%d;%s;%s\n", time.UnixMilli(e.Start).Format("02.01.2006 15:04:05"), end, dur,
			strings.ReplaceAll(e.HostName, ";", ","), typeNames[e.Type], e.Count, deNum(mx), deNum(avg))
	}
}

func (s *Server) exportSamples(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	now := time.Now().UnixMilli()
	to := qInt(r, "to", now)
	from := qInt(r, "from", to-24*3600_000)
	if to-from > 31*24*3600_000 {
		from = to - 31*24*3600_000
	}
	s.m.store.Flush()
	csvHeader(w, "pingwatch-"+strings.ReplaceAll(h.cfg.Name, " ", "_")+".csv")
	fmt.Fprintln(w, "Zeit;Antwortzeit (ms);Status")
	status := []string{"OK", "Spitze", "verloren"}
	for d := time.UnixMilli(from); dayStart(d.Format("2006-01-02")).UnixMilli() < to; d = d.AddDate(0, 0, 1) {
		for _, sm := range s.m.store.ReadDay(h.cfg.ID, d.Format("2006-01-02")) {
			if sm.T < from || sm.T >= to {
				continue
			}
			fmt.Fprintf(w, "%s;%s;%s\n", time.UnixMilli(sm.T).Format("02.01.2006 15:04:05"), deNum(sm.R), status[sm.F])
		}
	}
}

func (s *Server) exportStats(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	days := int(qInt(r, "days", 7))
	if days < 1 || days > 366 {
		days = 7
	}
	csvHeader(w, "pingwatch-statistik-"+strings.ReplaceAll(h.cfg.Name, " ", "_")+".csv")
	fmt.Fprintln(w, "Tag;Stunde;Pings;Verloren;Verlust %;Ø (ms);Min (ms);Max (ms);Spitzen")
	for _, d := range s.m.dayStats(h, days) {
		for hr, a := range d.Hours {
			if a.N == 0 {
				continue
			}
			mn, mx := a.Min, a.Max
			if a.N == a.Lost {
				mn, mx = -1, -1
			}
			fmt.Fprintf(w, "%s;%02d:00;%d;%d;%s;%s;%s;%s;%d\n", dayStart(d.Day).Format("02.01.2006"), hr, a.N, a.Lost,
				strings.Replace(strconv.FormatFloat(float64(a.Lost)*100/float64(a.N), 'f', 2, 64), ".", ",", 1),
				deNum(a.Avg()), deNum(mn), deNum(mx), a.Spk)
		}
	}
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	var req struct {
		Host    string `json:"host"`
		Hours   int    `json:"hours"`
		DayFrom string `json:"dayFrom"`
		DayTo   string `json:"dayTo"`
		Samples bool   `json:"samples"`
		Events  bool   `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	if req.Host != "" && s.hostFor(r, req.Host) == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	if !req.Samples && !req.Events {
		http.Error(w, "Messwerte und/oder Ereignisse auswählen", 400)
		return
	}
	now := time.Now()
	var from, to int64
	if req.DayFrom != "" {
		d1, err := time.ParseInLocation("2006-01-02", req.DayFrom, time.Local)
		if err != nil {
			http.Error(w, "Datum ungültig", 400)
			return
		}
		d2 := d1
		if req.DayTo != "" {
			if d2, err = time.ParseInLocation("2006-01-02", req.DayTo, time.Local); err != nil {
				http.Error(w, "Datum ungültig", 400)
				return
			}
		}
		if d2.Before(d1) {
			d1, d2 = d2, d1
		}
		from, to = d1.UnixMilli(), d2.AddDate(0, 0, 1).UnixMilli()
	} else {
		switch req.Hours {
		case 24, 48, 72, 168, 336:
		default:
			http.Error(w, "Zeitraum ungültig", 400)
			return
		}
		from = now.Add(-time.Duration(req.Hours) * time.Hour).UnixMilli()
		to = now.Add(time.Minute).UnixMilli()
	}
	n := s.m.Reset(principal(r).Org, req.Host, from, to, req.Samples, req.Events)
	writeJSON(w, map[string]any{"ok": true, "events": n})
}

// ---------- iPhone-App ----------

func (s *Server) handleAppLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	tok, _, err := s.login(req.Email, req.Password)
	if err != nil {
		time.Sleep(800 * time.Millisecond)
		http.Error(w, err.Error(), 401)
		return
	}
	writeJSON(w, map[string]any{"token": tok, "version": version})
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	m := s.m
	switch r.Method {
	case http.MethodPost:
		var d Device
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || len(d.Token) < 32 {
			http.Error(w, "Gerätetoken fehlt", 400)
			return
		}
		d.Name = strings.TrimSpace(d.Name)
		if d.Name == "" {
			d.Name = "iPhone"
		}
		p := principal(r)
		d.Org = p.Org
		d.AllOrgs = d.AllOrgs && p.Super
		if p.User != nil {
			d.User = p.User.ID
		}
		m.mu.Lock()
		found := false
		for i := range m.cfg.Devices {
			if m.cfg.Devices[i].Token == d.Token {
				d.Added = m.cfg.Devices[i].Added
				m.cfg.Devices[i] = d
				found = true
			}
		}
		if !found {
			d.Added = time.Now().UnixMilli()
			m.cfg.Devices = append(m.cfg.Devices, d)
		}
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "push": m.pusher != nil})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		out := m.cfg.Devices[:0]
		org := principal(r).Org
		for _, d := range m.cfg.Devices {
			if id == "" || !strings.HasPrefix(d.Token, id) || (d.Org != org && !principal(r).Super) {
				out = append(out, d)
			}
		}
		m.cfg.Devices = out
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		tok := r.URL.Query().Get("token")
		m.mu.RLock()
		defer m.mu.RUnlock()
		for _, d := range m.cfg.Devices {
			if d.Token == tok {
				writeJSON(w, d)
				return
			}
		}
		http.NotFound(w, r)
	}
}

// Testbenachrichtigung an alle Geräte
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	if s.m.pusher == nil {
		http.Error(w, "Push nicht eingerichtet (APNS_KEY, APNS_KEY_ID, APNS_TEAM_ID fehlen)", 400)
		return
	}
	org := principal(r).Org
	s.m.mu.RLock()
	var devs []Device
	for _, d := range s.m.cfg.Devices {
		if d.Org == org {
			devs = append(devs, d)
		}
	}
	s.m.mu.RUnlock()
	ok, errs := 0, []string{}
	for _, d := range devs {
		if err := s.m.pusher.send(d, notice{"test", "", "PingWatch", "Testbenachrichtigung – Push funktioniert ✅", ""}); err != nil {
			errs = append(errs, d.Name+": "+err.Error())
		} else {
			ok++
		}
	}
	writeJSON(w, map[string]any{"sent": ok, "errors": errs})
}

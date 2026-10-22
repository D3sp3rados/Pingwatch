package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type AgentFile struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	Site   string `json:"site,omitempty"`
	// "sysonly": nur diesen Rechner überwachen (keine Hosts), z. B. auf weiteren Servern/PCs
	Mode string `json:"mode,omitempty"`
	// true: dieser Agent startet niemals Dienste neu, auch wenn es im Dashboard erlaubt ist
	NoSelfHeal bool `json:"noSelfHeal,omitempty"`
	// true: dieser Agent installiert nie Updates und startet den Rechner nie neu (auch wenn im Dashboard erlaubt)
	NoActions bool `json:"noActions,omitempty"`
	// Ordner für Datenbank-Sicherungen (leer = <Datenordner>/backups)
	BackupDir string `json:"backupDir,omitempty"`
}

type agentHost struct {
	cfg    HostCfg
	stop   chan struct{}
	consec int
	down   bool
	lastR  int32
	lastT  int64
	lastE  string
}

type Agent struct {
	file     AgentFile
	dataDir  string
	mu       sync.Mutex
	buf      []pushSample
	hosts    map[string]*agentHost
	siteName string
	unifi    *UniFiCfg
	cfgVer   int64
	serverOK bool
	lastOK   time.Time
	lastErr  string
	refresh  chan struct{}
	stopAll  chan struct{}
	notify   func(title, msg string, warn bool)
	changed  func()
	client   *http.Client
	// Aktualisierung
	latest        string // Version auf dem Server
	autoUpdate    bool
	requestExit   func() // Oberfläche beenden (Windows-Tray)
	lastAutoCheck time.Time
	// Datenbank-Sicherungen auf diesem Agenten
	backupOn   bool
	backupKeep int
	// Rechner-Überwachung
	sysOnly bool
	sysOn   bool
	sysSvcs []string
	sysErr  string
	machine string
	// Selbstheilung (nur wenn am Standort erlaubt und nicht in agent.json abgeschaltet)
	sysHeal    bool
	sysActions bool // Updates/Neustart erlaubt (Dashboard und agent.json)
	sysRestart []string
	invMu      sync.Mutex
	invBusy    bool
	invAt      time.Time
}

const maxBuffer = 2_000_000

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

func loadAgentFile(paths ...string) (AgentFile, string, error) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var af AgentFile
		if err := json.Unmarshal(b, &af); err != nil {
			return af, p, fmt.Errorf("%s ist fehlerhaft: %v", p, err)
		}
		af.Server = strings.TrimRight(strings.TrimSpace(af.Server), "/")
		if af.Server == "" || af.Token == "" {
			return af, p, fmt.Errorf("%s: server oder token fehlt", p)
		}
		return af, p, nil
	}
	return AgentFile{}, "", errors.New("agent.json nicht gefunden")
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Pfad zur agent.json")
	dataFlag := fs.String("data", "", "Datenverzeichnis")
	sysOnlyFlag := fs.Bool("sysonly", false, "nur diesen Rechner überwachen (keine Hosts prüfen)")
	fs.Parse(args)
	dataDir := *dataFlag
	if dataDir == "" {
		dataDir = agentDataDir()
	}
	os.MkdirAll(dataDir, 0o755)
	if lf, err := os.OpenFile(filepath.Join(dataDir, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		if fi, _ := lf.Stat(); fi != nil && fi.Size() > 5<<20 {
			lf.Truncate(0)
		}
		log.SetOutput(io.MultiWriter(lf, os.Stderr))
	}
	// nach einem Update kurz warten, bis die alte Version beendet ist
	if os.Getenv("PINGWACHT_UPDATED") == "1" {
		os.Unsetenv("PINGWACHT_UPDATED")
		for i := 0; i < 60 && !singleInstance(); i++ {
			time.Sleep(500 * time.Millisecond)
		}
		log.Printf("Agent nach Update gestartet (Version %s)", version)
	} else if !singleInstance() {
		showError("PingWacht-Agent läuft bereits (siehe Infobereich der Taskleiste).")
		return
	}
	// agent.json per Drag & Drop auf die exe oder als Argument → ins Datenverzeichnis übernehmen
	for _, a := range fs.Args() {
		if strings.HasSuffix(strings.ToLower(a), ".json") {
			if b, err := os.ReadFile(a); err == nil {
				os.WriteFile(filepath.Join(dataDir, "agent.json"), b, 0o600)
			}
		}
	}
	af, _, err := loadAgentFile(*cfgPath, filepath.Join(exeDir(), "agent.json"), filepath.Join(dataDir, "agent.json"))
	if err != nil {
		showError("PingWacht-Agent: " + err.Error() + "\n\nIm Dashboard unter Einstellungen → Standorte die Datei agent.json herunterladen und neben die pingwatch-agent.exe legen (oder auf die exe ziehen).")
		return
	}
	// Modus „nur Rechner“: Startparameter hat Vorrang, sonst gemerkter Modus aus dem Datenverzeichnis
	flagSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "sysonly" {
			flagSet = true
		}
	})
	if flagSet {
		af.Mode = ""
		if *sysOnlyFlag {
			af.Mode = "sysonly"
		}
	} else if af.Mode == "" {
		if prev, _, err := loadAgentFile(filepath.Join(dataDir, "agent.json")); err == nil && prev.Mode == "sysonly" && prev.Token == af.Token {
			af.Mode = "sysonly"
		}
	}
	// Kopie im Datenverzeichnis, damit Autostart auch ohne Datei neben der exe klappt
	if b, err := json.MarshalIndent(af, "", "  "); err == nil {
		os.WriteFile(filepath.Join(dataDir, "agent.json"), b, 0o600)
	}
	a := &Agent{
		file: af, dataDir: dataDir, hosts: map[string]*agentHost{}, siteName: af.Site,
		refresh: make(chan struct{}, 1), stopAll: make(chan struct{}),
		notify: func(string, string, bool) {}, changed: func() {},
		client:  &http.Client{Timeout: 25 * time.Second},
		sysOnly: af.Mode == "sysonly", machine: sysMachineID(dataDir),
	}
	a.loadPending()
	log.Printf("PingWacht-Agent %s, Server %s", version, af.Server)
	if a.sysOnly {
		log.Printf("Modus: nur Rechner-Überwachung (keine Hosts)")
	}
	go a.configLoop()
	go a.unifiLoop()
	go a.sendLoop()
	go a.sysLoop()
	go a.backupLoop()
	agentUI(a)
	a.shutdown()
}

func (a *Agent) req(method, path string, body io.Reader, gz bool) (*http.Response, error) {
	r, err := http.NewRequest(method, a.file.Server+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+a.file.Token)
	r.Header.Set("X-Agent-Version", version)
	if a.machine != "" {
		r.Header.Set("X-Machine", a.machine) // für rechnerbezogene Einstellungen (z. B. eigene Dienste)
	}
	if a.sysOnly {
		r.Header.Set("X-Agent-Mode", "sysonly")
	}
	if gz {
		r.Header.Set("Content-Encoding", "gzip")
		r.Header.Set("Content-Type", "application/json")
	}
	return a.client.Do(r)
}

func (a *Agent) setServer(ok bool, errText string) {
	a.mu.Lock()
	prev := a.serverOK
	a.serverOK = ok
	if ok {
		a.lastOK = time.Now()
		a.lastErr = ""
	} else {
		a.lastErr = errText
	}
	a.mu.Unlock()
	if prev != ok {
		if ok {
			log.Printf("Server erreichbar")
		} else {
			log.Printf("Server nicht erreichbar: %s", errText)
		}
		a.changed()
	}
}

func (a *Agent) configLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		ok := a.fetchConfig()
		wait := t.C
		var retry <-chan time.Time
		if !ok {
			retry = time.After(5 * time.Second)
		}
		select {
		case <-a.stopAll:
			return
		case <-wait:
		case <-retry:
		case <-a.refresh:
		}
	}
}

func (a *Agent) fetchConfig() bool {
	resp, err := a.req("GET", "/api/agent/config", nil, false)
	if err != nil {
		a.setServer(false, err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		a.setServer(false, "Token ungültig – agent.json neu herunterladen")
		return false
	}
	if resp.StatusCode != 200 {
		a.setServer(false, resp.Status)
		return false
	}
	var cfg struct {
		Site struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"site"`
		Hosts   []HostCfg  `json:"hosts"`
		Version int64      `json:"version"`
		Jobs    []agentJob `json:"jobs"`
		UniFi   *UniFiCfg  `json:"unifi"`
		Latest  string     `json:"latest"`
		Auto    *bool      `json:"autoUpdate"`
		Backup  *struct {
			On   bool `json:"on"`
			Keep int  `json:"keep"`
		} `json:"backup"`
		Sys *struct {
			On       bool     `json:"on"`
			Services []string `json:"services"`
			SelfHeal bool     `json:"selfHeal"`
			Restart  []string `json:"restart"`
			Updates  bool     `json:"updates"`
		} `json:"sys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		a.setServer(false, err.Error())
		return false
	}
	for _, j := range cfg.Jobs {
		go a.runJob(j)
	}
	a.mu.Lock()
	a.latest = cfg.Latest
	a.autoUpdate = cfg.Auto == nil || *cfg.Auto
	auto := a.autoUpdate
	a.mu.Unlock()
	// automatische Updates nur stündlich prüfen (manuell per Knopf sofort)
	if auto && cfg.Latest != "" && time.Since(a.lastAutoCheck) >= time.Hour {
		a.lastAutoCheck = time.Now()
		if newerVersion(cfg.Latest, version) {
			go a.selfUpdate(cfg.Latest, false)
		}
	}
	a.setServer(true, "")
	a.mu.Lock()
	a.siteName = cfg.Site.Name
	if a.sysOnly {
		a.siteName += " (nur Rechner)"
	}
	a.sysOn, a.sysSvcs, a.sysHeal, a.sysRestart = cfg.Sys != nil && cfg.Sys.On, nil, false, nil
	a.sysActions = cfg.Sys != nil && cfg.Sys.On && cfg.Sys.Updates && !a.file.NoActions
	if cfg.Sys != nil {
		a.sysSvcs = cfg.Sys.Services
		a.sysHeal, a.sysRestart = cfg.Sys.SelfHeal && !a.file.NoSelfHeal, cfg.Sys.Restart
	}
	a.backupOn, a.backupKeep = cfg.Backup != nil && cfg.Backup.On, 14
	if cfg.Backup != nil && cfg.Backup.Keep > 0 {
		a.backupKeep = cfg.Backup.Keep
	}
	a.unifi = cfg.UniFi
	if a.sysOnly {
		a.unifi, cfg.Hosts = nil, nil
	}
	a.cfgVer = cfg.Version
	want := map[string]HostCfg{}
	for _, hc := range cfg.Hosts {
		hc.normalize()
		want[hc.ID] = hc
	}
	for id, ah := range a.hosts {
		if hc, ok := want[id]; !ok || hc != ah.cfg {
			close(ah.stop)
			delete(a.hosts, id)
		}
	}
	for id, hc := range want {
		if _, ok := a.hosts[id]; !ok {
			ah := &agentHost{cfg: hc, stop: make(chan struct{}), lastR: -1}
			a.hosts[id] = ah
			go a.worker(ah)
		}
	}
	a.mu.Unlock()
	a.changed()
	return true
}

func (a *Agent) worker(ah *agentHost) {
	if ah.cfg.Method == "snmp" {
		go a.snmpLoop(ah)
	}
	if ah.cfg.Fritz {
		go a.fritzLoop(ah)
	}
	res := &resolver{addr: ah.cfg.Addr}
	tk := time.NewTicker(time.Duration(ah.cfg.IntervalMs) * time.Millisecond)
	defer tk.Stop()
	for {
		t := time.Now().UnixMilli()
		r, ip, e := probe(ah.cfg, res)
		select {
		case <-ah.stop:
			return
		default:
		}
		a.record(ah, pushSample{H: ah.cfg.ID, T: t, R: r, E: e, IP: ip, C: certExpiry(ah.cfg.ID)})
		select {
		case <-ah.stop:
			return
		case <-tk.C:
		}
	}
}

func (a *Agent) record(ah *agentHost, s pushSample) {
	a.mu.Lock()
	if len(a.buf) >= maxBuffer {
		a.buf = append([]pushSample(nil), a.buf[maxBuffer/10:]...)
	}
	a.buf = append(a.buf, s)
	ah.lastR, ah.lastT, ah.lastE = s.R, s.T, s.E
	var note, msg string
	warn := false
	if s.R < 0 {
		ah.consec++
		if ah.consec == ah.cfg.OutageCount && !ah.down {
			ah.down = true
			note = ah.cfg.Name + " antwortet nicht"
			msg = fmt.Sprintf("%s (%s) – seit %s keine Antwort", ah.cfg.Name, ah.cfg.Addr, time.Now().Add(-time.Duration(ah.consec*ah.cfg.IntervalMs)*time.Millisecond).Format("15:04:05"))
			warn = true
		}
	} else {
		if ah.down {
			note = ah.cfg.Name + " wieder erreichbar"
			msg = fmt.Sprintf("%s antwortet wieder (%s ms) nach %d verlorenen Pings", ah.cfg.Name, fmtR(s.R), ah.consec)
		}
		ah.consec = 0
		ah.down = false
	}
	a.mu.Unlock()
	if note != "" {
		log.Printf("%s: %s", note, msg)
		a.notify(note, msg, warn)
		a.changed()
	}
}

func (a *Agent) sendLoop() {
	backoff := 2 * time.Second
	lastSave := time.Now()
	for {
		select {
		case <-a.stopAll:
			return
		case <-time.After(backoff):
		}
		for {
			a.mu.Lock()
			n := len(a.buf)
			if n > 5000 {
				n = 5000
			}
			batch := append([]pushSample(nil), a.buf[:n]...)
			a.mu.Unlock()
			if n == 0 {
				break
			}
			if err := a.push(batch); err != nil {
				a.setServer(false, err.Error())
				if backoff < 30*time.Second {
					backoff *= 2
				}
				if time.Since(lastSave) > time.Minute {
					a.savePending()
					lastSave = time.Now()
				}
				break
			}
			backoff = 2 * time.Second
			a.mu.Lock()
			a.buf = a.buf[n:]
			if len(a.buf) == 0 {
				a.buf = nil
			}
			more := len(a.buf) > 0
			a.mu.Unlock()
			a.setServer(true, "")
			if !more {
				os.Remove(filepath.Join(a.dataDir, "pending.json"))
				break
			}
		}
	}
}

func (a *Agent) push(batch []pushSample) error {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	json.NewEncoder(gz).Encode(map[string]any{"s": batch})
	gz.Close()
	resp, err := a.req("POST", "/api/agent/push", &b, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 {
			return errors.New("Token ungültig – agent.json neu herunterladen")
		}
		return errors.New(resp.Status)
	}
	var r struct {
		Version int64      `json:"version"`
		Traces  []traceReq `json:"traces"`
		Jobs    []agentJob `json:"jobs"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	for _, t := range r.Traces {
		go a.trace(t)
	}
	for _, j := range r.Jobs {
		go a.runJob(j)
	}
	a.mu.Lock()
	changed := r.Version != 0 && r.Version != a.cfgVer
	a.mu.Unlock()
	if changed {
		select {
		case a.refresh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (a *Agent) savePending() {
	a.mu.Lock()
	cp := append([]pushSample(nil), a.buf...)
	a.mu.Unlock()
	if len(cp) == 0 {
		return
	}
	p := filepath.Join(a.dataDir, "pending.json")
	f, err := os.Create(p + ".tmp")
	if err != nil {
		return
	}
	gz := gzip.NewWriter(f)
	json.NewEncoder(gz).Encode(cp)
	gz.Close()
	f.Close()
	os.Rename(p+".tmp", p)
}

func (a *Agent) loadPending() {
	f, err := os.Open(filepath.Join(a.dataDir, "pending.json"))
	if err != nil {
		return
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return
	}
	var cp []pushSample
	if json.NewDecoder(gz).Decode(&cp) == nil {
		sort.SliceStable(cp, func(i, j int) bool { return cp[i].T < cp[j].T })
		a.buf = cp
		log.Printf("%d gepufferte Messwerte geladen", len(cp))
	}
}

func (a *Agent) shutdown() {
	close(a.stopAll)
	a.mu.Lock()
	for _, ah := range a.hosts {
		close(ah.stop)
	}
	a.hosts = map[string]*agentHost{}
	a.mu.Unlock()
	// letzter Versuch, Puffer zu senden
	a.mu.Lock()
	n := len(a.buf)
	if n > 5000 {
		n = 5000
	}
	batch := append([]pushSample(nil), a.buf[:n]...)
	a.mu.Unlock()
	if n > 0 && a.push(batch) == nil {
		a.mu.Lock()
		a.buf = a.buf[n:]
		a.mu.Unlock()
	}
	a.savePending()
	if len(a.buf) == 0 {
		os.Remove(filepath.Join(a.dataDir, "pending.json"))
	}
}

// Zustand für Tray/Konsole
type agentStatus struct {
	Site      string
	ServerOK  bool
	ServerErr string
	Pending   int
	Total     int
	Down      []string
	Loss      int
}

func (a *Agent) status() agentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := agentStatus{Site: a.siteName, ServerOK: a.serverOK, ServerErr: a.lastErr, Pending: len(a.buf), Total: len(a.hosts)}
	for _, ah := range a.hosts {
		if ah.down {
			st.Down = append(st.Down, ah.cfg.Name)
		} else if ah.lastR < 0 && ah.lastT > 0 {
			st.Loss++
		}
	}
	sort.Strings(st.Down)
	return st
}

// trace führt eine vom Server angeforderte Routenverfolgung aus und schickt das Ergebnis zurück.
func (a *Agent) trace(t traceReq) {
	text := runTrace(t.Addr)
	b, _ := json.Marshal(map[string]any{"id": t.ID, "text": text})
	for try := 0; try < 3; try++ {
		resp, err := a.req("POST", "/api/agent/trace", bytes.NewReader(b), false)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
}

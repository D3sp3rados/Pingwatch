package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	memWindow = 48 * time.Hour
	mergeGap  = int64(60_000) // ms: Verluste/Spitzen innerhalb 60 s = ein Ereignis
	baseSize  = 300
)

type HostCfg struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Addr        string `json:"addr"`
	Site        string `json:"site"` // "cloud" oder Standort-ID
	Method      string `json:"method"`
	Port        int    `json:"port"`
	IntervalMs  int    `json:"intervalMs"`
	TimeoutMs   int    `json:"timeoutMs"`
	ThresholdMs int    `json:"thresholdMs"` // 0 = automatisch
	OutageCount int    `json:"outageCount"`
	Paused      bool   `json:"paused"`
	Org         string `json:"org,omitempty"`
	Parent      string `json:"parent,omitempty"`     // hängt ab von (z. B. Router) – keine Alarme, wenn dieser ausgefallen ist
	Expect      string `json:"expect,omitempty"`     // http: Statuscode oder Text; dns: erwartete Adresse
	DNSServer   string `json:"dnsServer,omitempty"`  // dns: abzufragender Server (leer = System)
	Community   string `json:"community,omitempty"`  // snmp: Community (v2c)
	Watch       string `json:"watch,omitempty"`      // snmp: überwachte Ports (ifIndex, Komma-getrennt)
	Group       string `json:"group,omitempty"`      // Gruppe/Ordner (z. B. Endkunde)
	PauseUntil  int64  `json:"pauseUntil,omitempty"` // pausiert bis (ms), 0 = unbegrenzt
	// Fritzbox per TR-064 über den Agenten auslesen (Internet-Status, Sync, Abbrüche)
	Fritz     bool   `json:"fritz,omitempty"`
	FritzPort int    `json:"fritzPort,omitempty"` // 49000 (http) oder 49443 (https)
	FritzUser string `json:"fritzUser,omitempty"`
	FritzPass string `json:"fritzPass,omitempty"` // nur an den Agenten, nie an den Browser
	// FritzNewDev: neue Geräte im Heimnetz der Fritzbox melden
	FritzNewDev bool `json:"fritzNewDev,omitempty"`
	// FritzFullNums: Rufnummern vollständig anzeigen (sonst nur die letzten 4 Ziffern)
	FritzFullNums bool `json:"fritzFullNums,omitempty"`
	// MikroTik über die REST-API (RouterOS v7) durch den Agenten auslesen
	Mt     bool   `json:"mt,omitempty"`
	MtUser string `json:"mtUser,omitempty"`
	MtPass string `json:"mtPass,omitempty"` // nur an den Agenten, nie an den Browser
	MtPort int    `json:"mtPort,omitempty"` // 443 (https, Standard) oder 80 (http)
	// Temperatur-Alarm (CPU) ab °C: 0 = Standard (75), -1 = aus
	MtTempMax int `json:"mtTempMax,omitempty"`
	// Alarm, wenn ein überwachter Port langsamer verbindet (z. B. 1 G → 100 M); Standard an
	NoSpeedAlarm bool `json:"noSpeedAlarm,omitempty"`
}

type SiteCfg struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Token string    `json:"token"`
	Org   string    `json:"org,omitempty"`
	UniFi *UniFiCfg `json:"unifi,omitempty"` // UniFi-Livestatus über den Agenten oder die UniFi-Cloud
	// "unifi" = Standort ohne Agent, nur UniFi über die Cloud
	Kind string `json:"kind,omitempty"`
	// Agent nicht automatisch aktualisieren
	NoAutoUpdate bool  `json:"noAutoUpdate,omitempty"`
	SetAuto      *bool `json:"setAutoUpdate,omitempty"` // nur API-Eingabe
	CheckUpdate  bool  `json:"checkUpdate,omitempty"`   // nur API-Eingabe: „Jetzt prüfen“
	// UpdateNow: Zeitpunkt von „Jetzt prüfen“ – alle Agenten des Standorts prüfen dann sofort auf Updates
	UpdateNow int64 `json:"updateNow,omitempty"`
	// Rechner-Überwachung (CPU, RAM, Laufwerke, Dienste …)
	Sys *SysCfg `json:"sys,omitempty"`
	// Zuordnung zu einem Kunden des Kontos und eigene Kundennummer des Standorts
	Customer    string  `json:"customer,omitempty"`
	Number      string  `json:"number,omitempty"`
	SetCustomer *string `json:"setCustomer,omitempty"` // nur API-Eingabe
	SetNumber   *string `json:"setNumber,omitempty"`   // nur API-Eingabe
	// zuletzt gemeldete Agent-Version und Zeitpunkt des letzten Versionswechsels (Update)
	AgentVer     string `json:"agentVer,omitempty"`
	AgentUpdated int64  `json:"agentUpdated,omitempty"`
	AgentPrev    string `json:"agentPrev,omitempty"`
}

type Config struct {
	Hosts         []HostCfg    `json:"hosts"`
	Sites         []SiteCfg    `json:"sites"`
	RetentionDays int          `json:"retentionDays"`
	PassHash      string       `json:"passHash"`
	Devices       []Device     `json:"devices"`
	Orgs          []Org        `json:"orgs"`
	Users         []User       `json:"users"`
	Channels      []Channel    `json:"channels"`
	Maints        []Maint      `json:"maints"`
	Groups        []Group      `json:"groups"`
	Maps          []NetMap     `json:"maps,omitempty"`
	Teams         []TeamList   `json:"teams,omitempty"`
	Customers     []Customer   `json:"customers,omitempty"` // Kunden je Konto (ordnen Standorte)
	Actions       []ActionLog  `json:"actions,omitempty"`   // Protokoll: Updates und Neustarts über PingWacht
	UpdatePlans   []UpdatePlan `json:"updatePlans,omitempty"`
	Scripts       []Script     `json:"scripts,omitempty"`
	// Zwei-Faktor-Code des Betreiber-Logins (ADMIN_PASSWORD)
	AdminTOTP        string            `json:"adminTotp,omitempty"`
	AdminTOTPPending string            `json:"adminTotpPending,omitempty"`
	AdminPasskeys    []Passkey         `json:"adminPasskeys,omitempty"`
	Requests         []FeatureRequest  `json:"requests,omitempty"` // Änderungswünsche aus dem KI-Assistenten
	Machines         []MachineCfg      `json:"machines,omitempty"` // überwachte Rechner (Rechner-Überwachung)
	Acs              []AcsDevice       `json:"acs,omitempty"`      // TR-069: Fritzboxen am ACS (Prototyp)
	AcsProfiles      []AcsCodeProfile  `json:"acsProfiles,omitempty"`
	VapidKey         string            `json:"vapidKey,omitempty"`  // Web-Push-Schlüssel (privat)
	UniFiKeys        map[string]string `json:"unifiKeys,omitempty"` // Kunde → UniFi-Cloud-Key (Site Manager)
	// Datenbank-Sicherungen auf Agenten (Standort → Einstellung / letzter Stand)
	BackupTargets map[string]*BackupCfg    `json:"backupTargets,omitempty"`
	BackupStatus  map[string]*BackupStatus `json:"backupStatus,omitempty"`
	Secret        string                   `json:"secret"`
	Version       int64                    `json:"version"`
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *HostCfg) normalize() {
	h.Addr = strings.TrimSpace(h.Addr)
	if h.Addr == "" { // ohne Adresse nur als pausierter Platzhalter
		h.Paused, h.PauseUntil = true, 0
	}
	if h.IntervalMs < 500 {
		h.IntervalMs = 1000
	}
	if h.IntervalMs > 300000 {
		h.IntervalMs = 300000
	}
	if h.TimeoutMs <= 0 {
		h.TimeoutMs = 1000
	}
	if h.TimeoutMs > 10000 {
		h.TimeoutMs = 10000
	}
	if h.OutageCount < 1 {
		h.OutageCount = 3
	}
	switch h.Method {
	case "tcp":
		if h.Port <= 0 || h.Port > 65535 {
			h.Port = 443
		}
	case "http":
		h.Addr = normalizeURL(h.Addr)
		h.Port = 0
		if h.IntervalMs < 30000 {
			h.IntervalMs = 30000
		}
		if h.TimeoutMs < 5000 {
			h.TimeoutMs = 10000
		}
	case "snmp":
		if h.Port <= 0 || h.Port > 65535 {
			h.Port = 161
		}
		if h.Community == "" {
			h.Community = "public"
		}
		if h.IntervalMs < 10000 {
			h.IntervalMs = 10000
		}
		if h.TimeoutMs < 1000 {
			h.TimeoutMs = 2000
		}
	case methodUniFiWAN:
		h.Port, h.IntervalMs, h.TimeoutMs = 0, 60000, 1000
	case "dns":
		h.Port = 0
		if h.IntervalMs < 10000 {
			h.IntervalMs = 10000
		}
		if h.TimeoutMs < 2000 {
			h.TimeoutMs = 3000
		}
	default:
		h.Method = "icmp"
		h.Port = 0
	}
	if h.Method != "http" && h.Method != "dns" {
		h.Expect, h.DNSServer = "", ""
	}
	if !h.Mt || h.Site == "cloud" || h.Site == "" {
		h.Mt, h.MtUser, h.MtPass, h.MtPort, h.MtTempMax = false, "", "", 0, 0
	} else if h.MtPort <= 0 || h.MtPort > 65535 {
		h.MtPort = 443
	}
	if h.Method != "snmp" {
		h.Community = ""
		if !h.Mt {
			h.Watch = ""
		}
	}
	if h.Site == "" {
		h.Site = "cloud"
	}
	if !h.Fritz || h.Site == "cloud" {
		h.Fritz, h.FritzPort, h.FritzUser, h.FritzPass, h.FritzNewDev, h.FritzFullNums = false, 0, "", "", false, false
	} else if h.FritzPort != 49443 {
		h.FritzPort = 49000
	}
	if h.ThresholdMs < 0 {
		h.ThresholdMs = 0
	}
}

type Host struct {
	mu      sync.Mutex
	cfg     HostCfg
	samples []Sample
	lastR   int32
	lastT   int64
	lastF   uint8
	lastErr string
	ip      string
	consec  int
	stStart int64
	stLast  int64
	base    [baseSize]int32
	baseN   int
	basePos int
	baseMed int32
	baseCnt int
	oOut    *Event
	oLoss   *Event
	oSpk    *Event
	stop    chan struct{}
}

func (h *Host) addBase(r int32) {
	h.base[h.basePos] = r
	h.basePos = (h.basePos + 1) % baseSize
	if h.baseN < baseSize {
		h.baseN++
	}
	h.baseCnt++
	if h.baseCnt%10 == 1 || h.baseN < 30 {
		tmp := make([]int32, h.baseN)
		copy(tmp, h.base[:h.baseN])
		sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
		h.baseMed = tmp[len(tmp)/2]
	}
}

// effThreshold in Zehntel-ms, 0 = noch keine Grundlage
func (h *Host) effThreshold() int32 {
	if h.cfg.ThresholdMs > 0 {
		return int32(h.cfg.ThresholdMs) * 10
	}
	if h.baseN < 30 {
		return 0
	}
	t := h.baseMed * 3
	if alt := h.baseMed + 300; alt > t {
		t = alt
	}
	return t
}

type siteState struct {
	mu       sync.Mutex
	lastSeen int64
	ip       string
	version  string
	offEv    *Event
	pushMu   sync.Mutex
}

type Monitor struct {
	mu      sync.RWMutex
	cfg     Config
	hosts   map[string]*Host
	sites   map[string]*siteState
	store   Storage
	evMu    sync.Mutex
	events  []*Event
	nextEv  int64
	started int64
	icmpOK  string
	pusher  *Pusher
	pushErr string // Grund, warum Apple-Push nicht aktiv ist
	notices chan notice
}

func NewMonitor(store Storage) *Monitor {
	m := &Monitor{
		hosts:   map[string]*Host{},
		sites:   map[string]*siteState{},
		store:   store,
		started: time.Now().UnixMilli(),
	}
	if b, err := store.LoadConfig(); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &m.cfg); err != nil {
			log.Fatalf("Konfiguration fehlerhaft: %v", err)
		}
	}
	if m.cfg.RetentionDays <= 0 {
		m.cfg.RetentionDays = 60
	}
	if m.cfg.Secret == "" {
		m.cfg.Secret = randHex(32)
	}
	m.migrateTenancy()
	m.events = m.store.LoadEvents(m.cfg.RetentionDays)
	m.fixEventOrgs()
	for _, e := range m.events {
		if e.ID >= m.nextEv {
			m.nextEv = e.ID
		}
	}
	for _, s := range m.cfg.Sites {
		m.sites[s.ID] = &siteState{}
	}
	m.saveConfigLocked()
	return m
}

func (m *Monitor) saveConfigLocked() {
	if storeFrozen.Load() {
		return // Wiederherstellung läuft – nichts mehr überschreiben
	}
	m.cfg.Version = time.Now().UnixNano()
	b, _ := json.MarshalIndent(m.cfg, "", "  ")
	if err := m.store.SaveConfig(b); err != nil {
		log.Printf("Konfiguration speichern fehlgeschlagen: %v", err)
	}
}

// Start lädt die letzten 48 h aus den Dateien und startet die Cloud-Pinger.
func (m *Monitor) Start() {
	m.mu.Lock()
	for _, hc := range m.cfg.Hosts {
		h := &Host{cfg: hc, lastR: -1}
		m.loadMemory(h)
		m.hosts[hc.ID] = h
		if hc.Site == "cloud" && !hc.Paused {
			m.startWorker(h)
		}
	}
	m.mu.Unlock()
	m.loadSysState()
	m.loadProtectState()
	m.loadMapReach()
	go m.housekeeping()
}

func (m *Monitor) loadMemory(h *Host) {
	now := time.Now()
	from := now.Add(-memWindow).UnixMilli()
	for d := now.Add(-memWindow); !d.After(now.Add(24 * time.Hour)); d = d.AddDate(0, 0, 1) {
		for _, sm := range m.store.ReadDay(h.cfg.ID, d.Format("2006-01-02")) {
			if sm.T >= from {
				h.samples = append(h.samples, sm)
			}
		}
	}
	sort.Slice(h.samples, func(i, j int) bool { return h.samples[i].T < h.samples[j].T })
	for _, sm := range h.samples {
		if sm.R >= 0 {
			h.addBase(sm.R)
		}
	}
	if n := len(h.samples); n > 0 {
		l := h.samples[n-1]
		h.lastR, h.lastT, h.lastF = l.R, l.T, l.F
	}
}

func (m *Monitor) startWorker(h *Host) {
	stop := make(chan struct{})
	h.stop = stop
	cfg := h.cfg
	if cfg.Method == "snmp" {
		go m.snmpLoopCloud(h, cfg, stop)
	}
	go func() {
		res := &resolver{addr: cfg.Addr}
		tk := time.NewTicker(time.Duration(cfg.IntervalMs) * time.Millisecond)
		defer tk.Stop()
		for {
			t := time.Now().UnixMilli()
			r, ip, e := probe(cfg, res)
			select {
			case <-stop:
				return
			default:
			}
			m.Ingest(h, t, r, ip, e)
			select {
			case <-stop:
				return
			case <-tk.C:
			}
		}
	}()
}

func (m *Monitor) newEvent(h *Host, typ string, start int64) *Event {
	m.evMu.Lock()
	m.nextEv++
	e := &Event{ID: m.nextEv, HostID: h.cfg.ID, HostName: h.cfg.Name, Site: h.cfg.Site, Org: h.cfg.Org, Type: typ, Start: start, Last: start}
	// Messwerte, die ein Agent nach einer Unterbrechung nachliefert: nicht melden
	// (für die Zeit gab es schon „Agent nicht erreichbar“). Läuft der Ausfall noch, wird er
	// mit dem ersten aktuellen Messwert nachgemeldet (lateNotify).
	if t := max(start, h.stLast); t > 0 && time.Now().UnixMilli()-t > lateAfter {
		e.Late = true
	}
	m.events = append(m.events, e)
	m.evMu.Unlock()
	// Achtung: Aufrufer hält h.mu – darum Wartung/Abhängigkeit/Meldung asynchron prüfen.
	go m.eventStarted(e, h)
	return e
}

const lateAfter = 120_000 // ms

// lateNotify: Ausfall aus nachgelieferten Messwerten dauert noch an → jetzt melden.
func (m *Monitor) lateNotify(e *Event) {
	for i := 0; i < 400; i++ { // warten, bis Wartung/Abhängigkeit geprüft sind
		m.evMu.Lock()
		d := e.Decided
		m.evMu.Unlock()
		if d {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	m.evMu.Lock()
	if !e.Late || e.End != 0 || e.Maint || e.Cause != "" {
		m.evMu.Unlock()
		return
	}
	e.Late, e.Notified = false, true
	cp := *e
	m.evMu.Unlock()
	m.notifyEventStart(cp)
}

func (m *Monitor) finishEvent(e *Event, end int64) {
	m.evMu.Lock()
	e.End = end
	cp := *e
	m.evMu.Unlock()
	m.store.AppendEvent(cp)
	if cp.Decided || cp.Type == "offline" {
		m.notifyEventEnd(cp)
		return
	}
	// Beginn wird noch geprüft (Wartung/Abhängigkeit) – Ende erst danach melden.
	go func() {
		for i := 0; i < 400; i++ {
			time.Sleep(100 * time.Millisecond)
			m.evMu.Lock()
			d := e.Decided
			cp = *e
			m.evMu.Unlock()
			if d {
				break
			}
		}
		m.notifyEventEnd(cp)
	}()
}

// finishEventQuiet schließt ein Ereignis ohne Benachrichtigung (Pause, Löschen, Neustart).
func (m *Monitor) finishEventQuiet(e *Event, end int64) {
	m.evMu.Lock()
	e.End = end
	cp := *e
	m.evMu.Unlock()
	m.store.AppendEvent(cp)
}

// Ingest verarbeitet einen Messwert (lokal oder vom Agenten).
func (m *Monitor) Ingest(h *Host, t int64, r int32, ip, errStr string) {
	h.mu.Lock()
	thr := h.effThreshold()
	var f uint8
	if r < 0 {
		f = 2
	} else if thr > 0 && r > thr {
		f = 1
	}
	sm := Sample{T: t, R: r, F: f}
	n := len(h.samples)
	if n == 0 || h.samples[n-1].T <= t {
		h.samples = append(h.samples, sm)
	} else {
		i := sort.Search(n, func(i int) bool { return h.samples[i].T > t })
		h.samples = append(h.samples, Sample{})
		copy(h.samples[i+1:], h.samples[i:])
		h.samples[i] = sm
	}
	if t >= h.lastT {
		h.lastR, h.lastT, h.lastF, h.lastErr = r, t, f, errStr
		if ip != "" {
			h.ip = ip
		}
	}
	if r >= 0 {
		h.addBase(r)
	}

	var done []*Event
	m.evMu.Lock()
	if h.oLoss != nil && t-h.oLoss.Last > mergeGap {
		done = append(done, h.oLoss)
		h.oLoss = nil
	}
	if h.oSpk != nil && t-h.oSpk.Last > mergeGap {
		done = append(done, h.oSpk)
		h.oSpk = nil
	}
	m.evMu.Unlock()

	if r < 0 {
		h.consec++
		if h.consec == 1 {
			h.stStart = t
		}
		h.stLast = t
		if h.consec >= h.cfg.OutageCount {
			if h.oOut == nil {
				e := m.newEvent(h, "outage", h.stStart)
				m.evMu.Lock()
				e.Count, e.Last = h.consec, t
				m.evMu.Unlock()
				h.oOut = e
			} else {
				m.evMu.Lock()
				h.oOut.Count++
				h.oOut.Last = t
				late := h.oOut.Late && time.Now().UnixMilli()-t <= lateAfter
				m.evMu.Unlock()
				if late {
					go m.lateNotify(h.oOut)
				}
			}
		}
	} else {
		if h.consec > 0 {
			if h.oOut != nil {
				m.finishEvent(h.oOut, t)
				h.oOut = nil
			} else {
				if h.oLoss == nil {
					h.oLoss = m.newEvent(h, "loss", h.stStart)
				}
				m.evMu.Lock()
				h.oLoss.Count += h.consec
				h.oLoss.Last = h.stLast
				m.evMu.Unlock()
			}
			h.consec = 0
		}
		if f == 1 {
			if h.oSpk == nil {
				h.oSpk = m.newEvent(h, "spike", t)
			}
			m.evMu.Lock()
			h.oSpk.Count++
			h.oSpk.Last = t
			h.oSpk.Sum += int64(r)
			if r > h.oSpk.Max {
				h.oSpk.Max = r
			}
			m.evMu.Unlock()
		}
	}
	id := h.cfg.ID
	h.mu.Unlock()

	for _, e := range done {
		m.finishEvent(e, e.Last)
	}
	m.store.Append(id, sm)
}

// closeIdle schließt Verlust-/Spitzen-Ereignisse ohne neue Messwerte.
func (m *Monitor) closeIdle(h *Host, now int64) {
	h.mu.Lock()
	var done []*Event
	m.evMu.Lock()
	if h.oLoss != nil && now-h.oLoss.Last > mergeGap+5000 {
		done = append(done, h.oLoss)
		h.oLoss = nil
	}
	if h.oSpk != nil && now-h.oSpk.Last > mergeGap+5000 {
		done = append(done, h.oSpk)
		h.oSpk = nil
	}
	m.evMu.Unlock()
	h.mu.Unlock()
	for _, e := range done {
		m.finishEvent(e, e.Last)
	}
}

func (m *Monitor) closeAll(h *Host) {
	h.mu.Lock()
	evs := []*Event{h.oOut, h.oLoss, h.oSpk}
	h.oOut, h.oLoss, h.oSpk = nil, nil, nil
	h.consec = 0
	h.mu.Unlock()
	for _, e := range evs {
		if e != nil {
			m.finishEventQuiet(e, e.Last)
		}
	}
}

func (m *Monitor) housekeeping() {
	tick := time.NewTicker(5 * time.Second)
	lastMaint := time.Time{}
	for range tick.C {
		now := time.Now()
		nowMs := now.UnixMilli()
		m.store.Flush()
		m.resumePaused(nowMs)
		m.trialCheck(now)
		m.mu.RLock()
		hosts := make([]*Host, 0, len(m.hosts))
		for _, h := range m.hosts {
			hosts = append(hosts, h)
		}
		sites := m.cfg.Sites
		m.mu.RUnlock()
		cut := now.Add(-memWindow).UnixMilli()
		for _, h := range hosts {
			m.closeIdle(h, nowMs)
			h.mu.Lock()
			if len(h.samples) > 0 && h.samples[0].T < cut {
				i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= cut })
				h.samples = append([]Sample(nil), h.samples[i:]...)
			}
			h.mu.Unlock()
		}
		m.checkMachines(nowMs)
		if now.Minute()%5 == 0 && now.Second() < 5 {
			m.saveSysState()
		}
		if now.Second() < 5 {
			m.mapPingTick()
			m.mapSNMPTick()
			if now.Minute()%30 == 0 {
				go m.saveMapStats()
			}
			if now.Hour() == 3 && now.Minute() == 30 {
				go m.pruneMapStats()
			}
		}
		if now.Second() < 5 {
			m.checkBackups(now)
		}
		// Standorte ohne Meldung → offline
		for _, s := range sites {
			m.mu.RLock()
			st := m.sites[s.ID]
			hasHosts := false
			for _, hc := range m.cfg.Hosts {
				if hc.Site == s.ID && !hc.Paused && !isUniFiWanHost(hc) {
					hasHosts = true
				}
			}
			m.mu.RUnlock()
			if st == nil || !hasHosts {
				continue
			}
			st.mu.Lock()
			// Agent hat sich seit dem Serverstart noch gar nicht gemeldet (z. B. Büro-PC aus) → nach 2 Minuten melden
			if st.lastSeen == 0 && st.offEv == nil && nowMs-serverStart.UnixMilli() > 120_000 {
				st.lastSeen = serverStart.UnixMilli()
			}
			var startNote *Event
			if st.lastSeen > 0 && nowMs-st.lastSeen > 45_000 && st.offEv == nil {
				inM := m.inMaintSite(s.Org, s.ID, now)
				m.evMu.Lock()
				m.nextEv++
				e := &Event{ID: m.nextEv, Site: s.ID, HostName: s.Name, Org: s.Org, Type: "offline", Start: st.lastSeen, Last: st.lastSeen,
					Maint: inM}
				m.events = append(m.events, e)
				cp := *e
				m.evMu.Unlock()
				st.offEv = e
				startNote = &cp
			}
			st.mu.Unlock()
			if startNote != nil {
				m.notifyEventStart(*startNote)
			}
		}
		if now.Sub(lastMaint) > time.Hour {
			lastMaint = now
			m.mu.RLock()
			ret := m.cfg.RetentionDays
			m.mu.RUnlock()
			go func() {
				m.store.Maintenance(ret)
				m.applyOrgRetention()
				m.cleanupMaints()
				m.checkCerts()
			}()
			m.evMu.Lock()
			c := now.AddDate(0, 0, -ret).UnixMilli()
			keep := m.events[:0]
			for _, e := range m.events {
				if e.End == 0 || e.Start >= c {
					keep = append(keep, e)
				}
			}
			m.events = keep
			if len(keep) > 0 {
				m.store.RewriteEvents(keep)
			}
			m.evMu.Unlock()
		}
	}
}

func (m *Monitor) siteSeen(id, ip, ver string) {
	m.mu.Lock()
	st := m.sites[id]
	if st == nil {
		st = &siteState{}
		m.sites[id] = st
	}
	m.mu.Unlock()
	now := time.Now().UnixMilli()
	st.mu.Lock()
	st.lastSeen, st.ip = now, ip
	if ver != "" {
		st.version = ver
	}
	ev := st.offEv
	st.offEv = nil
	st.mu.Unlock()
	if ver != "" {
		m.noteAgentVersion(id, ver, now)
	}
	if ev != nil {
		m.finishEvent(ev, now)
	}
}

func (m *Monitor) Shutdown() {
	m.mu.Lock()
	for _, h := range m.hosts {
		if h.stop != nil {
			close(h.stop)
			h.stop = nil
		}
	}
	hosts := m.hosts
	m.mu.Unlock()
	for _, h := range hosts {
		m.closeAll(h)
	}
	m.saveSysState()
	m.saveMapStats()
	m.saveMapReach()
	m.store.Close()
}

// ---- Konfigurationsänderungen ----

func (m *Monitor) UpsertHost(hc HostCfg) HostCfg {
	hc.normalize()
	m.mu.Lock()
	defer m.mu.Unlock()
	if hc.ID == "" {
		hc.ID = randHex(4)
	}
	found := false
	for i := range m.cfg.Hosts {
		if m.cfg.Hosts[i].ID == hc.ID {
			m.cfg.Hosts[i] = hc
			found = true
		}
	}
	if !found {
		m.cfg.Hosts = append(m.cfg.Hosts, hc)
	}
	h := m.hosts[hc.ID]
	if h == nil {
		h = &Host{cfg: hc, lastR: -1}
		m.loadMemory(h)
		m.hosts[hc.ID] = h
	} else {
		if h.stop != nil {
			close(h.stop)
			h.stop = nil
		}
		h.mu.Lock()
		addrChanged := h.cfg.Addr != hc.Addr || h.cfg.Site != hc.Site
		h.cfg = hc
		if addrChanged {
			h.baseN, h.basePos, h.baseMed, h.ip = 0, 0, 0, ""
		}
		h.mu.Unlock()
		if hc.Paused || addrChanged {
			go m.closeAll(h)
		}
	}
	if hc.Site == "cloud" && !hc.Paused {
		m.startWorker(h)
	}
	m.saveConfigLocked()
	return hc
}

func (m *Monitor) DeleteHost(id string) {
	m.mu.Lock()
	h := m.hosts[id]
	delete(m.hosts, id)
	out := m.cfg.Hosts[:0]
	for _, hc := range m.cfg.Hosts {
		if hc.ID != id {
			out = append(out, hc)
		}
	}
	m.cfg.Hosts = out
	m.saveConfigLocked()
	m.mu.Unlock()
	if h != nil {
		if h.stop != nil {
			close(h.stop)
		}
		m.closeAll(h)
	}
}

func (m *Monitor) UpsertSite(s SiteCfg) SiteCfg {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == "" {
		s.ID = randHex(4)
		s.Token = randHex(24)
		if s.Kind != siteKindUniFi {
			s.Kind = ""
		}
		if s.SetCustomer != nil {
			s.Customer = *s.SetCustomer
		}
		if s.SetNumber != nil {
			s.Number = *s.SetNumber
		}
		s.SetCustomer, s.SetNumber = nil, nil
		if s.Sys != nil {
			sc := *s.Sys
			sc.norm()
			s.Sys = &sc
		}
		m.cfg.Sites = append(m.cfg.Sites, s)
		m.sites[s.ID] = &siteState{}
	} else {
		for i := range m.cfg.Sites {
			if m.cfg.Sites[i].ID == s.ID {
				if s.Name != "" {
					m.cfg.Sites[i].Name = s.Name
				}
				if s.Token == "new" {
					m.cfg.Sites[i].Token = randHex(24)
				}
				if s.SetAuto != nil {
					m.cfg.Sites[i].NoAutoUpdate = !*s.SetAuto
				}
				if s.CheckUpdate {
					m.cfg.Sites[i].UpdateNow = time.Now().UnixMilli()
				}
				if s.SetCustomer != nil {
					m.cfg.Sites[i].Customer = *s.SetCustomer
				}
				if s.SetNumber != nil {
					m.cfg.Sites[i].Number = *s.SetNumber
				}
				if s.Sys != nil {
					sc := *s.Sys
					sc.norm()
					m.cfg.Sites[i].Sys = &sc
				}
				s = m.cfg.Sites[i]
			}
		}
	}
	m.saveConfigLocked()
	return s
}

func (m *Monitor) DeleteSite(id string) {
	m.mu.Lock()
	out := m.cfg.Sites[:0]
	for _, s := range m.cfg.Sites {
		if s.ID != id {
			out = append(out, s)
		}
	}
	m.cfg.Sites = out
	delete(m.sites, id)
	machOut := m.cfg.Machines[:0]
	for _, mc := range m.cfg.Machines {
		if mc.Site != id {
			machOut = append(machOut, mc)
		}
	}
	m.cfg.Machines = machOut
	var paused []string
	for i := range m.cfg.Hosts {
		if m.cfg.Hosts[i].Site == id {
			m.cfg.Hosts[i].Paused = true
			paused = append(paused, m.cfg.Hosts[i].ID)
		}
	}
	m.saveConfigLocked()
	m.mu.Unlock()
	for _, hid := range paused {
		m.mu.RLock()
		h := m.hosts[hid]
		m.mu.RUnlock()
		if h != nil {
			h.mu.Lock()
			h.cfg.Paused = true
			h.mu.Unlock()
			m.closeAll(h)
		}
	}
}

func (m *Monitor) siteByID(id string) (SiteCfg, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sc := range m.cfg.Sites {
		if sc.ID == id {
			return sc, true
		}
	}
	return SiteCfg{}, false
}

func (m *Monitor) siteByToken(tok string) (SiteCfg, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if tok == "" {
		return SiteCfg{}, false
	}
	for _, s := range m.cfg.Sites {
		if s.Token == tok {
			return s, true
		}
	}
	return SiteCfg{}, false
}

// Reset löscht Ereignisse (und optional Messwerte) im Zeitraum [from, to) für einen Host oder alle ("").
// Liefert die Anzahl gelöschter Ereignisse.
func (m *Monitor) Reset(org, hostID string, from, to int64, samples, events bool) int {
	in := func(t int64) bool { return t >= from && t < to }
	var hosts []*Host
	m.mu.RLock()
	if hostID == "" {
		for _, h := range m.hosts {
			if h.cfg.Org == org {
				hosts = append(hosts, h)
			}
		}
	} else if h := m.hosts[hostID]; h != nil && h.cfg.Org == org {
		hosts = append(hosts, h)
	}
	m.mu.RUnlock()
	ids := map[string]bool{}
	var idList []string
	for _, h := range hosts {
		ids[h.cfg.ID] = true
		idList = append(idList, h.cfg.ID)
	}

	// laufende Ereignisse im Zeitraum verwerfen, Messwerte im Speicher entfernen
	for _, h := range hosts {
		h.mu.Lock()
		if events {
			m.evMu.Lock()
			if h.oOut != nil && in(h.oOut.Start) {
				h.oOut = nil
				h.consec = 0
			}
			if h.oLoss != nil && in(h.oLoss.Start) {
				h.oLoss = nil
			}
			if h.oSpk != nil && in(h.oSpk.Start) {
				h.oSpk = nil
			}
			m.evMu.Unlock()
		}
		if samples {
			kept := make([]Sample, 0, len(h.samples))
			for _, sm := range h.samples {
				if !in(sm.T) {
					kept = append(kept, sm)
				}
			}
			h.samples = kept
			h.baseN, h.basePos, h.baseMed = 0, 0, 0
			for _, sm := range h.samples {
				if sm.R >= 0 {
					h.addBase(sm.R)
				}
			}
		}
		h.mu.Unlock()
	}

	// Ereignisliste bereinigen
	var removed []int64
	if !events {
		if samples {
			m.store.DeleteSamples(idList, from, to)
		}
		log.Printf("Zurückgesetzt: %d Host(s) %s – %s, nur Messwerte", len(hosts),
			time.UnixMilli(from).Format("02.01.2006 15:04"), time.UnixMilli(to).Format("02.01.2006 15:04"))
		return 0
	}
	m.evMu.Lock()
	keep := m.events[:0]
	for _, e := range m.events {
		match := in(e.Start) && (ids[e.HostID] || (hostID == "" && e.Type == "offline" && e.Org == org))
		if match {
			removed = append(removed, e.ID)
			continue
		}
		keep = append(keep, e)
	}
	m.events = keep
	cp := append([]*Event(nil), keep...)
	m.evMu.Unlock()

	m.store.DeleteEvents(removed)
	m.store.RewriteEvents(cp)
	if samples {
		m.store.DeleteSamples(idList, from, to)
	}
	log.Printf("Zurückgesetzt: %d Host(s) %s – %s, Messwerte=%v, %d Ereignisse entfernt",
		len(hosts), time.UnixMilli(from).Format("02.01.2006 15:04"), time.UnixMilli(to).Format("02.01.2006 15:04"), samples, len(removed))
	return len(removed)
}

var serverStart = time.Now()

// noteAgentVersion merkt sich, wann der Agent eines Standorts zuletzt die Version gewechselt hat.
func (m *Monitor) noteAgentVersion(siteID, ver string, now int64) {
	m.mu.RLock()
	same := false
	for _, s := range m.cfg.Sites {
		if s.ID == siteID {
			same = s.AgentVer == ver
		}
	}
	m.mu.RUnlock()
	if same {
		return
	}
	m.mu.Lock()
	for i := range m.cfg.Sites {
		if s := &m.cfg.Sites[i]; s.ID == siteID && s.AgentVer != ver {
			if s.AgentVer != "" {
				s.AgentPrev, s.AgentUpdated = s.AgentVer, now
			}
			s.AgentVer = ver
		}
	}
	m.saveConfigLocked()
	m.mu.Unlock()
}

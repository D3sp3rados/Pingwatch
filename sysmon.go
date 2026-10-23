package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Rechner-Überwachung („RMM light“): Der Agent meldet jede Minute Auslastung, Laufwerke,
// Dienste, Ereignisprotokoll, Updates, Virenschutz und ausstehende Neustarts des Rechners,
// auf dem er läuft. Weitere Server/PCs im selben Netz bekommen einen Agenten im Modus
// „nur Rechner“ (agent.json mit "mode":"sysonly" oder Startparameter -sysonly) – sie
// erscheinen als zusätzliche Rechner des Standorts, prüfen aber keine Hosts.
// Es wird nur gemessen – PingWacht führt auf den Rechnern nichts aus.

// SysCfg: Einstellungen je Standort.
type SysCfg struct {
	On         bool     `json:"on"`
	Services   []string `json:"services,omitempty"`   // überwachte Dienste (Name oder Anzeigename)
	DiskPct    int      `json:"diskPct,omitempty"`    // Laufwerk voll ab … % belegt (Standard 90)
	DiskMinGB  float64  `json:"diskMinGB,omitempty"`  // oder weniger als … GB frei (0 = aus)
	CPUPct     int      `json:"cpuPct,omitempty"`     // CPU dauerhaft über … % (Standard 95)
	MemPct     int      `json:"memPct,omitempty"`     // RAM dauerhaft über … % (Standard 95)
	HighMin    int      `json:"highMin,omitempty"`    // „dauerhaft“ = … Minuten (Standard 15)
	EvCrit     int      `json:"evCrit,omitempty"`     // ab … kritischen Ereignissen je Stunde (Standard 1)
	AVSigDays  int      `json:"avSigDays,omitempty"`  // Virensignaturen älter als … Tage (Standard 3)
	RebootDays int      `json:"rebootDays,omitempty"` // Neustart ausstehend seit … Tagen (Standard 7)
	// Selbstheilung: gestoppte Dienste automatisch neu starten – nur wenn hier erlaubt UND der
	// Dienst ausdrücklich in Restart steht (Teilmenge von Services). Standard: aus.
	SelfHeal bool     `json:"selfHeal,omitempty"`
	Restart  []string `json:"restart,omitempty"`
	// Updates installieren und Neustarts über PingWacht erlauben (Standard: aus)
	Updates bool `json:"updates,omitempty"`
}

// validServiceName: nur harmlose Zeichen in Dienstnamen (für den automatischen Neustart).
func validServiceName(s string) bool {
	if s == "" || len(s) > 120 || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-', c == '@', c == ' ':
		default:
			return false
		}
	}
	return !strings.HasPrefix(s, "-")
}

func (c *SysCfg) norm() {
	def := func(v *int, d, lo, hi int) {
		if *v <= 0 {
			*v = d
		}
		if *v < lo {
			*v = lo
		}
		if *v > hi {
			*v = hi
		}
	}
	def(&c.DiskPct, 90, 50, 99)
	def(&c.CPUPct, 95, 50, 100)
	def(&c.MemPct, 95, 50, 100)
	def(&c.HighMin, 15, 5, 240)
	def(&c.EvCrit, 1, 1, 1000)
	def(&c.AVSigDays, 3, 1, 60)
	def(&c.RebootDays, 7, 1, 90)
	if c.DiskMinGB < 0 || math.IsNaN(c.DiskMinGB) {
		c.DiskMinGB = 0
	}
	var svc []string
	seen := map[string]bool{}
	for _, s := range c.Services {
		s = strings.TrimSpace(s)
		if s == "" || len(s) > 120 || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		svc = append(svc, s)
		if len(svc) >= 50 {
			break
		}
	}
	c.Services = svc
	// Neustart nur für überwachte Dienste mit gültigem Namen
	in := map[string]string{}
	for _, s := range svc {
		in[strings.ToLower(s)] = s
	}
	var rs []string
	done := map[string]bool{}
	for _, s := range c.Restart {
		name, ok := in[strings.ToLower(strings.TrimSpace(s))]
		if !ok || !validServiceName(name) || done[strings.ToLower(name)] {
			continue
		}
		done[strings.ToLower(name)] = true
		rs = append(rs, name)
	}
	c.Restart = rs
}

func (c *SysCfg) eff() SysCfg {
	if c == nil {
		x := SysCfg{}
		x.norm()
		return x
	}
	x := *c
	x.norm()
	return x
}

// MachineCfg: ein überwachter Rechner (dauerhaft in der Konfiguration).
type MachineCfg struct {
	ID      string `json:"id"`
	Site    string `json:"site"`
	Org     string `json:"org,omitempty"`
	Name    string `json:"name"`            // Anzeigename (Standard: Rechnername)
	Host    string `json:"host,omitempty"`  // Rechnername laut Agent
	Group   string `json:"group,omitempty"` // Gruppe/Kunde
	SysOnly bool   `json:"sysOnly,omitempty"`
	Created int64  `json:"created"`
	// eigene Dienste-Liste für diesen Rechner (nil = Liste des Standorts)
	Services *[]string `json:"services,omitempty"`
	// Meldung, wenn der Rechner sich nicht meldet: "" = automatisch (Server ja, Arbeitsplatz nein), "on", "off"
	OffAlert string `json:"offAlert,omitempty"`
	// Beschreibung (eine Zeile) und Notizen der Techniker
	Desc  string `json:"desc,omitempty"`
	Notes string `json:"notes,omitempty"`
	// OffNoted: „meldet sich nicht“ wurde gemeldet (Zeitpunkt der letzten Meldung des Rechners) –
	// gespeichert, damit ein Server-Neustart nicht erneut meldet
	OffNoted int64 `json:"offNoted,omitempty"`
}

// ---------- Messwerte (Agent → Server) ----------

type sysDisk struct {
	Mount string `json:"m"`
	Total uint64 `json:"t"` // Bytes
	Free  uint64 `json:"f"`
}

func (d sysDisk) usedPct() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Total-min(d.Free, d.Total)) * 100 / float64(d.Total)
}

type sysSvc struct {
	Name  string `json:"n"`
	State string `json:"s"` // running, stopped, missing, unknown
}

type sysAV struct {
	Name     string `json:"name,omitempty"`
	Enabled  bool   `json:"on"`
	RealTime bool   `json:"rt"`
	SigDays  int    `json:"sig"` // Alter der Signaturen in Tagen (-1 = unbekannt)
}

type sysReport struct {
	Host     string    `json:"host"`
	OS       string    `json:"os,omitempty"`
	Uptime   int64     `json:"up,omitempty"`   // s
	CPU      float64   `json:"cpu"`            // %, -1 = unbekannt
	MemTotal uint64    `json:"memT,omitempty"` // Bytes
	MemUsed  uint64    `json:"memU,omitempty"`
	Disks    []sysDisk `json:"disks,omitempty"`
	Svcs     []sysSvc  `json:"svcs,omitempty"`
	EvCrit   int       `json:"evC"`               // kritische Ereignisse letzte 60 min, -1 = unbekannt
	EvErr    int       `json:"evE"`               // Fehler letzte 60 min, -1 = unbekannt
	Updates  int       `json:"upd"`               // ausstehende Updates, -1 = unbekannt
	UpdDrv   int       `json:"updDrv,omitempty"`  // davon Treiber (Windows)
	UpdList  []string  `json:"updList,omitempty"` // Titel der ausstehenden Updates (Windows)
	UpdAt    int64     `json:"updAt"`             // Zeitpunkt der Update-Prüfung (ms)
	AV       *sysAV    `json:"av,omitempty"`
	Reboot   int       `json:"reboot"` // Neustart ausstehend: 1 ja, 0 nein, -1 unbekannt
	Ver      string    `json:"ver,omitempty"`
	User     string    `json:"user,omitempty"`   // angemeldete Benutzer (Komma-getrennt)
	IPs      []string  `json:"ips,omitempty"`    // eigene Adressen (IPv4 zuerst)
	Heal     []sysHeal `json:"heal,omitempty"`   // Selbstheilung: Neustart-Versuche seit der letzten Meldung
	NoHeal   bool      `json:"noHeal,omitempty"` // Selbstheilung im Agenten abgeschaltet (agent.json)
}

// sysHeal: ein automatischer Neustart-Versuch des Agenten.
type sysHeal struct {
	Svc    string `json:"svc"`
	T      int64  `json:"t"`
	Try    int    `json:"try"`
	OK     bool   `json:"ok"`
	Err    string `json:"err,omitempty"`
	GaveUp bool   `json:"gaveUp,omitempty"` // letzter Versuch fehlgeschlagen – Agent gibt auf
}

func (r *sysReport) memPct() float64 {
	if r == nil || r.MemTotal == 0 {
		return -1
	}
	return float64(min(r.MemUsed, r.MemTotal)) * 100 / float64(r.MemTotal)
}

// fullestDisk: am stärksten belegtes Laufwerk.
func (r *sysReport) fullestDisk() (sysDisk, float64) {
	var best sysDisk
	pct := -1.0
	if r == nil {
		return best, pct
	}
	for _, d := range r.Disks {
		if p := d.usedPct(); d.Total > 0 && p > pct {
			best, pct = d, p
		}
	}
	return best, pct
}

type sysSample struct {
	T int64      `json:"t"`
	R *sysReport `json:"r"`
}

// ---------- Server: Zustand im Speicher ----------

type sysPoint struct {
	T int64 `json:"t"`
	C int8  `json:"c"` // CPU % (-1 unbekannt)
	M int8  `json:"m"` // RAM %
	D int8  `json:"d"` // vollstes Laufwerk %
	n int32
}

type sysCond struct {
	since int64  // Bedingung erfüllt seit
	ev    *Event // offenes Ereignis (Meldung verschickt)
}

type sysMachine struct {
	mu        sync.Mutex
	at        int64 // Zeitpunkt der letzten Messung
	seen      int64 // letzter Kontakt (Serverzeit)
	cur       *sysReport
	hist      []sysPoint // 24 h, 1 Minute
	hist7     []sysPoint // 7 Tage, 15 Minuten
	conds     map[string]*sysCond
	offEv     *Event
	rebootAt  int64 // Neustart ausstehend seit
	rebootMsg bool
	lastEvAt  int64 // letztes Ereignis „kritische Einträge“
}

var sysData = struct {
	sync.Mutex
	m map[string]*sysMachine
}{m: map[string]*sysMachine{}}

func getSysMachine(id string) *sysMachine {
	sysData.Lock()
	defer sysData.Unlock()
	sm := sysData.m[id]
	if sm == nil {
		sm = &sysMachine{conds: map[string]*sysCond{}}
		sysData.m[id] = sm
	}
	return sm
}

func dropSysMachine(id string) {
	sysData.Lock()
	delete(sysData.m, id)
	sysData.Unlock()
}

func pct8(v float64) int8 {
	if v < 0 {
		return -1
	}
	if v > 100 {
		v = 100
	}
	return int8(math.Round(v))
}

// machineLimit: Rechner je Standort laut Paket (-1 = unbegrenzt, 0 = nicht enthalten).
func machineLimit(plan string) int {
	return planByID(plan).MachinesPerSite
}

// machineByID liefert eine Kopie der Rechner-Konfiguration.
func (m *Monitor) machineByID(id string) (MachineCfg, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mc := range m.cfg.Machines {
		if mc.ID == id {
			return mc, true
		}
	}
	return MachineCfg{}, false
}

func machineKey(site, id string) string { return site + ":" + id }

// registerMachine meldet einen Rechner an (oder aktualisiert Name/Modus).
func (m *Monitor) registerMachine(site SiteCfg, machID, hostName string, sysOnly bool) (MachineCfg, error) {
	key := machineKey(site.ID, machID)
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i := range m.cfg.Machines {
		mc := &m.cfg.Machines[i]
		if mc.Site != site.ID {
			continue
		}
		if mc.ID == key {
			changed := false
			if hostName != "" && mc.Host != hostName {
				if mc.Name == "" || mc.Name == mc.Host {
					mc.Name = hostName
				}
				mc.Host = hostName
				changed = true
			}
			if mc.SysOnly != sysOnly {
				mc.SysOnly = sysOnly
				changed = true
			}
			if changed {
				m.saveConfigLocked()
			}
			return *mc, nil
		}
		n++
	}
	plan := "internal" // Standorte ohne Kunde (ältere Installationen) wie intern
	if o := m.orgByIDLocked(site.Org); o != nil {
		plan = o.Plan
	}
	lim := machineLimit(plan)
	if lim == 0 {
		return MachineCfg{}, fmt.Errorf("Die Rechner-Überwachung ist im Paket „%s“ nicht enthalten", planByID(plan).Name)
	}
	if lim > 0 && n >= lim {
		return MachineCfg{}, fmt.Errorf("Das Paket „%s“ umfasst %d Rechner je Standort – bitte einen Rechner entfernen oder das Paket wechseln", planByID(plan).Name, lim)
	}
	name := hostName
	if name == "" {
		name = "Rechner " + machID[:min(6, len(machID))]
	}
	mc := MachineCfg{ID: key, Site: site.ID, Org: site.Org, Name: name, Host: hostName, SysOnly: sysOnly, Created: time.Now().UnixMilli()}
	m.cfg.Machines = append(m.cfg.Machines, mc)
	m.saveConfigLocked()
	log.Printf("Rechner angemeldet: %s (Standort %s)", name, site.Name)
	return mc, nil
}

func (m *Monitor) orgByIDLocked(id string) *Org {
	for i := range m.cfg.Orgs {
		if m.cfg.Orgs[i].ID == id {
			return &m.cfg.Orgs[i]
		}
	}
	return nil
}

// machineSeen: Lebenszeichen eines Rechners (auch „nur Rechner“-Agenten beim Konfig-Abruf).
func (m *Monitor) machineSeen(mc MachineCfg) {
	sm := getSysMachine(mc.ID)
	now := time.Now().UnixMilli()
	sm.mu.Lock()
	sm.seen = now
	ev := sm.offEv
	sm.offEv = nil
	sm.mu.Unlock()
	if ev != nil {
		ce := m.closeSiteEvent(ev, now)
		if ce.Notified && !ce.Maint {
			m.notify(notice{"syson", "m:" + mc.ID, "🟢 Rechner wieder erreichbar: " + mc.Name,
				m.sysInfo(mc, fmt.Sprintf("Hat sich %s nicht gemeldet (%s – %s)", fmtDurShort(ce.End-ce.Start),
					time.UnixMilli(ce.Start).Format("15:04"), time.UnixMilli(ce.End).Format("15:04"))), mc.Org, ""})
		}
	} else if mc.OffNoted > 0 && !m.machineInMaint(mc, time.Now()) {
		// Meldung stammt von vor einem Server-Neustart
		m.notify(notice{"syson", "m:" + mc.ID, "🟢 Rechner wieder erreichbar: " + mc.Name,
			m.sysInfo(mc, fmt.Sprintf("Hat sich seit %s nicht gemeldet", time.UnixMilli(mc.OffNoted).Format("02.01. 15:04"))), mc.Org, ""})
	}
	if mc.OffNoted > 0 {
		m.setOffNoted(mc.ID, 0)
	}
}

func (m *Monitor) setOffNoted(id string, v int64) {
	m.mu.Lock()
	for i := range m.cfg.Machines {
		if m.cfg.Machines[i].ID == id {
			m.cfg.Machines[i].OffNoted = v
		}
	}
	m.saveConfigLocked()
	m.mu.Unlock()
}

// sysInfo: „Kunde · Standort“ für die Meldungstexte der Rechner.
func (m *Monitor) sysInfo(mc MachineCfg, text string) string {
	head := mc.Host
	if mc.Host == mc.Name {
		head = ""
	}
	m.mu.RLock()
	for _, g := range m.cfg.Groups {
		if g.ID == mc.Group && mc.Group != "" {
			if head != "" {
				head += " · "
			}
			head += "Kunde " + g.Name
		}
	}
	m.mu.RUnlock()
	out := text
	if head != "" {
		out = head + "\n" + out
	}
	if n := m.siteNameOf(mc.Site); n != "" {
		out += "\nStandort: " + n
	}
	return out
}

func (m *Monitor) newMachineEvent(mc MachineCfg, name string, at int64, maint bool) *Event {
	m.evMu.Lock()
	m.nextEv++
	e := &Event{ID: m.nextEv, HostID: "m:" + mc.ID, Site: mc.Site, HostName: name, Org: mc.Org, Type: "sys", Start: at, Last: at, Maint: maint, Decided: true, Notified: !maint}
	m.events = append(m.events, e)
	m.evMu.Unlock()
	return e
}

func (m *Monitor) machineInMaint(mc MachineCfg, t time.Time) bool {
	return m.inMaintSite(mc.Org, mc.Site, t) || m.inMaint(mc.Org, "m:"+mc.ID, t)
}

func gb(b uint64) string {
	g := float64(b) / (1 << 30)
	if g >= 100 {
		return fmt.Sprintf("%.0f GB", g)
	}
	return fmt.Sprintf("%.1f GB", g)
}

// sysCheck: eine bewertete Bedingung eines Messzeitpunkts.
type sysCheck struct {
	key   string
	bad   bool   // Bedingung erfüllt
	clear bool   // deutlich wieder gut (Hysterese); bei !bad && !clear bleibt der Zustand
	hold  int64  // muss so lange (ms) anhalten, bevor gemeldet wird
	title string // Titel der Meldung
	body  string // Text der Meldung
	ok    string // Titel der Entwarnung
	push  bool   // Push/Alarmwege (sonst nur Ereignis)
	label string // Kurzname für Ereignis und Übersicht
}

// sysChecks bewertet einen Messwert anhand der Schwellwerte.
func sysChecks(r *sysReport, c SysCfg, name string) []sysCheck {
	var out []sysCheck
	for _, d := range r.Disks {
		if d.Total == 0 {
			continue
		}
		p := d.usedPct()
		lowFree := c.DiskMinGB > 0 && float64(d.Free) < c.DiskMinGB*(1<<30)
		bad := p >= float64(c.DiskPct) || lowFree
		clear := p < float64(c.DiskPct)-2 && (c.DiskMinGB == 0 || float64(d.Free) > c.DiskMinGB*1.1*(1<<30))
		out = append(out, sysCheck{key: "disk:" + strings.ToLower(d.Mount), bad: bad, clear: clear,
			title: "🟠 Laufwerk fast voll: " + name + " " + d.Mount,
			body:  fmt.Sprintf("%s ist zu %.0f %% belegt, noch %s frei (von %s).", d.Mount, p, gb(d.Free), gb(d.Total)),
			ok:    "🟢 Laufwerk wieder in Ordnung: " + name + " " + d.Mount, push: true, label: "Laufwerk " + d.Mount + " fast voll"})
	}
	if r.CPU >= 0 {
		out = append(out, sysCheck{key: "cpu", bad: r.CPU >= float64(c.CPUPct), clear: r.CPU < float64(c.CPUPct)-10,
			hold: int64(c.HighMin) * 60000, title: "🟠 Hohe CPU-Last: " + name,
			body: fmt.Sprintf("CPU seit über %d min über %d %% (zuletzt %.0f %%).", c.HighMin, c.CPUPct, r.CPU),
			ok:   "🟢 CPU-Last wieder normal: " + name, push: true, label: "Hohe CPU-Last"})
	}
	if mp := r.memPct(); mp >= 0 {
		out = append(out, sysCheck{key: "mem", bad: mp >= float64(c.MemPct), clear: mp < float64(c.MemPct)-5,
			hold: int64(c.HighMin) * 60000, title: "🟠 Arbeitsspeicher voll: " + name,
			body: fmt.Sprintf("RAM seit über %d min über %d %% belegt (%s von %s).", c.HighMin, c.MemPct, gb(r.MemUsed), gb(r.MemTotal)),
			ok:   "🟢 Arbeitsspeicher wieder normal: " + name, push: true, label: "Arbeitsspeicher voll"})
	}
	heal := map[string]bool{}
	if c.SelfHeal && !r.NoHeal {
		for _, s := range c.Restart {
			heal[strings.ToLower(s)] = true
		}
	}
	for _, s := range r.Svcs {
		hold := int64(2 * 60000)
		if heal[strings.ToLower(s.Name)] {
			hold = 5 * 60000 // erst den automatischen Neustart abwarten
		}
		switch s.State {
		case "running":
			out = append(out, sysCheck{key: "svc:" + strings.ToLower(s.Name), clear: true})
		case "stopped", "missing":
			why := "ist gestoppt"
			if s.State == "missing" {
				why = "ist auf dem Rechner nicht vorhanden"
			}
			out = append(out, sysCheck{key: "svc:" + strings.ToLower(s.Name), bad: true, hold: hold,
				title: "🔴 Dienst gestoppt: " + s.Name + " (" + name + ")",
				body:  "Der Dienst „" + s.Name + "“ " + why + ".",
				ok:    "🟢 Dienst läuft wieder: " + s.Name + " (" + name + ")", push: true, label: "Dienst " + s.Name + " gestoppt"})
		}
	}
	if av := r.AV; av != nil {
		off := !av.Enabled || !av.RealTime
		what := "Virenschutz ist ausgeschaltet"
		if av.Enabled && !av.RealTime {
			what = "Echtzeitschutz ist ausgeschaltet"
		}
		if av.Name != "" {
			what += " (" + av.Name + ")"
		}
		out = append(out, sysCheck{key: "av", bad: off, clear: !off, hold: 5 * 60000,
			title: "🔴 Virenschutz aus: " + name, body: what + ".",
			ok: "🟢 Virenschutz wieder aktiv: " + name, push: true, label: "Virenschutz aus"})
		if av.SigDays >= 0 {
			old := av.SigDays > c.AVSigDays
			out = append(out, sysCheck{key: "avsig", bad: old, clear: !old,
				title: "🟠 Virensignaturen veraltet: " + name,
				body:  fmt.Sprintf("Die Virensignaturen sind %d Tage alt.", av.SigDays),
				ok:    "🟢 Virensignaturen aktuell: " + name, push: true, label: "Virensignaturen veraltet"})
		}
	}
	return out
}

// ingestSys verarbeitet die Messwerte eines Rechners (zeitlich sortiert, auch nachgereichte).
func (m *Monitor) ingestSys(mc MachineCfg, c SysCfg, samples []sysSample) {
	now := time.Now().UnixMilli()
	sort.Slice(samples, func(i, j int) bool { return samples[i].T < samples[j].T })
	sm := getSysMachine(mc.ID)
	var notes []notice
	name := mc.Name
	sm.mu.Lock()
	for _, s := range samples {
		if s.R == nil || s.T <= sm.at || s.T > now+60000 || s.T < now-24*3600*1000 {
			continue
		}
		r := s.R
		r.User = clipStr(r.User, 200)
		if len(r.IPs) > 12 {
			r.IPs = r.IPs[:12]
		}
		sm.at = s.T
		sm.cur = r
		_, dp := r.fullestDisk()
		p := sysPoint{T: s.T, C: pct8(r.CPU), M: pct8(r.memPct()), D: pct8(dp)}
		sm.hist = append(sm.hist, p)
		sm.addHist7(p)
		recent := s.T >= now-10*60000
		if !recent {
			continue // nachgereichte Messungen nur für den Verlauf
		}
		maint := m.machineInMaint(mc, time.UnixMilli(s.T))
		seen := map[string]bool{}
		for _, ck := range sysChecks(r, c, name) {
			seen[ck.key] = true
			cd := sm.conds[ck.key]
			if cd == nil {
				cd = &sysCond{}
				sm.conds[ck.key] = cd
			}
			switch {
			case ck.bad:
				if cd.since == 0 {
					cd.since = s.T
				}
				if cd.ev == nil && s.T-cd.since >= ck.hold {
					cd.ev = m.newMachineEvent(mc, ck.label+" · "+name, cd.since, maint)
					if !maint && ck.push {
						notes = append(notes, notice{"syswarn", "m:" + mc.ID, ck.title, m.sysInfo(mc, ck.body), mc.Org, ""})
					} else {
						m.evMu.Lock()
						cd.ev.Notified = false
						m.evMu.Unlock()
					}
				}
			case ck.clear:
				cd.since = 0
				if cd.ev != nil {
					ce := m.closeSiteEvent(cd.ev, s.T)
					cd.ev = nil
					if ce.Notified && ck.ok != "" {
						notes = append(notes, notice{"sysok", "m:" + mc.ID, ck.ok,
							m.sysInfo(mc, "War "+fmtDurShort(ce.End-ce.Start)+" auffällig."), mc.Org, ""})
					}
				}
			}
		}
		// Bedingungen, die es nicht mehr gibt (z. B. Dienst aus der Liste entfernt) → schließen
		for k, cd := range sm.conds {
			if !seen[k] {
				if cd.ev != nil {
					m.closeSiteEvent(cd.ev, s.T)
				}
				delete(sm.conds, k)
			}
		}
		// Selbstheilung: Neustart-Versuche des Agenten
		for _, h := range r.Heal {
			svc := h.Svc
			switch {
			case h.OK:
				m.closeSiteEvent(m.newMachineEvent(mc, fmt.Sprintf("Dienst %s gestoppt – automatisch neu gestartet (Versuch %d)", svc, h.Try)+" · "+name, h.T, maint), h.T)
			case h.GaveUp:
				e := m.newMachineEvent(mc, fmt.Sprintf("Dienst %s: automatischer Neustart fehlgeschlagen", svc)+" · "+name, h.T, maint)
				m.closeSiteEvent(e, h.T)
				if !maint {
					why := ""
					if h.Err != "" {
						why = " Fehler: " + h.Err
					}
					notes = append(notes, notice{"syswarn", "m:" + mc.ID, "🔴 Neustart fehlgeschlagen: " + svc + " (" + name + ")",
						m.sysInfo(mc, fmt.Sprintf("Der Dienst „%s“ ist gestoppt, %d automatische Neustarts sind fehlgeschlagen.%s Bitte selbst prüfen.", svc, h.Try, why)), mc.Org, ""})
				}
			default:
				e := m.newMachineEvent(mc, fmt.Sprintf("Dienst %s: Neustart-Versuch %d fehlgeschlagen", svc, h.Try)+" · "+name, h.T, maint)
				m.evMu.Lock()
				e.Notified = false
				m.evMu.Unlock()
				m.closeSiteEvent(e, h.T)
			}
		}
		// Neustart ausstehend
		switch r.Reboot {
		case 1:
			if sm.rebootAt == 0 {
				sm.rebootAt = s.T
			}
			if !sm.rebootMsg && s.T-sm.rebootAt >= int64(c.RebootDays)*24*3600*1000 {
				sm.rebootMsg = true
				m.closeSiteEvent(m.newMachineEvent(mc, "Neustart ausstehend · "+name, sm.rebootAt, maint), s.T)
				if !maint {
					notes = append(notes, notice{"syswarn", "m:" + mc.ID, "🟠 Neustart ausstehend: " + name,
						m.sysInfo(mc, fmt.Sprintf("Der Rechner wartet seit über %d Tagen auf einen Neustart (z. B. nach Updates).", c.RebootDays)), mc.Org, ""})
				}
			}
		case 0:
			sm.rebootAt, sm.rebootMsg = 0, false
		}
		// Kritische Einträge im Ereignisprotokoll – nur als Ereignis, höchstens stündlich
		if r.EvCrit >= c.EvCrit && s.T-sm.lastEvAt >= 3600*1000 {
			sm.lastEvAt = s.T
			e := m.newMachineEvent(mc, fmt.Sprintf("%d kritische Ereignisse (Ereignisprotokoll) · %s", r.EvCrit, name), s.T, maint)
			m.evMu.Lock()
			e.Notified = false
			m.evMu.Unlock()
			m.closeSiteEvent(e, s.T)
		}
	}
	cut := now - 24*3600*1000
	if i := sort.Search(len(sm.hist), func(i int) bool { return sm.hist[i].T >= cut }); i > 0 {
		sm.hist = append([]sysPoint(nil), sm.hist[i:]...)
	}
	cut7 := now - 7*24*3600*1000
	if i := sort.Search(len(sm.hist7), func(i int) bool { return sm.hist7[i].T >= cut7 }); i > 0 {
		sm.hist7 = append([]sysPoint(nil), sm.hist7[i:]...)
	}
	sm.seen = now
	sm.mu.Unlock()
	for _, n := range notes {
		m.notify(n)
	}
}

// addHist7: 15-Minuten-Mittelwerte (CPU/RAM) bzw. Höchstwert (Laufwerk) für 7 Tage.
func (sm *sysMachine) addHist7(p sysPoint) {
	const b = 15 * 60000
	bucket := p.T / b * b
	if n := len(sm.hist7); n > 0 && sm.hist7[n-1].T == bucket {
		q := &sm.hist7[n-1]
		avg := func(old, v int8) int8 {
			if v < 0 {
				return old
			}
			if old < 0 {
				return v
			}
			return int8((int32(old)*q.n + int32(v) + q.n/2) / (q.n + 1))
		}
		q.C, q.M = avg(q.C, p.C), avg(q.M, p.M)
		if p.D > q.D {
			q.D = p.D
		}
		q.n++
		return
	}
	p.T, p.n = bucket, 1
	sm.hist7 = append(sm.hist7, p)
}

// checkMachines: „nur Rechner“-Agenten, die sich nicht mehr melden (der Standort-Agent selbst
// wird schon über „Agent offline“ gemeldet).
func (m *Monitor) checkMachines(nowMs int64) {
	m.mu.RLock()
	var list []MachineCfg
	on := map[string]bool{}
	for _, s := range m.cfg.Sites {
		on[s.ID] = s.Sys != nil && s.Sys.On
	}
	for _, mc := range m.cfg.Machines {
		if mc.SysOnly && on[mc.Site] {
			list = append(list, mc)
		}
	}
	m.mu.RUnlock()
	for _, mc := range list {
		sm := getSysMachine(mc.ID)
		sm.mu.Lock()
		if sm.seen == 0 && nowMs-serverStart.UnixMilli() > 5*60000 {
			sm.seen = serverStart.UnixMilli()
		}
		if mc.OffNoted > 0 && (sm.seen == 0 || sm.seen == serverStart.UnixMilli()) {
			sm.seen = mc.OffNoted // vor dem Neustart schon gemeldet: Zeitpunkt übernehmen
		}
		var start *Event
		alert := mc.wantsOffAlert(sm.cur)
		// höchstens eine Meldung je Ausfall – auch über Server-Neustarts hinweg (OffNoted)
		if sm.seen > 0 && nowMs-sm.seen > 5*60000 && sm.offEv == nil && alert && mc.OffNoted == 0 {
			maint := m.machineInMaint(mc, time.UnixMilli(nowMs))
			sm.offEv = m.newMachineEvent(mc, "Rechner meldet sich nicht · "+mc.Name, sm.seen, maint)
			start = sm.offEv
		}
		seen := sm.seen
		sm.mu.Unlock()
		if start != nil {
			m.setOffNoted(mc.ID, seen)
		}
		if start != nil && !start.Maint {
			m.notify(notice{"sysoff", "m:" + mc.ID, "⚪️ Rechner meldet sich nicht: " + mc.Name,
				m.sysInfo(mc, "Seit "+time.UnixMilli(seen).Format("15:04")+" keine Meldung (ausgeschaltet, kein Netz oder Agent beendet?)"), mc.Org, ""})
		}
	}
}

// ---------- Ansichten ----------

type machineView struct {
	MachineCfg
	At       int64      `json:"at,omitempty"`
	Online   bool       `json:"online"`
	Status   string     `json:"status"` // ok, warn, bad, off, unknown, disabled
	Problems []string   `json:"problems,omitempty"`
	Cur      *sysReport `json:"cur,omitempty"`
	CPU      int8       `json:"cpu"`
	Mem      int8       `json:"mem"`
	Disk     int8       `json:"disk"`
	DiskName string     `json:"diskName,omitempty"`
	SvcOK    int        `json:"svcOK"`
	SvcN     int        `json:"svcN"`
	Reboot   int64      `json:"rebootSince,omitempty"`
}

func (m *Monitor) machineView(mc MachineCfg, siteOn bool) machineView {
	v := machineView{MachineCfg: mc, CPU: -1, Mem: -1, Disk: -1, Status: "unknown"}
	if !siteOn {
		v.Status = "disabled"
		return v
	}
	sm := getSysMachine(mc.ID)
	now := time.Now().UnixMilli()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	v.At = sm.at
	v.Reboot = sm.rebootAt
	if sm.cur == nil {
		return v
	}
	r := sm.cur
	v.Cur = r
	v.Online = now-sm.at < 3*60000
	v.CPU, v.Mem = pct8(r.CPU), pct8(r.memPct())
	d, dp := r.fullestDisk()
	v.Disk, v.DiskName = pct8(dp), d.Mount
	for _, s := range r.Svcs {
		v.SvcN++
		if s.State == "running" {
			v.SvcOK++
		}
	}
	bad, warn := false, false
	keys := make([]string, 0, len(sm.conds))
	for k := range sm.conds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cd := sm.conds[k]
		if cd.ev == nil {
			continue
		}
		label := cd.ev.HostName
		if i := strings.LastIndex(label, " · "); i > 0 {
			label = label[:i]
		}
		v.Problems = append(v.Problems, label)
		if strings.HasPrefix(k, "svc:") || k == "av" {
			bad = true
		} else {
			warn = true
		}
	}
	switch {
	case !v.Online && !mc.wantsOffAlert(r):
		v.Status = "sleep" // Arbeitsplatz ausgeschaltet – kein Problem
	case sm.offEv != nil || !v.Online:
		v.Status = "off"
	case bad:
		v.Status = "bad"
	case warn:
		v.Status = "warn"
	default:
		v.Status = "ok"
	}
	return v
}

// machinesOf: alle Rechner eines Kunden (für die Übersicht).
func (m *Monitor) machinesOf(org string) []machineView {
	m.mu.RLock()
	on := map[string]bool{}
	for _, s := range m.cfg.Sites {
		on[s.ID] = s.Sys != nil && s.Sys.On
	}
	var list []MachineCfg
	for _, mc := range m.cfg.Machines {
		if mc.Org == org {
			list = append(list, mc)
		}
	}
	m.mu.RUnlock()
	out := []machineView{}
	for _, mc := range list {
		out = append(out, m.machineView(mc, on[mc.Site]))
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

type machineDetail struct {
	machineView
	Hist  []sysPoint `json:"hist"`
	Hist7 []sysPoint `json:"hist7"`
	Cfg   SysCfg     `json:"cfg"`
}

// ---------- API ----------

// POST /api/agent/sys {machine, host, sysOnly, samples}
func (s *Server) handleAgentSys(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Machine string      `json:"machine"`
		Host    string      `json:"host"`
		SysOnly bool        `json:"sysOnly"`
		Samples []sysSample `json:"samples"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	req.Machine = strings.TrimSpace(req.Machine)
	if req.Machine == "" || len(req.Machine) > 64 {
		http.Error(w, "Rechner-ID fehlt", 400)
		return
	}
	if site.Sys == nil || !site.Sys.On {
		writeJSON(w, map[string]any{"ok": true, "off": true})
		return
	}
	host := strings.TrimSpace(req.Host)
	if len(host) > 100 {
		host = host[:100]
	}
	mc, err := s.m.registerMachine(site, req.Machine, host, req.SysOnly)
	if err != nil {
		http.Error(w, err.Error(), http.StatusPaymentRequired)
		return
	}
	s.m.machineSeen(mc)
	s.m.ingestSys(mc, site.Sys.eff(), req.Samples)
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/machine?id= → Detail mit Verlauf
// POST /api/machine {id, name, group} → umbenennen/zuordnen
// DELETE /api/machine?id= → entfernen
func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	id := r.URL.Query().Get("id")
	var body struct {
		ID    string  `json:"id"`
		Name  *string `json:"name"`
		Group *string `json:"group"`
		// Dienste: "site" = Liste des Standorts, "own" = eigene Liste (Services)
		SvcMode  string   `json:"svcMode"`
		OffAlert *string  `json:"offAlert"` // "", "on", "off"
		Desc     *string  `json:"desc"`
		Notes    *string  `json:"notes"`
		Services []string `json:"services"`
	}
	if r.Method == http.MethodPost {
		if !readJSON(w, r, &body) {
			return
		}
		id = body.ID
	}
	mc, ok := m.machineByID(id)
	if !ok || mc.Org != p.Org {
		http.Error(w, "unbekannter Rechner", 404)
		return
	}
	switch r.Method {
	case http.MethodGet:
		site := s.siteFor(r, mc.Site)
		on := site != nil && site.Sys != nil && site.Sys.On
		d := machineDetail{machineView: m.machineView(mc, on)}
		if site != nil {
			d.Cfg = site.Sys.eff()
		}
		sm := getSysMachine(mc.ID)
		sm.mu.Lock()
		step := len(sm.hist)/288 + 1
		for i := 0; i < len(sm.hist); i += step {
			d.Hist = append(d.Hist, sm.hist[i])
		}
		d.Hist7 = append([]sysPoint(nil), sm.hist7...)
		sm.mu.Unlock()
		writeJSON(w, d)
	case http.MethodPost:
		m.mu.Lock()
		for i := range m.cfg.Machines {
			x := &m.cfg.Machines[i]
			if x.ID != mc.ID {
				continue
			}
			if body.Name != nil {
				if n := strings.TrimSpace(*body.Name); n != "" && len(n) <= 80 {
					x.Name = n
				}
			}
			if body.Desc != nil {
				x.Desc = clipStr(strings.TrimSpace(strings.ReplaceAll(*body.Desc, "\n", " ")), 200)
			}
			if body.Notes != nil {
				x.Notes = clipStr(strings.TrimRight(*body.Notes, " \n"), 8000)
			}
			if body.OffAlert != nil && (*body.OffAlert == "" || *body.OffAlert == "on" || *body.OffAlert == "off") {
				x.OffAlert = *body.OffAlert
			}
			switch body.SvcMode {
			case "site":
				x.Services = nil
			case "own":
				l := []string{}
				for _, sv := range body.Services {
					if sv = strings.TrimSpace(sv); sv != "" && len(sv) <= 120 && len(l) < 50 {
						l = append(l, sv)
					}
				}
				x.Services = &l
			}
			if body.Group != nil {
				g := strings.TrimSpace(*body.Group)
				okG := g == ""
				for _, gr := range m.cfg.Groups {
					if gr.ID == g && gr.Org == p.Org {
						okG = true
					}
				}
				if okG {
					x.Group = g
				}
			}
			mc = *x
		}
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, mc)
	case http.MethodDelete:
		m.removeMachines(func(x MachineCfg) bool { return x.ID == mc.ID })
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

// removeMachines entfernt Rechner (z. B. beim Löschen eines Standorts oder Kunden).
func (m *Monitor) removeMachines(match func(MachineCfg) bool) {
	m.mu.Lock()
	out := m.cfg.Machines[:0]
	var gone []string
	for _, x := range m.cfg.Machines {
		if match(x) {
			gone = append(gone, x.ID)
			continue
		}
		out = append(out, x)
	}
	m.cfg.Machines = out
	if len(gone) > 0 {
		m.saveConfigLocked()
	}
	m.mu.Unlock()
	for _, id := range gone {
		m.dropInv(id)
		sm := getSysMachine(id)
		sm.mu.Lock()
		evs := []*Event{sm.offEv}
		for _, cd := range sm.conds {
			evs = append(evs, cd.ev)
		}
		sm.mu.Unlock()
		now := time.Now().UnixMilli()
		for _, e := range evs {
			if e != nil {
				m.closeSiteEvent(e, now)
			}
		}
		dropSysMachine(id)
	}
}

// machineGroupLocked: Gruppe eines Rechners (für Kunden-Stummschaltung und Alarmwege).
func (m *Monitor) machineGroupLocked(id string) string {
	id = strings.TrimPrefix(id, "m:")
	for _, mc := range m.cfg.Machines {
		if mc.ID == id {
			return mc.Group
		}
	}
	return ""
}

// machineOnline: Rechner hat sich in den letzten 3 Minuten gemeldet.
func (m *Monitor) machineOnline(mc MachineCfg) bool {
	sm := getSysMachine(mc.ID)
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.seen > 0 && time.Now().UnixMilli()-sm.seen < 3*60000
}

// isServerOS: Windows Server oder Linux (meist Server) – Mac und Windows-Clients nicht.
func isServerOS(os string) bool {
	l := strings.ToLower(os)
	return strings.Contains(l, "server") || (l != "" && !strings.Contains(l, "windows") && !strings.Contains(l, "mac"))
}

// wantsOffAlert: Meldung, wenn sich der Rechner nicht meldet?
func (mc MachineCfg) wantsOffAlert(cur *sysReport) bool {
	switch mc.OffAlert {
	case "on":
		return true
	case "off":
		return false
	}
	return cur == nil || isServerOS(cur.OS)
}

// clipStr kürzt auf höchstens n Bytes, ohne ein UTF-8-Zeichen zu zerschneiden.
func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

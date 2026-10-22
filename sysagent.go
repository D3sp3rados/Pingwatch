package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Agent-Seite der Rechner-Überwachung: misst jede Minute den eigenen Rechner und meldet
// an /api/agent/sys. Langsame Abfragen (Updates, Virenschutz, Ereignisprotokoll, Neustart)
// laufen im Hintergrund und werden zwischengespeichert. Nur lesende Abfragen.

const sysPollEvery = 60 * time.Second

// runCmd führt ein Programm mit Zeitlimit und ohne Fenster aus und liefert stdout.
func runCmd(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// psEncoded: PowerShell-Befehl als -EncodedCommand (UTF-16LE, Base64) – vermeidet Quoting-Probleme.
func psEncoded(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func runPS(timeout time.Duration, script string) ([]byte, error) {
	return runCmd(timeout, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", psEncoded(script))
}

// sysMachineID: stabile Kennung des Rechners (aus der Maschinen-ID des Systems, sonst zufällig
// und im Datenverzeichnis gespeichert).
func sysMachineID(dataDir string) string {
	if id := strings.TrimSpace(osMachineID()); id != "" {
		h := sha256.Sum256([]byte("pingwacht:" + strings.ToLower(id)))
		return hex.EncodeToString(h[:8])
	}
	p := filepath.Join(dataDir, "machine-id")
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 8 {
			return s
		}
	}
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	os.WriteFile(p, []byte(id), 0o600)
	return id
}

type sysSlow struct {
	evCrit, evErr int
	updates       int
	updDrv        int
	updAt         int64
	av            *sysAV
	reboot        int
}

type sysCollector struct {
	mu   sync.Mutex
	slow sysSlow
	next map[string]time.Time
	busy map[string]bool
	cpu  cpuState
}

func newSysCollector() *sysCollector {
	return &sysCollector{slow: sysSlow{evCrit: -1, evErr: -1, updates: -1, reboot: -1},
		next: map[string]time.Time{}, busy: map[string]bool{}}
}

// due startet eine langsame Abfrage im Hintergrund, wenn sie fällig ist.
func (c *sysCollector) due(key string, every time.Duration, fn func()) {
	c.mu.Lock()
	if c.busy[key] || time.Now().Before(c.next[key]) {
		c.mu.Unlock()
		return
	}
	c.busy[key] = true
	c.next[key] = time.Now().Add(every)
	c.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Rechner-Überwachung (%s): %v", key, r)
			}
			c.mu.Lock()
			c.busy[key] = false
			c.mu.Unlock()
		}()
		fn()
	}()
}

func (c *sysCollector) collect(services []string) *sysReport {
	r := &sysReport{CPU: -1, EvCrit: -1, EvErr: -1, Updates: -1, Reboot: -1, Ver: version}
	r.Host, _ = os.Hostname()
	collectFast(c, r)
	for _, s := range services {
		r.Svcs = append(r.Svcs, sysSvc{Name: s, State: serviceState(s)})
	}
	c.due("events", 5*time.Minute, func() {
		crit, errs, ok := eventCounts()
		c.mu.Lock()
		if ok {
			c.slow.evCrit, c.slow.evErr = crit, errs
		}
		c.mu.Unlock()
	})
	c.due("reboot", 15*time.Minute, func() {
		v := rebootPending()
		c.mu.Lock()
		c.slow.reboot = v
		c.mu.Unlock()
	})
	c.due("av", time.Hour, func() {
		av := antivirus()
		c.mu.Lock()
		c.slow.av = av
		c.mu.Unlock()
	})
	c.due("updates", 2*time.Hour, func() {
		n, drv, ok := pendingUpdates()
		c.mu.Lock()
		if ok {
			c.slow.updates, c.slow.updDrv, c.slow.updAt = n, drv, time.Now().UnixMilli()
		}
		c.mu.Unlock()
	})
	c.mu.Lock()
	r.EvCrit, r.EvErr = c.slow.evCrit, c.slow.evErr
	r.Updates, r.UpdDrv, r.UpdAt = c.slow.updates, c.slow.updDrv, c.slow.updAt
	r.AV, r.Reboot = c.slow.av, c.slow.reboot
	c.mu.Unlock()
	return r
}

// sysLoop: misst den Rechner, solange die Rechner-Überwachung am Standort eingeschaltet ist.
func (a *Agent) sysLoop() {
	col := newSysCollector()
	healer := newSelfHealer(restartService)
	machine := a.machine
	if machine == "" {
		machine = sysMachineID(a.dataDir)
	}
	var pend []sysSample
	lastErr := ""
	for {
		a.mu.Lock()
		on, svcs, sysOnly := a.sysOn, append([]string(nil), a.sysSvcs...), a.sysOnly
		healOn, restart := a.sysHeal, append([]string(nil), a.sysRestart...)
		a.mu.Unlock()
		if on {
			rep := col.collect(svcs)
			rep.NoHeal = a.file.NoSelfHeal
			if healOn {
				rep.Heal = healer.step(rep.Svcs, restart, svcs, time.Now())
			} else {
				healer.reset()
			}
			a.mu.Lock()
			due := time.Since(a.invAt) >= 24*time.Hour
			a.mu.Unlock()
			if due {
				go a.sendInventory(machine)
			}
			pend = append(pend, sysSample{T: time.Now().UnixMilli(), R: rep})
			if len(pend) > 1440 {
				pend = pend[len(pend)-1440:]
			}
			b, _ := json.Marshal(map[string]any{"machine": machine, "host": rep.Host, "sysOnly": sysOnly, "samples": pend})
			if resp, err := a.req("POST", "/api/agent/sys", bytes.NewReader(b), false); err == nil {
				msg := ""
				switch resp.StatusCode {
				case http.StatusOK:
					pend = nil
				case http.StatusPaymentRequired, http.StatusUnauthorized, http.StatusBadRequest:
					var buf bytes.Buffer
					buf.ReadFrom(resp.Body)
					msg = strings.TrimSpace(buf.String())
					pend = nil // abgelehnt – nicht weiter sammeln
				}
				resp.Body.Close()
				if msg != lastErr {
					if msg != "" {
						log.Printf("Rechner-Überwachung: %s", msg)
					}
					lastErr = msg
					a.mu.Lock()
					a.sysErr = msg
					a.mu.Unlock()
					a.changed()
				}
			}
		} else {
			pend = nil
		}
		select {
		case <-a.stopAll:
			return
		case <-time.After(sysPollEvery):
		}
	}
}

// ---------- Auswertung der Ausgaben (plattformunabhängig, getestet) ----------

// parseProcStat: Summe aller und der Leerlauf-Zeiten aus /proc/stat (erste Zeile „cpu“).
func parseProcStat(b []byte) (total, idle uint64, ok bool) {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		for i, v := range f[1:] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				continue
			}
			if i >= 8 { // guest/guest_nice sind in user/nice schon enthalten
				break
			}
			total += n
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			}
		}
		return total, idle, total > 0
	}
	return 0, 0, false
}

func cpuPct(t0, i0, t1, i1 uint64) float64 {
	if t1 <= t0 {
		return -1
	}
	dt, di := float64(t1-t0), float64(i1-i0)
	if di > dt {
		di = dt
	}
	return (dt - di) * 100 / dt
}

// parseMeminfo: Gesamt und belegt (gesamt − verfügbar) aus /proc/meminfo.
func parseMeminfo(b []byte) (total, used uint64, ok bool) {
	var avail, free, buffers, cached uint64
	hasAvail := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		n *= 1024
		switch f[0] {
		case "MemTotal:":
			total = n
		case "MemAvailable:":
			avail, hasAvail = n, true
		case "MemFree:":
			free = n
		case "Buffers:":
			buffers = n
		case "Cached:":
			cached = n
		}
	}
	if total == 0 {
		return 0, 0, false
	}
	if !hasAvail {
		avail = free + buffers + cached
	}
	if avail > total {
		avail = total
	}
	return total, total - avail, true
}

// parseAptCheck: „12;3“ (Updates;Sicherheitsupdates) von apt-check.
func parseAptCheck(b []byte) (int, bool) {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, ';'); i > 0 {
		if n, err := strconv.Atoi(s[:i]); err == nil {
			return n, true
		}
	}
	return -1, false
}

// parseAptList: Anzahl der Pakete aus „apt list --upgradable“.
func parseAptList(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "/") && strings.Contains(line, "upgradable") {
			n++
		}
	}
	return n
}

// parseSystemctlShow: Zustand eines Dienstes aus „systemctl show -p LoadState -p ActiveState“.
func parseSystemctlShow(b []byte) string {
	load, active := "", ""
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			load = v
		case "ActiveState":
			active = v
		}
	}
	switch {
	case load == "not-found":
		return "missing"
	case active == "active", active == "activating", active == "reloading":
		return "running"
	case active == "inactive", active == "failed", active == "deactivating":
		return "stopped"
	}
	return "unknown"
}

// parseEventCounts: Ausgabe „1=3“ / „2=17“ (Level=Anzahl) aus Get-WinEvent | Group-Object.
func parseEventCounts(b []byte) (crit, errs int, ok bool) {
	ok = true
	for _, line := range strings.Split(string(b), "\n") {
		k, v, found := strings.Cut(strings.Trim(line, " \t\r\ufeff"), "=")
		if !found {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "1":
			crit += n
		case "2":
			errs += n
		}
	}
	return
}

// parseJournalCount: Anzahl der Zeilen (journalctl -o cat).
func parseJournalCount(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "-- ") {
			n++
		}
	}
	return n
}

// parseAVJSON wertet das PowerShell-Ergebnis zu Defender und Security Center aus.
func parseAVJSON(b []byte) *sysAV {
	var raw struct {
		Def *struct {
			On  bool   `json:"on"`
			RT  bool   `json:"rt"`
			Age int    `json:"age"`
			AM  string `json:"am"`
		} `json:"def"`
		SC json.RawMessage `json:"sc"`
	}
	b = bytes.TrimSpace(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")))
	if len(b) == 0 || json.Unmarshal(b, &raw) != nil {
		return nil
	}
	type scProd struct {
		N  string `json:"n"`
		St int    `json:"st"`
	}
	var prods []scProd
	if len(raw.SC) > 0 {
		if raw.SC[0] == '[' {
			json.Unmarshal(raw.SC, &prods)
		} else {
			var one scProd
			if json.Unmarshal(raw.SC, &one) == nil && one.N != "" {
				prods = []scProd{one}
			}
		}
	}
	isDef := func(n string) bool {
		n = strings.ToLower(n)
		return strings.Contains(n, "defender") || strings.Contains(n, "windows-virenschutz")
	}
	// Läuft Defender im Normalmodus mit aktivem Schutz, ist er zuständig – Windows schaltet Defender
	// sonst in den passiven Modus. Einträge anderer Produkte sind dann Reste einer Deinstallation.
	if d := raw.Def; d != nil && strings.EqualFold(d.AM, "Normal") && d.On && d.RT {
		return &sysAV{Name: "Microsoft Defender", Enabled: true, RealTime: true, SigDays: d.Age}
	}
	// Security Center (Windows-Clients): aktives Produkt eines Drittanbieters bevorzugen
	var firstOff *scProd
	for i, p := range prods {
		mid := (p.St >> 8) & 0xFF
		on := mid == 0x10 || mid == 0x11
		if on && !isDef(p.N) {
			return &sysAV{Name: p.N, Enabled: true, RealTime: true, SigDays: -1}
		}
		if on && raw.Def == nil {
			return &sysAV{Name: p.N, Enabled: true, RealTime: true, SigDays: -1}
		}
		if !on && firstOff == nil {
			firstOff = &prods[i]
		}
	}
	if d := raw.Def; d != nil {
		if d.AM != "" && !strings.EqualFold(d.AM, "Normal") && !d.On {
			// Defender im passiven Modus – ein anderes Produkt ist zuständig, das wir nicht kennen
			if firstOff == nil {
				return nil
			}
		} else {
			return &sysAV{Name: "Microsoft Defender", Enabled: d.On, RealTime: d.RT, SigDays: d.Age}
		}
	}
	if firstOff != nil {
		return &sysAV{Name: firstOff.N, Enabled: false, SigDays: -1}
	}
	return nil
}

// parseLaunchctl: Zustand eines launchd-Dienstes aus „launchctl list“ (PID  Status  Label).
func parseLaunchctl(b []byte, name string) string {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.EqualFold(f[2], name) {
			continue
		}
		if f[0] != "-" {
			return "running"
		}
		return "stopped"
	}
	return "missing"
}

// parseVMStat: belegter Speicher aus „vm_stat“ (aktiv + wired + komprimiert).
func parseVMStat(b []byte) (used uint64, ok bool) {
	page := uint64(4096)
	var active, wired, comp uint64
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "page size of "); i >= 0 {
			if n, err := strconv.ParseUint(strings.Fields(line[i+13:])[0], 10, 64); err == nil {
				page = n
			}
			continue
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages active":
			active = n
		case "Pages wired down":
			wired = n
		case "Pages occupied by compressor":
			comp = n
		}
	}
	if active+wired == 0 {
		return 0, false
	}
	return (active + wired + comp) * page, true
}

// parseTopCPU: CPU-Last aus der letzten „CPU usage: … idle“-Zeile von top (macOS).
func parseTopCPU(b []byte) float64 {
	res := -1.0
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "CPU usage:") {
			continue
		}
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			if strings.HasSuffix(part, "idle") {
				v := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(part, "idle"), " "))
				v = strings.TrimSuffix(v, "%")
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					res = 100 - f
				}
			}
		}
	}
	return res
}

// parseOSRelease: PRETTY_NAME aus /etc/os-release.
func parseOSRelease(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// winOSName: „Windows 11 Pro 23H2 (Build 22631)“ aus den Registry-Werten.
func winOSName(product, display, build string) string {
	if n, err := strconv.Atoi(build); err == nil && n >= 22000 {
		product = strings.Replace(product, "Windows 10", "Windows 11", 1)
	}
	out := product
	if display != "" {
		out += " " + display
	}
	if build != "" {
		out += " (Build " + build + ")"
	}
	return strings.TrimSpace(out)
}

// parseSoftwareUpdate: Anzahl der Einträge aus „softwareupdate -l“.
func parseSoftwareUpdate(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "* Label:") {
			n++
		}
	}
	return n
}

// runCmdStderr wie runCmd, liefert aber stderr (apt-check).
func runCmdStderr(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	var out bytes.Buffer
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// ---------- Selbstheilung: gestoppte Dienste neu starten (eng begrenzt) ----------

// selfHealer startet nur Dienste, die (1) im Dashboard ausdrücklich zum Neustart markiert sind,
// (2) zugleich überwacht werden und (3) einen unverdächtigen Namen haben. Höchstens 3 Versuche
// mit mindestens 1 Minute Abstand; danach wird aufgegeben, bis der Dienst wieder läuft.
type selfHealer struct {
	restart func(name string) error
	st      map[string]*healState
	verify  func(name string) string // Zustand nach dem Versuch (für Tests austauschbar)
	wait    time.Duration
}

type healState struct {
	tries  int
	last   time.Time
	gaveUp bool
}

const healMaxTries = 3

func newSelfHealer(restart func(string) error) *selfHealer {
	return &selfHealer{restart: restart, st: map[string]*healState{}, verify: serviceState, wait: 5 * time.Second}
}

func (h *selfHealer) reset() { h.st = map[string]*healState{} }

func (h *selfHealer) step(svcs []sysSvc, restart, monitored []string, now time.Time) []sysHeal {
	allowed := map[string]bool{}
	mon := map[string]bool{}
	for _, s := range monitored {
		mon[strings.ToLower(s)] = true
	}
	for _, s := range restart {
		if validServiceName(s) && mon[strings.ToLower(s)] {
			allowed[strings.ToLower(s)] = true
		}
	}
	var out []sysHeal
	for i, s := range svcs {
		k := strings.ToLower(s.Name)
		if !allowed[k] || !validServiceName(s.Name) {
			continue
		}
		st := h.st[k]
		if s.State == "running" {
			delete(h.st, k) // läuft wieder → Zähler zurücksetzen
			continue
		}
		if s.State != "stopped" {
			continue // „missing“/„unknown“: nichts tun
		}
		if st == nil {
			st = &healState{}
			h.st[k] = st
		}
		if st.gaveUp || st.tries >= healMaxTries || (!st.last.IsZero() && now.Sub(st.last) < time.Minute) {
			continue
		}
		st.tries++
		st.last = now
		ev := sysHeal{Svc: s.Name, T: now.UnixMilli(), Try: st.tries}
		err := h.restart(s.Name)
		if err == nil && h.verify != nil {
			if h.wait > 0 {
				time.Sleep(h.wait)
			}
			if v := h.verify(s.Name); v != "running" {
				err = fmt.Errorf("Dienst läuft nach dem Start nicht (Zustand: %s)", v)
			}
		}
		if err != nil {
			ev.Err = err.Error()
			if st.tries >= healMaxTries {
				st.gaveUp, ev.GaveUp = true, true
			}
			log.Printf("Selbstheilung: Neustart von %s fehlgeschlagen (Versuch %d): %v", s.Name, st.tries, err)
		} else {
			ev.OK = true
			svcs[i].State = "running"
			log.Printf("Selbstheilung: Dienst %s neu gestartet (Versuch %d)", s.Name, st.tries)
			delete(h.st, k)
		}
		out = append(out, ev)
	}
	return out
}

// ---------- Inventur ----------

// sendInventory liest das Inventar (nur lesend) und schickt es an den Server.
func (a *Agent) sendInventory(machine string) error {
	a.invMu.Lock()
	if a.invBusy {
		a.invMu.Unlock()
		return fmt.Errorf("Inventur läuft bereits")
	}
	a.invBusy = true
	a.invMu.Unlock()
	defer func() {
		a.invMu.Lock()
		a.invBusy = false
		a.invMu.Unlock()
	}()
	inv := collectInventory()
	inv.At = time.Now().UnixMilli()
	b, _ := json.Marshal(map[string]any{"machine": machine, "inv": inv})
	resp, err := a.req("POST", "/api/agent/inventory", bytes.NewReader(b), false)
	ok := err == nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}
	a.mu.Lock()
	if ok {
		a.invAt = time.Now()
	} else {
		a.invAt = time.Now().Add(-23 * time.Hour) // in einer Stunde erneut versuchen
	}
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Server: %s", resp.Status)
	}
	log.Printf("Inventur übertragen (%d Programme)", len(inv.SW))
	return nil
}

// netInventory: Netzwerkadapter (plattformunabhängig).
func netInventory() []invNIC {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []invNIC
	for _, it := range ifs {
		if it.Flags&net.FlagLoopback != 0 || len(it.HardwareAddr) == 0 {
			continue
		}
		n := invNIC{Name: it.Name, MAC: strings.ToUpper(it.HardwareAddr.String())}
		if addrs, err := it.Addrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
					n.IPs = append(n.IPs, ipn.IP.String())
				}
			}
		}
		if len(n.IPs) == 0 && it.Flags&net.FlagUp == 0 {
			continue // inaktive Adapter ohne Adresse weglassen
		}
		out = append(out, n)
	}
	return out
}

// parseDpkg: „Paket<TAB>Version<TAB>Maintainer“ je Zeile.
func parseDpkg(b []byte) []invSW {
	var out []invSW
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		s := invSW{Name: f[0], Version: f[1]}
		if len(f) > 2 {
			s.Vendor = f[2]
			if i := strings.Index(s.Vendor, " <"); i > 0 {
				s.Vendor = s.Vendor[:i]
			}
		}
		out = append(out, s)
	}
	return out
}

// parseRpm: „Name<TAB>Version<TAB>Hersteller<TAB>Installationszeit (Unix)“.
func parseRpm(b []byte) []invSW {
	var out []invSW
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		s := invSW{Name: f[0], Version: f[1]}
		if len(f) > 2 && f[2] != "(none)" {
			s.Vendor = f[2]
		}
		if len(f) > 3 {
			if sec, err := strconv.ParseInt(f[3], 10, 64); err == nil && sec > 0 {
				s.Installed = time.Unix(sec, 0).Format("2006-01-02")
			}
		}
		out = append(out, s)
	}
	return out
}

// parseWinInventory wertet das JSON des PowerShell-Skripts (Windows) aus.
func parseWinInventory(b []byte) (*sysInventory, error) {
	b = bytes.TrimSpace(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")))
	var raw struct {
		HW struct {
			Vendor, Model, Serial, BIOS, CPU string
			Cores, Threads                   int
			RAM                              uint64
			Disks                            json.RawMessage
		} `json:"hw"`
		OS struct {
			Name, Version, Build string
			Installed, Boot      int64
			Domain               string
			PartOfDomain         bool `json:"partOfDomain"`
			User                 string
			Lic                  int `json:"lic"`
		} `json:"os"`
		SW json.RawMessage `json:"sw"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	inv := &sysInventory{}
	inv.HW = invHW{Vendor: raw.HW.Vendor, Model: raw.HW.Model, Serial: raw.HW.Serial, BIOS: raw.HW.BIOS, CPU: raw.HW.CPU,
		Cores: raw.HW.Cores, Threads: raw.HW.Threads, RAM: raw.HW.RAM}
	type d struct {
		M string `json:"m"`
		S uint64 `json:"s"`
	}
	var disks []d
	jsonList(raw.HW.Disks, &disks)
	for _, x := range disks {
		inv.HW.Disks = append(inv.HW.Disks, invDisk{Model: x.M, Size: x.S})
	}
	inv.OS = invOS{Name: raw.OS.Name, Version: raw.OS.Version, Build: raw.OS.Build, Installed: raw.OS.Installed, Boot: raw.OS.Boot,
		Domain: raw.OS.Domain, InDomain: raw.OS.PartOfDomain, User: raw.OS.User, Activation: winLicense(raw.OS.Lic)}
	type sw struct{ N, V, P, D string }
	var list []sw
	jsonList(raw.SW, &list)
	for _, s := range list {
		inst := ""
		if len(s.D) == 8 { // YYYYMMDD aus der Registry
			inst = s.D[:4] + "-" + s.D[4:6] + "-" + s.D[6:]
		}
		inv.SW = append(inv.SW, invSW{Name: s.N, Version: s.V, Vendor: s.P, Installed: inst})
	}
	return inv, nil
}

// jsonList: PowerShell liefert bei einem Element ein Objekt statt einer Liste.
func jsonList[T any](raw json.RawMessage, out *[]T) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	if raw[0] == '[' {
		json.Unmarshal(raw, out)
		return
	}
	var one T
	if json.Unmarshal(raw, &one) == nil {
		*out = append(*out, one)
	}
}

func winLicense(s int) string {
	switch s {
	case 0:
		return "nicht aktiviert"
	case 1:
		return "aktiviert"
	case 2, 3, 6:
		return "Kulanzzeit"
	case 4:
		return "nicht echt (Kulanzzeit)"
	case 5:
		return "Benachrichtigung (nicht aktiviert)"
	}
	return ""
}

// parseMacProfiler: system_profiler SPHardwareDataType SPApplicationsDataType -json
func parseMacProfiler(b []byte) *sysInventory {
	var raw struct {
		HW []map[string]any `json:"SPHardwareDataType"`
		SW []map[string]any `json:"SPApplicationsDataType"`
	}
	inv := &sysInventory{}
	if json.Unmarshal(b, &raw) != nil {
		return inv
	}
	str := func(m map[string]any, k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	if len(raw.HW) > 0 {
		h := raw.HW[0]
		inv.HW.Vendor = "Apple"
		inv.HW.Model = strings.TrimSpace(str(h, "machine_name") + " (" + str(h, "machine_model") + ")")
		inv.HW.Serial = str(h, "serial_number")
		inv.HW.BIOS = str(h, "boot_rom_version")
		inv.HW.CPU = str(h, "chip_type")
		if inv.HW.CPU == "" {
			inv.HW.CPU = str(h, "cpu_type")
		}
		if p := str(h, "number_processors"); p != "" { // „proc 8:4:4“
			f := strings.FieldsFunc(p, func(r rune) bool { return r == ' ' || r == ':' })
			if len(f) >= 2 {
				inv.HW.Cores, _ = strconv.Atoi(f[1])
			}
		}
		if mem := strings.Fields(str(h, "physical_memory")); len(mem) == 2 {
			if n, err := strconv.ParseFloat(mem[0], 64); err == nil {
				mul := float64(1 << 30)
				if mem[1] == "TB" {
					mul = 1 << 40
				}
				inv.HW.RAM = uint64(n * mul)
			}
		}
	}
	for _, a := range raw.SW {
		s := invSW{Name: str(a, "_name"), Version: str(a, "version"), Vendor: str(a, "obtained_from")}
		if t := str(a, "lastModified"); len(t) >= 10 {
			s.Installed = t[:10]
		}
		if s.Name != "" {
			inv.SW = append(inv.SW, s)
		}
	}
	return inv
}

// runCmdCombined wie runCmd, liefert stdout und stderr zusammen.
func runCmdCombined(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	return cmd.CombinedOutput()
}

// parseWinUpdates: „6;2“ (gesamt;davon Treiber) bzw. ältere Ausgabe „6“.
func parseWinUpdates(out string) (int, int, bool) {
	for _, line := range strings.Split(out, "\n") {
		v := strings.TrimSpace(line)
		if v == "" {
			continue
		}
		a, b, _ := strings.Cut(v, ";")
		n, ok := atoiSafe(strings.TrimSpace(a))
		if !ok || n < 0 {
			continue
		}
		d, _ := atoiSafe(strings.TrimSpace(b))
		if d > n {
			d = n
		}
		return n, d, true
	}
	return -1, 0, false
}

func atoiSafe(s string) (int, bool) {
	n := 0
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

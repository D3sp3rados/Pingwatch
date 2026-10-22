package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
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
	c.due("updates", 6*time.Hour, func() {
		n, ok := pendingUpdates()
		c.mu.Lock()
		if ok {
			c.slow.updates, c.slow.updAt = n, time.Now().UnixMilli()
		}
		c.mu.Unlock()
	})
	c.mu.Lock()
	r.EvCrit, r.EvErr = c.slow.evCrit, c.slow.evErr
	r.Updates, r.UpdAt = c.slow.updates, c.slow.updAt
	r.AV, r.Reboot = c.slow.av, c.slow.reboot
	c.mu.Unlock()
	return r
}

// sysLoop: misst den Rechner, solange die Rechner-Überwachung am Standort eingeschaltet ist.
func (a *Agent) sysLoop() {
	col := newSysCollector()
	machine := a.machine
	if machine == "" {
		machine = sysMachineID(a.dataDir)
	}
	var pend []sysSample
	lastErr := ""
	for {
		a.mu.Lock()
		on, svcs, sysOnly := a.sysOn, append([]string(nil), a.sysSvcs...), a.sysOnly
		a.mu.Unlock()
		if on {
			rep := col.collect(svcs)
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

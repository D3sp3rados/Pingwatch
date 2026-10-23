package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

func TestSysParsers(t *testing.T) {
	tot, idle, ok := parseProcStat([]byte("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4\n"))
	if !ok || tot != 1000 || idle != 850 {
		t.Fatalf("procstat: %d %d %v", tot, idle, ok)
	}
	if p := cpuPct(1000, 850, 2000, 1350); p < 49.9 || p > 50.1 {
		t.Fatalf("cpuPct: %f", p)
	}
	if p := cpuPct(1000, 850, 1000, 850); p != -1 {
		t.Fatal("cpuPct ohne Zeit")
	}
	total, used, ok := parseMeminfo([]byte("MemTotal:  8000000 kB\nMemFree: 1000000 kB\nMemAvailable: 6000000 kB\n"))
	if !ok || total != 8000000*1024 || used != 2000000*1024 {
		t.Fatalf("meminfo: %d %d", total, used)
	}
	if _, used, _ := parseMeminfo([]byte("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 100 kB\nCached: 300 kB\n")); used != 500*1024 {
		t.Fatalf("meminfo ohne MemAvailable: %d", used)
	}
	if n, ok := parseAptCheck([]byte("12;3")); !ok || n != 12 {
		t.Fatal("apt-check", n)
	}
	if _, ok := parseAptCheck([]byte("Fehler")); ok {
		t.Fatal("apt-check Fehler")
	}
	if n := parseAptList([]byte("Listing... Done\nopenssl/stable 3.0.14 amd64 [upgradable from: 3.0.13]\ncurl/stable 8 amd64 [upgradable from: 7]\n")); n != 2 {
		t.Fatal("apt list", n)
	}
	for in, want := range map[string]string{
		"LoadState=loaded\nActiveState=active\n":      "running",
		"LoadState=loaded\nActiveState=failed\n":      "stopped",
		"LoadState=not-found\nActiveState=inactive\n": "missing",
		"": "unknown",
	} {
		if got := parseSystemctlShow([]byte(in)); got != want {
			t.Errorf("systemctl %q: %s", in, got)
		}
	}
	if c, e, ok := parseEventCounts([]byte("\ufeff1=2\r\n2=17\r\n")); !ok || c != 2 || e != 17 {
		t.Fatalf("Ereignisse: %d %d", c, e)
	}
	if c, e, _ := parseEventCounts([]byte("0=0\n")); c != 0 || e != 0 {
		t.Fatal("keine Ereignisse")
	}
	if n := parseJournalCount([]byte("a\n\nb\n-- No entries --\n")); n != 2 {
		t.Fatal("journal", n)
	}
	if p := parseLaunchctl([]byte("PID\tStatus\tLabel\n123\t0\tcom.apple.smbd\n-\t0\tcom.example.backup\n"), "com.example.backup"); p != "stopped" {
		t.Fatal("launchctl", p)
	}
	if p := parseLaunchctl([]byte("123\t0\tcom.apple.smbd\n"), "com.apple.smbd"); p != "running" {
		t.Fatal("launchctl running", p)
	}
	if p := parseLaunchctl([]byte("123\t0\tcom.apple.smbd\n"), "x"); p != "missing" {
		t.Fatal("launchctl missing", p)
	}
	vm := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 100.\nPages active: 1000.\nPages wired down: 500.\nPages occupied by compressor: 100.\n"
	if u, ok := parseVMStat([]byte(vm)); !ok || u != 1600*16384 {
		t.Fatal("vm_stat", u)
	}
	if c := parseTopCPU([]byte("CPU usage: 50.0% user, 10.0% sys, 40.0% idle\nCPU usage: 5.5% user, 4.5% sys, 90.0% idle\n")); c < 9.9 || c > 10.1 {
		t.Fatal("top", c)
	}
	if s := parseOSRelease([]byte("NAME=Debian\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n")); s != "Debian GNU/Linux 12 (bookworm)" {
		t.Fatal("os-release", s)
	}
	if s := winOSName("Windows 10 Pro", "23H2", "22631"); s != "Windows 11 Pro 23H2 (Build 22631)" {
		t.Fatal("Windows-Name", s)
	}
	if n := parseSoftwareUpdate([]byte("Software Update found the following:\n* Label: macOS 14.6\n\tTitle: …\n* Label: Safari\n")); n != 2 {
		t.Fatal("softwareupdate", n)
	}
	// PowerShell -EncodedCommand: UTF-16LE + Base64
	raw, _ := base64.StdEncoding.DecodeString(psEncoded("ä=1"))
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if string(utf16.Decode(u)) != "ä=1" {
		t.Fatal("psEncoded")
	}
}

func TestParseAV(t *testing.T) {
	cases := []struct {
		in      string
		nilWant bool
		name    string
		on, rt  bool
		sig     int
	}{
		{in: `{"def":{"on":true,"rt":true,"age":1,"am":"Normal"},"sc":[{"n":"Windows Defender","st":397568}]}`, name: "Microsoft Defender", on: true, rt: true, sig: 1},
		{in: `{"def":{"on":true,"rt":false,"age":5,"am":"Normal"}}`, name: "Microsoft Defender", on: true, rt: false, sig: 5},
		// Drittanbieter aktiv, Defender passiv
		{in: `{"def":{"on":false,"rt":false,"age":30,"am":"Passive Mode"},"sc":[{"n":"Windows Defender","st":393472},{"n":"ESET Security","st":266240}]}`, name: "ESET Security", on: true, rt: true, sig: -1},
		// Reste eines deinstallierten Drittanbieters im Security Center, Defender läuft normal → Defender
		{in: `{"def":{"on":true,"rt":true,"age":0,"am":"Normal"},"sc":[{"n":"Norton 360","st":266240},{"n":"Windows Defender","st":397568}]}`, name: "Microsoft Defender", on: true, rt: true, sig: 0},
		// einzelnes Produkt als Objekt statt Liste
		{in: "\ufeff" + `{"sc":{"n":"Sophos","st":266240}}`, name: "Sophos", on: true, rt: true, sig: -1},
		// alles aus
		{in: `{"sc":[{"n":"Windows Defender","st":393472}]}`, name: "Windows Defender", on: false, sig: -1},
		// Defender passiv, nichts anderes bekannt → unbekannt
		{in: `{"def":{"on":false,"rt":false,"age":3,"am":"Passive Mode"}}`, nilWant: true},
		{in: `{}`, nilWant: true},
		{in: ``, nilWant: true},
	}
	for _, c := range cases {
		av := parseAVJSON([]byte(c.in))
		if c.nilWant {
			if av != nil {
				t.Errorf("%s: erwartet nil, bekam %+v", c.in, av)
			}
			continue
		}
		if av == nil || av.Name != c.name || av.Enabled != c.on || av.RealTime != c.rt || av.SigDays != c.sig {
			t.Errorf("%s: %+v", c.in, av)
		}
	}
}

func TestSysConfigNorm(t *testing.T) {
	c := SysCfg{On: true, Services: []string{" Spooler ", "spooler", "", "MSSQLSERVER"}, DiskPct: 200, CPUPct: -1}
	c.norm()
	if c.DiskPct != 99 || c.CPUPct != 95 || c.MemPct != 95 || c.HighMin != 15 || c.EvCrit != 1 || c.AVSigDays != 3 || c.RebootDays != 7 {
		t.Fatalf("Standardwerte: %+v", c)
	}
	if len(c.Services) != 2 || c.Services[0] != "Spooler" {
		t.Fatalf("Dienste: %v", c.Services)
	}
	var nilCfg *SysCfg
	if e := nilCfg.eff(); e.DiskPct != 90 {
		t.Fatal("eff nil")
	}
}

func TestSysIngest(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	m.notices = make(chan notice, 100)
	site := m.UpsertSite(SiteCfg{Name: "Büro", Org: "o1", Sys: &SysCfg{On: true, HighMin: 5}})
	m.cfg.Orgs = append(m.cfg.Orgs, Org{ID: "o1", Name: "Kunde A", Plan: "pro"})
	mc, err := m.registerMachine(site, "abc123", "SRV-01", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := site.Sys.eff()
	const g = 1 << 30
	rep := func(cpu float64, cUsed float64) *sysReport {
		return &sysReport{Host: "SRV-01", CPU: cpu, MemTotal: 16 * g, MemUsed: 4 * g, EvCrit: 0, Reboot: 0,
			Disks: []sysDisk{{Mount: "C:", Total: 100 * g, Free: uint64((100 - cUsed) * g)}},
			Svcs:  []sysSvc{{Name: "Spooler", State: "running"}}}
	}
	now := time.Now().UnixMilli()
	// nachgereichte, alte Messungen: nur Verlauf, keine Meldung
	m.ingestSys(mc, cfg, []sysSample{{T: now - 3600000, R: rep(99, 99)}, {T: now - 3500000, R: rep(99, 99)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("alte Messungen melden: %+v", n)
	}
	// Laufwerk voll → sofort
	m.ingestSys(mc, cfg, []sysSample{{T: now - 60000, R: rep(10, 95)}})
	n := drainNotices(m)
	if len(n) != 1 || n[0].kind != "syswarn" || !strings.Contains(n[0].title, "Laufwerk fast voll") || !strings.Contains(n[0].body, "C:") || n[0].hostID != "m:"+mc.ID || n[0].org != "o1" {
		t.Fatalf("Laufwerk: %+v", n)
	}
	// Hysterese: 89 % ist noch nicht „gut“
	m.ingestSys(mc, cfg, []sysSample{{T: now - 50000, R: rep(10, 89)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Hysterese: %+v", n)
	}
	if v := m.machineView(mc, true); v.Status != "warn" || len(v.Problems) != 1 || !strings.Contains(v.Problems[0], "Laufwerk C:") {
		t.Fatalf("Ansicht warn: %+v", v)
	}
	m.ingestSys(mc, cfg, []sysSample{{T: now - 40000, R: rep(10, 80)}})
	if n := drainNotices(m); len(n) != 1 || n[0].kind != "sysok" {
		t.Fatalf("Entwarnung: %+v", n)
	}
	// CPU: erst nach HighMin (5 min) dauerhaft hoch
	t0 := now - 290000
	m2, _ := m.registerMachine(site, "cpu1", "PC-CPU", true)
	m.ingestSys(m2, cfg, []sysSample{{T: t0, R: rep(99, 50)}, {T: t0 + 120000, R: rep(98, 50)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("CPU zu früh: %+v", n)
	}
	m.ingestSys(m2, cfg, []sysSample{{T: t0 + 300000, R: rep(97, 50)}})
	if n := drainNotices(m); len(n) != 1 || !strings.Contains(n[0].title, "CPU") {
		t.Fatalf("CPU: %+v", n)
	}
	// Dienst gestoppt (2 min), Virenschutz aus (5 min)
	r := rep(10, 50)
	r.Svcs = []sysSvc{{Name: "Spooler", State: "stopped"}}
	r.AV = &sysAV{Name: "Microsoft Defender", Enabled: false, SigDays: 0}
	m3, _ := m.registerMachine(site, "svc1", "PC-SVC", true)
	m.ingestSys(m3, cfg, []sysSample{{T: now - 305000, R: r}, {T: now, R: r}})
	n = drainNotices(m)
	if len(n) != 2 {
		t.Fatalf("Dienst/Virenschutz: %+v", n)
	}
	if v := m.machineView(m3, true); v.Status != "bad" || v.SvcN != 1 || v.SvcOK != 0 {
		t.Fatalf("Ansicht bad: %+v", v)
	}
	// Wartung des ganzen Kunden: keine Meldung, Ereignis ohne Meldung
	m.cfg.Maints = append(m.cfg.Maints, Maint{ID: "w", Org: "o1", Start: now - 1000, End: now + 3600000})
	m4, _ := m.registerMachine(site, "mt1", "PC-MAINT", true)
	m.ingestSys(m4, cfg, []sysSample{{T: now, R: rep(10, 97)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Meldung trotz Wartung: %+v", n)
	}
	m.cfg.Maints = nil
	// Stummschaltung nur dieses Rechners (wie /api/mute scope=machine)
	m.cfg.Maints = append(m.cfg.Maints, Maint{ID: "w2", Org: "o1", Hosts: []string{"m:" + mc.ID}, Start: now - 1000, End: now + 3600000})
	m.ingestSys(mc, cfg, []sysSample{{T: now + 1000, R: rep(10, 97)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Meldung trotz Stummschaltung: %+v", n)
	}
	m.cfg.Maints = nil
	// Neustart ausstehend: einmalig nach RebootDays
	sm := getSysMachine(m2.ID)
	rr := rep(10, 50)
	rr.Reboot = 1
	m.ingestSys(m2, cfg, []sysSample{{T: now + 20000, R: rr}})
	sm.mu.Lock()
	sm.rebootAt = now - 8*24*3600*1000
	sm.mu.Unlock()
	m.ingestSys(m2, cfg, []sysSample{{T: now + 21000, R: rr}})
	m.ingestSys(m2, cfg, []sysSample{{T: now + 22000, R: rr}})
	reb := 0
	for _, x := range drainNotices(m) {
		if strings.Contains(x.title, "Neustart ausstehend") {
			reb++
		}
	}
	if reb != 1 {
		t.Fatalf("Neustart-Meldungen: %d", reb)
	}
	// kritische Ereignisse: nur Ereignis, keine Meldung, höchstens stündlich
	re := rep(10, 50)
	re.EvCrit = 3
	m.ingestSys(m2, cfg, []sysSample{{T: now + 23000, R: re}, {T: now + 24000, R: re}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Ereignisprotokoll darf nicht pushen: %+v", n)
	}
	cnt := 0
	m.evMu.Lock()
	for _, e := range m.events {
		if e.HostID == "m:"+m2.ID && strings.Contains(e.HostName, "kritische Ereignisse") {
			cnt++
		}
	}
	m.evMu.Unlock()
	if cnt != 1 {
		t.Fatalf("Ereignisse kritisch: %d", cnt)
	}
	// Verlauf: 24 h und 7 Tage (15-Minuten-Raster)
	sm1 := getSysMachine(mc.ID)
	sm1.mu.Lock()
	h, h7 := len(sm1.hist), len(sm1.hist7)
	sm1.mu.Unlock()
	if h < 5 || h7 < 2 || h7 > h {
		t.Fatalf("Verlauf: %d / %d", h, h7)
	}
	// „nur Rechner“-Agent meldet sich nicht mehr → Meldung, danach wieder da
	sm3 := getSysMachine(m3.ID)
	sm3.mu.Lock()
	sm3.seen = now - 10*60000
	sm3.mu.Unlock()
	// Arbeitsplatz (kein Server-Betriebssystem) meldet „aus“ standardmäßig nicht
	sm3.mu.Lock()
	sm3.cur.OS = "Windows 11 Pro"
	sm3.at = now - 10*60000
	sm3.mu.Unlock()
	m.checkMachines(now)
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Arbeitsplatz aus gemeldet: %+v", n)
	}
	if v := m.machineView(m3, true); v.Status != "sleep" {
		t.Fatalf("Arbeitsplatz aus: %s", v.Status)
	}
	// … außer es ist ausdrücklich eingeschaltet
	m.mu.Lock()
	for i := range m.cfg.Machines {
		if m.cfg.Machines[i].ID == m3.ID {
			m.cfg.Machines[i].OffAlert = "on"
		}
	}
	m.mu.Unlock()
	m3, _ = m.machineByID(m3.ID)
	m.checkMachines(now)
	n = drainNotices(m)
	if len(n) != 1 || n[0].kind != "sysoff" {
		t.Fatalf("offline: %+v", n)
	}
	// weitere Prüfungen und ein Server-Neustart (Speicherzustand weg) melden nicht erneut
	m.checkMachines(now + 60000)
	sm3.mu.Lock()
	sm3.offEv, sm3.seen = nil, 0
	sm3.mu.Unlock()
	m3, _ = m.machineByID(m3.ID)
	if m3.OffNoted == 0 {
		t.Fatal("OffNoted nicht gespeichert")
	}
	m.checkMachines(now + 20*60000)
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("nach Neustart erneut gemeldet: %+v", n)
	}
	m.machineSeen(m3)
	if n := drainNotices(m); len(n) != 1 || n[0].kind != "syson" {
		t.Fatalf("wieder da nach Neustart: %+v", n)
	}
	m3, _ = m.machineByID(m3.ID)
	if m3.OffNoted != 0 {
		t.Fatal("OffNoted nicht gelöscht")
	}
	// erneuter Ausfall → wieder genau eine Meldung, danach wieder da
	sm3.mu.Lock()
	sm3.seen = now - 10*60000
	sm3.mu.Unlock()
	m.checkMachines(now)
	if n := drainNotices(m); len(n) != 1 {
		t.Fatalf("zweiter Ausfall: %+v", n)
	}
	m3, _ = m.machineByID(m3.ID)
	m.machineSeen(m3)
	if n := drainNotices(m); len(n) != 1 || n[0].kind != "syson" {
		t.Fatalf("wieder da: %+v", n)
	}
	// Standort-Agent selbst (nicht sysOnly) wird über „Agent offline“ gemeldet, nicht doppelt
	smA := getSysMachine(mc.ID)
	smA.mu.Lock()
	smA.seen = now - 10*60000
	smA.mu.Unlock()
	m.checkMachines(now)
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Standort-Agent doppelt gemeldet: %+v", n)
	}
	// Geräte bekommen Rechner-Meldungen auch bei Host-Auswahl
	if !(Device{Outage: true, Hosts: []string{"x"}}).wants(notice{kind: "syswarn", hostID: "m:" + mc.ID}) {
		t.Fatal("Device.wants syswarn")
	}
	if (Device{}).wants(notice{kind: "sysoff"}) {
		t.Fatal("Device ohne Ausfälle/Offline")
	}
}

func TestMachineLimits(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	m.cfg.Orgs = append(m.cfg.Orgs, Org{ID: "b", Plan: "basis"}, Org{ID: "f", Plan: "free"}, Org{ID: "i", Plan: "internal"})
	sb := m.UpsertSite(SiteCfg{Name: "B", Org: "b", Sys: &SysCfg{On: true}})
	for i := 0; i < 5; i++ {
		if _, err := m.registerMachine(sb, "m"+string(rune('a'+i)), "PC", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.registerMachine(sb, "zz", "PC", true); err == nil || !strings.Contains(err.Error(), "5 Rechner") {
		t.Fatalf("Limit Basis: %v", err)
	}
	// vorhandener Rechner darf sich weiter melden
	if _, err := m.registerMachine(sb, "ma", "PC-NEU", true); err != nil {
		t.Fatal(err)
	}
	sf := m.UpsertSite(SiteCfg{Name: "F", Org: "f", Sys: &SysCfg{On: true}})
	if _, err := m.registerMachine(sf, "x", "PC", false); err == nil {
		t.Fatal("Free darf keine Rechner")
	}
	si := m.UpsertSite(SiteCfg{Name: "I", Org: "i", Sys: &SysCfg{On: true}})
	for i := 0; i < 40; i++ {
		if _, err := m.registerMachine(si, "i"+string(rune('a'+i)), "PC", true); err != nil {
			t.Fatal("Intern unbegrenzt:", err)
		}
	}
	// Umbenennung durch Rechnernamen nur, solange der Name nicht geändert wurde
	mc, _ := m.registerMachine(sb, "ma", "PC-NEU", true)
	if mc.Name != "PC-NEU" {
		t.Fatalf("Name: %+v", mc)
	}
	// Konfiguration wird gespeichert und wieder geladen
	dir := t.TempDir()
	m2 := NewMonitor(NewFileStore(dir))
	m2.cfg.Machines = append(m2.cfg.Machines, mc)
	m2.cfg.Sites = append(m2.cfg.Sites, SiteCfg{ID: "s", Name: "x", Sys: &SysCfg{On: true, Services: []string{"Spooler"}}})
	m2.mu.Lock()
	m2.saveConfigLocked()
	m2.mu.Unlock()
	m3 := NewMonitor(NewFileStore(dir))
	if len(m3.cfg.Machines) != 1 || m3.cfg.Machines[0].Name != "PC-NEU" || m3.cfg.Sites[0].Sys == nil || m3.cfg.Sites[0].Sys.Services[0] != "Spooler" {
		t.Fatalf("Speichern: %+v %+v", m3.cfg.Machines, m3.cfg.Sites)
	}
	// Standort löschen entfernt seine Rechner
	m.DeleteSite(sb.ID)
	for _, x := range m.cfg.Machines {
		if x.Site == sb.ID {
			t.Fatal("Rechner des gelöschten Standorts übrig")
		}
	}
}

func TestSysAPI(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	agent := func(method, path string, v any, hdr map[string]string) (int, string) {
		var rd io.Reader
		if v != nil {
			b, _ := json.Marshal(v)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+site.Token)
		for k, x := range hdr {
			req.Header.Set(k, x)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, cfg := agent("GET", "/api/agent/config", nil, nil); !strings.Contains(cfg, `"sys":{"on":false}`) {
		t.Fatal("sys aus:", cfg)
	}
	now := time.Now().UnixMilli()
	rep := &sysReport{Host: "SRV-DC01", OS: "Windows Server 2022", CPU: 12, MemTotal: 8 << 30, MemUsed: 2 << 30, EvCrit: -1, EvErr: -1, Updates: 3, Reboot: 0,
		Disks: []sysDisk{{Mount: "C:", Total: 100 << 30, Free: 40 << 30}}, Svcs: []sysSvc{{Name: "Spooler", State: "running"}}}
	if _, b := agent("POST", "/api/agent/sys", map[string]any{"machine": "m1", "host": "SRV-DC01", "samples": []sysSample{{T: now, R: rep}}}, nil); !strings.Contains(b, `"off":true`) {
		t.Fatal("ausgeschaltet:", b)
	}
	// einschalten
	code, body := c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "services": []string{"Spooler", " "}, "diskPct": 85}})
	if code != 200 || !strings.Contains(body, `"diskPct":85`) {
		t.Fatal(code, body)
	}
	if _, cfg := agent("GET", "/api/agent/config", nil, nil); !strings.Contains(cfg, `"sys":{"on":true,"services":["Spooler"]}`) {
		t.Fatal("sys an:", cfg)
	}
	if code, b := agent("POST", "/api/agent/sys", map[string]any{"machine": "m1", "host": "SRV-DC01", "samples": []sysSample{{T: now, R: rep}}}, nil); code != 200 {
		t.Fatal(code, b)
	}
	// „nur Rechner“-Agent: keine Hosts, Standort bleibt offline
	c.do("POST", "/api/hosts", map[string]any{"name": "Router", "addr": "192.0.2.1", "site": site.ID})
	s.m.mu.Lock()
	delete(s.m.sites, site.ID)
	s.m.mu.Unlock()
	_, cfg := agent("GET", "/api/agent/config", nil, map[string]string{"X-Agent-Mode": "sysonly", "X-Machine": "m2"})
	if !strings.Contains(cfg, `"hosts":[]`) || !strings.Contains(cfg, `"sys":{"on":true`) {
		t.Fatal("sysonly-Konfig:", cfg)
	}
	if on, _ := s.m.siteOnline(site.ID); on {
		t.Fatal("sysonly-Agent hält den Standort online")
	}
	agent("POST", "/api/agent/sys", map[string]any{"machine": "m2", "host": "BUCHHALTUNG", "sysOnly": true, "samples": []sysSample{{T: now, R: rep}}}, nil)
	_, st := c.do("GET", "/api/state", nil)
	var state struct {
		Machines []machineView `json:"machines"`
		Sites    []SiteView    `json:"sites"`
	}
	json.Unmarshal([]byte(st), &state)
	if len(state.Machines) != 2 || state.Machines[0].Name != "BUCHHALTUNG" || !state.Machines[0].SysOnly || state.Machines[1].Status != "ok" || state.Machines[1].Disk != 60 {
		t.Fatalf("State: %s", st)
	}
	var siteSys *SysCfg
	for _, sv := range state.Sites {
		if sv.ID == site.ID {
			siteSys = sv.Sys
		}
	}
	if siteSys == nil || !siteSys.On || siteSys.DiskPct != 85 {
		t.Fatalf("Standort-Einstellungen im State: %+v", state.Sites)
	}
	id := state.Machines[1].ID
	_, det := c.do("GET", "/api/machine?id="+id, nil)
	if !strings.Contains(det, "Windows Server 2022") || !strings.Contains(det, `"hist":[`) || !strings.Contains(det, `"cfg":{"on":true`) {
		t.Fatal("Detail:", det)
	}
	// umbenennen und Gruppe zuordnen
	c.do("POST", "/api/groups", map[string]string{"name": "Kunde Maier"})
	_, gs := c.do("GET", "/api/state", nil)
	var gst struct {
		Groups []Group `json:"groups"`
	}
	json.Unmarshal([]byte(gs), &gst)
	if len(gst.Groups) != 1 {
		t.Fatal("Gruppe:", gs)
	}
	code, body = c.do("POST", "/api/machine", map[string]any{"id": id, "name": "DC Maier", "group": gst.Groups[0].ID})
	if code != 200 || !strings.Contains(body, "DC Maier") || !strings.Contains(body, gst.Groups[0].ID) {
		t.Fatal(code, body)
	}
	// Stummschalten der Gruppe erfasst den Rechner
	code, body = c.do("POST", "/api/mute", map[string]any{"scope": "group", "id": gst.Groups[0].ID, "mode": "mute", "until": -1})
	if code != 200 {
		t.Fatal(code, body)
	}
	mc, _ := s.m.machineByID(id)
	if !s.m.machineInMaint(mc, time.Now()) {
		t.Fatal("Gruppen-Stummschaltung wirkt nicht auf Rechner")
	}
	// fremder Rechner/unbekannt
	if code, _ := c.do("GET", "/api/machine?id=gibtsnicht", nil); code != 404 {
		t.Fatal("unbekannt:", code)
	}
	// entfernen
	if code, _ := c.do("DELETE", "/api/machine?id="+id, nil); code != 200 {
		t.Fatal("löschen")
	}
	if _, ok := s.m.machineByID(id); ok {
		t.Fatal("nicht gelöscht")
	}
	// agent.json für „nur Rechner“
	_, aj := c.do("GET", "/download/agent.json?site="+site.ID+"&mode=sysonly", nil)
	if !strings.Contains(aj, `"mode": "sysonly"`) {
		t.Fatal("agent.json sysonly:", aj)
	}
	_, aj = c.do("GET", "/download/agent.json?site="+site.ID, nil)
	if strings.Contains(aj, "sysonly") {
		t.Fatal("agent.json normal:", aj)
	}
}

func TestAgentSysOnly(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "services": []string{"cron"}}})
	c.do("POST", "/api/hosts", map[string]any{"name": "Router", "addr": "192.0.2.1", "site": site.ID})
	dir := t.TempDir()
	a := &Agent{file: AgentFile{Server: ts.URL, Token: site.Token, Mode: "sysonly"}, dataDir: dir, hosts: map[string]*agentHost{},
		refresh: make(chan struct{}, 1), stopAll: make(chan struct{}), notify: func(string, string, bool) {}, changed: func() {},
		client: &http.Client{Timeout: 5 * time.Second}, sysOnly: true, machine: sysMachineID(dir)}
	if !a.fetchConfig() {
		t.Fatal("Konfig")
	}
	a.mu.Lock()
	on, svcs, nHosts, name := a.sysOn, a.sysSvcs, len(a.hosts), a.siteName
	a.mu.Unlock()
	if !on || len(svcs) != 1 || nHosts != 0 || !strings.Contains(name, "nur Rechner") {
		t.Fatalf("sysonly: on=%v svcs=%v hosts=%d name=%q", on, svcs, nHosts, name)
	}
	if id2 := sysMachineID(dir); id2 != a.machine || len(a.machine) < 8 {
		t.Fatal("Rechner-ID nicht stabil")
	}
	// Messung auf diesem Rechner (Linux im Test)
	if runtime.GOOS == "linux" {
		col := newSysCollector()
		r := col.collect([]string{"gibt-es-sicher-nicht-123"})
		if r.CPU < 0 || r.CPU > 100 || r.MemTotal == 0 || r.Host == "" || r.Uptime <= 0 || len(r.Svcs) != 1 {
			t.Fatalf("Messung: %+v", r)
		}
	}
	// agent.json mit Modus wird gelesen
	p := filepath.Join(dir, "a.json")
	os.WriteFile(p, []byte(`{"server":"https://x","token":"t","mode":"sysonly"}`), 0o600)
	if af, _, err := loadAgentFile(p); err != nil || af.Mode != "sysonly" {
		t.Fatal("agent.json Modus", af, err)
	}
}

func TestMachineOwnServices(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "services": []string{"MSSQLSERVER", "Spooler"}}})
	mc, _ := s.m.registerMachine(site, "ws1", "WS1", true)
	if code, b := c.do("POST", "/api/machine", map[string]any{"id": mc.ID, "svcMode": "own", "services": []string{"Spooler", " "}}); code != 200 {
		t.Fatal(code, b)
	}
	get := func(mid string) []string {
		req, _ := http.NewRequest("GET", ts.URL+"/api/agent/config", nil)
		req.Header.Set("Authorization", "Bearer "+site.Token)
		req.Header.Set("X-Machine", mid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v struct {
			Sys struct{ Services []string } `json:"sys"`
		}
		json.NewDecoder(resp.Body).Decode(&v)
		return v.Sys.Services
	}
	if l := get("ws1"); len(l) != 1 || l[0] != "Spooler" {
		t.Fatalf("eigene Liste: %v", l)
	}
	if l := get("srv"); len(l) != 2 {
		t.Fatalf("Standort-Liste: %v", l)
	}
	c.do("POST", "/api/machine", map[string]any{"id": mc.ID, "svcMode": "site"})
	if l := get("ws1"); len(l) != 2 {
		t.Fatalf("zurück auf Standort: %v", l)
	}
}

func TestMachineNotes(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	mc, _ := s.m.registerMachine(site, "ws1", "WS1", true)
	long := strings.Repeat("ä", 5000) // 10000 Bytes
	if code, b := c.do("POST", "/api/machine", map[string]any{"id": mc.ID, "desc": "Empfang\nEG", "notes": long}); code != 200 {
		t.Fatal(code, b)
	}
	got, _ := s.m.machineByID(mc.ID)
	if got.Desc != "Empfang EG" || len(got.Notes) != 8000 || !utf8.ValidString(got.Notes) {
		t.Fatalf("desc %q notes %d", got.Desc, len(got.Notes))
	}
	// Name bleibt unverändert, wenn nur Notizen kommen
	if got.Name != mc.Name {
		t.Fatalf("Name geändert: %q", got.Name)
	}
}

func TestParseWho(t *testing.T) {
	if got := parseWho([]byte("anna     tty1  2026-10-22 08:01\nbert pts/0 2026-10-22 09:00 (10.0.0.5)\nanna pts/1 2026-10-22 09:10\n")); got != "anna, bert" {
		t.Fatal(got)
	}
}

func TestMachineRole(t *testing.T) {
	ws := &sysReport{OS: "Windows 11 Pro"}
	lin := &sysReport{OS: "Debian GNU/Linux 12"}
	if (MachineCfg{}).isServer(ws) || !(MachineCfg{}).isServer(lin) {
		t.Fatal("Automatik")
	}
	if !(MachineCfg{Role: "server"}).isServer(ws) || (MachineCfg{Role: "workstation"}).isServer(lin) {
		t.Fatal("feste Rolle")
	}
	if !(MachineCfg{Role: "server"}).wantsOffAlert(ws) || (MachineCfg{Role: "workstation"}).wantsOffAlert(lin) {
		t.Fatal("Aus-Meldung folgt der Rolle")
	}
	if (MachineCfg{Role: "server", OffAlert: "off"}).wantsOffAlert(ws) {
		t.Fatal("ausdrücklich aus hat Vorrang")
	}
}

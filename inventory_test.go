package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidServiceName(t *testing.T) {
	for _, ok := range []string{"Spooler", "MSSQL$SQLEXPRESS"[:5], "smbd", "getty@tty1", "Datev Sync", "my-svc_2.service"} {
		if !validServiceName(ok) {
			t.Errorf("sollte gültig sein: %q", ok)
		}
	}
	for _, bad := range []string{"", "a;rm -rf /", "x && y", "$(id)", "a|b", "-x", " lead", "trail ", "svc\nnext", "c:\\x", strings.Repeat("a", 121), "MSSQL$SQLEXPRESS"} {
		if validServiceName(bad) {
			t.Errorf("sollte ungültig sein: %q", bad)
		}
	}
	c := SysCfg{On: true, Services: []string{"Spooler", "smbd", "bad;name"}, SelfHeal: true, Restart: []string{"spooler", "gibtsnicht", "bad;name", "Spooler"}}
	c.norm()
	if len(c.Restart) != 1 || c.Restart[0] != "Spooler" {
		t.Fatalf("Restart-Liste: %v", c.Restart)
	}
}

func TestSelfHealer(t *testing.T) {
	var calls []string
	fail := true
	h := newSelfHealer(func(n string) error {
		calls = append(calls, n)
		if fail {
			return errors.New("Zugriff verweigert")
		}
		return nil
	})
	h.verify, h.wait = func(string) string { return "running" }, 0
	now := time.Now()
	svcs := func(st string) []sysSvc {
		return []sysSvc{{Name: "Spooler", State: st}, {Name: "Other", State: "stopped"}, {Name: "bad;x", State: "stopped"}}
	}
	restart := []string{"Spooler", "bad;x"}
	mon := []string{"Spooler", "Other", "bad;x"}
	// Versuch 1
	ev := h.step(svcs("stopped"), restart, mon, now)
	if len(ev) != 1 || ev[0].Svc != "Spooler" || ev[0].OK || ev[0].Try != 1 || ev[0].GaveUp {
		t.Fatalf("Versuch 1: %+v", ev)
	}
	// innerhalb einer Minute kein weiterer Versuch
	if ev := h.step(svcs("stopped"), restart, mon, now.Add(30*time.Second)); len(ev) != 0 {
		t.Fatalf("zu früh: %+v", ev)
	}
	h.step(svcs("stopped"), restart, mon, now.Add(61*time.Second))
	ev = h.step(svcs("stopped"), restart, mon, now.Add(122*time.Second))
	if len(ev) != 1 || ev[0].Try != 3 || !ev[0].GaveUp {
		t.Fatalf("Versuch 3: %+v", ev)
	}
	// aufgegeben: keine weiteren Versuche
	if ev := h.step(svcs("stopped"), restart, mon, now.Add(10*time.Minute)); len(ev) != 0 {
		t.Fatalf("nach Aufgeben: %+v", ev)
	}
	if len(calls) != 3 {
		t.Fatalf("Aufrufe: %v", calls)
	}
	for _, c := range calls {
		if c != "Spooler" {
			t.Fatalf("nicht freigegebener Dienst gestartet: %v", calls)
		}
	}
	// läuft wieder → Zähler zurück; danach erfolgreicher Neustart
	h.step(svcs("running"), restart, mon, now.Add(11*time.Minute))
	fail = false
	ev = h.step(svcs("stopped"), restart, mon, now.Add(12*time.Minute))
	if len(ev) != 1 || !ev[0].OK || ev[0].Try != 1 {
		t.Fatalf("Erfolg: %+v", ev)
	}
	// nur überwachte Dienste: steht nicht in der Dienstliste → nichts
	calls = nil
	h.reset()
	h.step([]sysSvc{{Name: "Spooler", State: "stopped"}}, []string{"Spooler"}, []string{"Other"}, now.Add(20*time.Minute))
	if len(calls) != 0 {
		t.Fatal("nicht überwachter Dienst gestartet")
	}
}

func TestSelfHealServer(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	m.notices = make(chan notice, 100)
	m.cfg.Orgs = append(m.cfg.Orgs, Org{ID: "o1", Plan: "pro"})
	site := m.UpsertSite(SiteCfg{Name: "Büro", Org: "o1", Sys: &SysCfg{On: true, Services: []string{"Spooler"}, SelfHeal: true, Restart: []string{"Spooler"}}})
	mc, _ := m.registerMachine(site, "h1", "SRV", true)
	cfg := site.Sys.eff()
	now := time.Now().UnixMilli()
	rep := func(h ...sysHeal) *sysReport {
		return &sysReport{Host: "SRV", CPU: 5, Svcs: []sysSvc{{Name: "Spooler", State: "stopped"}}, Heal: h, Reboot: -1, EvCrit: -1}
	}
	// erfolgreicher Neustart: Ereignis, keine Push-Meldung
	m.ingestSys(mc, cfg, []sysSample{{T: now - 30000, R: rep(sysHeal{Svc: "Spooler", T: now - 30000, Try: 1, OK: true})}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Erfolg darf nicht pushen: %+v", n)
	}
	// fehlgeschlagen, aufgegeben → Meldung
	m.ingestSys(mc, cfg, []sysSample{{T: now - 20000, R: rep(sysHeal{Svc: "Spooler", T: now - 20000, Try: 3, Err: "Zugriff verweigert", GaveUp: true})}})
	n := drainNotices(m)
	if len(n) != 1 || !strings.Contains(n[0].title, "Neustart fehlgeschlagen") || !strings.Contains(n[0].body, "Zugriff verweigert") {
		t.Fatalf("Fehlschlag: %+v", n)
	}
	cnt := 0
	m.evMu.Lock()
	for _, e := range m.events {
		if strings.Contains(e.HostName, "automatisch neu gestartet") || strings.Contains(e.HostName, "Neustart fehlgeschlagen") {
			cnt++
		}
	}
	m.evMu.Unlock()
	if cnt != 2 {
		t.Fatalf("Ereignisse: %d", cnt)
	}
	// Dienst-gestoppt-Meldung wartet bei Selbstheilung länger (5 min statt 2)
	m2, _ := m.registerMachine(site, "h2", "SRV2", true)
	m.ingestSys(m2, cfg, []sysSample{{T: now - 200000, R: rep()}, {T: now, R: rep()}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Dienst-Meldung zu früh trotz Selbstheilung: %+v", n)
	}
	// ohne Selbstheilung (Agent abgeschaltet) gilt wieder 2 min
	r2 := rep()
	r2.NoHeal = true
	m3, _ := m.registerMachine(site, "h3", "SRV3", true)
	m.ingestSys(m3, cfg, []sysSample{{T: now - 200000, R: r2}, {T: now, R: r2}})
	if n := drainNotices(m); len(n) != 1 {
		t.Fatalf("Dienst-Meldung ohne Selbstheilung: %+v", n)
	}
}

func TestInventoryDiffAndParse(t *testing.T) {
	old := &sysInventory{HW: invHW{Vendor: "Dell", Model: "T150", RAM: 16 << 30}, OS: invOS{Name: "Windows Server 2022", Build: "20348"},
		SW: []invSW{{Name: "7-Zip", Version: "22.01"}, {Name: "Chrome", Version: "129"}, {Name: "Alt", Version: "1"}}}
	cur := &sysInventory{HW: invHW{Vendor: "Dell", Model: "T150", RAM: 32 << 30}, OS: invOS{Name: "Windows Server 2022", Build: "20348"},
		SW: []invSW{{Name: "7-zip", Version: "22.01"}, {Name: "Chrome", Version: "130"}, {Name: "Neu", Version: "2"}}}
	ch := diffInventory(old, cur, 1)
	kinds := map[string]string{}
	for _, c := range ch {
		kinds[c.What] = c.Kind
	}
	if kinds["Chrome"] != "aktualisiert" || kinds["Neu"] != "neu" || kinds["Alt"] != "entfernt" || kinds["Arbeitsspeicher"] != "hardware" || len(ch) != 4 {
		t.Fatalf("Diff: %+v", ch)
	}
	if diffInventory(nil, cur, 1) != nil {
		t.Fatal("erste Inventur ohne Änderungen")
	}
	sw := parseDpkg([]byte("openssh-server\t1:9.2p1\tDebian OpenSSH Maintainers <debian-ssh@lists.debian.org>\nbash\t5.2\t\n\n"))
	if len(sw) != 2 || sw[0].Vendor != "Debian OpenSSH Maintainers" || sw[1].Name != "bash" {
		t.Fatalf("dpkg: %+v", sw)
	}
	rpm := parseRpm([]byte("bash\t5.1.8-9.el9\tRed Hat, Inc.\t1700000000\nfoo\t1-1\t(none)\t0\n"))
	if len(rpm) != 2 || rpm[0].Installed == "" || rpm[1].Vendor != "" {
		t.Fatalf("rpm: %+v", rpm)
	}
	win := `{"hw":{"vendor":"Dell Inc.","model":"OptiPlex 7010","serial":"ABC123","bios":"1.2","cpu":"Intel Core i5","cores":6,"threads":12,"ram":17179869184,"disks":{"m":"Samsung SSD","s":512110190592}},"os":{"name":"Microsoft Windows 11 Pro","version":"10.0.22631","build":"22631","installed":1700000000000,"boot":1720000000000,"domain":"WORKGROUP","partOfDomain":false,"user":"PC\\max","lic":1},"sw":[{"n":"7-Zip 23.01 (x64)","v":"23.01","p":"Igor Pavlov","d":"20240115"},{"n":"Chrome","v":"130","p":"Google","d":""}]}`
	inv, err := parseWinInventory([]byte("\ufeff" + win))
	if err != nil || inv.HW.Serial != "ABC123" || len(inv.HW.Disks) != 1 || inv.OS.Activation != "aktiviert" || len(inv.SW) != 2 || inv.SW[0].Installed != "2024-01-15" || inv.OS.User != `PC\max` {
		t.Fatalf("Windows-Inventur: %+v %v", inv, err)
	}
	mac := `{"SPHardwareDataType":[{"machine_name":"MacBook Air","machine_model":"Mac14,2","serial_number":"C02XYZ","chip_type":"Apple M2","number_processors":"proc 8:4:4","physical_memory":"16 GB"}],"SPApplicationsDataType":[{"_name":"Safari","version":"17.5","obtained_from":"apple","lastModified":"2024-06-01T10:00:00Z"}]}`
	mi := parseMacProfiler([]byte(mac))
	if mi.HW.Serial != "C02XYZ" || mi.HW.Cores != 8 || mi.HW.RAM != 16<<30 || len(mi.SW) != 1 || mi.SW[0].Installed != "2024-06-01" {
		t.Fatalf("Mac-Inventur: %+v", mi)
	}
}

func TestInventoryAPI(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "services": []string{"Spooler"}, "selfHeal": true, "restart": []string{"Spooler", "x;y"}}})
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
	if _, cfg := agent("GET", "/api/agent/config", nil, nil); !strings.Contains(cfg, `"selfHeal":true`) || !strings.Contains(cfg, `"restart":["Spooler"]`) {
		t.Fatal("Selbstheilung in der Agent-Konfig:", cfg)
	}
	now := time.Now().UnixMilli()
	rep := &sysReport{Host: "PC-1", CPU: 3, Reboot: -1, EvCrit: -1}
	agent("POST", "/api/agent/sys", map[string]any{"machine": "m1", "host": "PC-1", "sysOnly": true, "samples": []sysSample{{T: now, R: rep}}}, nil)
	inv := sysInventory{HW: invHW{Vendor: "Dell", Model: "OptiPlex", Serial: "SN1", RAM: 8 << 30}, OS: invOS{Name: "Windows 11 Pro", Build: "22631", Activation: "aktiviert"},
		SW: []invSW{{Name: "7-Zip", Version: "23.01", Vendor: "Igor Pavlov"}, {Name: "Chrome", Version: "129"}}}
	if code, b := agent("POST", "/api/agent/inventory", map[string]any{"machine": "m1", "inv": inv}, nil); code != 200 {
		t.Fatal(code, b)
	}
	if code, _ := agent("POST", "/api/agent/inventory", map[string]any{"machine": "unbekannt", "inv": inv}, nil); code != 404 {
		t.Fatal("unbekannter Rechner:", code)
	}
	inv.SW[1].Version = "130"
	inv.SW = append(inv.SW, invSW{Name: "TeamViewer", Version: "15"})
	agent("POST", "/api/agent/inventory", map[string]any{"machine": "m1", "inv": inv}, nil)
	_, st := c.do("GET", "/api/state", nil)
	var state struct {
		Machines []machineView `json:"machines"`
	}
	json.Unmarshal([]byte(st), &state)
	if len(state.Machines) != 1 {
		t.Fatal(st)
	}
	id := state.Machines[0].ID
	_, ib := c.do("GET", "/api/machine/inventory?id="+id, nil)
	var rec invRecord
	json.Unmarshal([]byte(ib), &rec)
	if rec.Inv == nil || rec.Inv.HW.Serial != "SN1" || len(rec.Inv.SW) != 3 || len(rec.Changes) != 2 {
		t.Fatalf("Inventar: %s", ib)
	}
	_, evs := c.do("GET", "/api/events?types=inv&host="+"m:"+id, nil)
	if !strings.Contains(evs, "Inventar geändert") {
		t.Fatal("Ereignis Inventar:", evs)
	}
	// CSV je Rechner und gesamt
	_, csv1 := c.do("GET", "/api/export/inventory.csv?id="+id, nil)
	if !strings.Contains(csv1, "TeamViewer") || !strings.Contains(csv1, "Seriennummer;SN1") {
		t.Fatal("CSV Rechner:", csv1)
	}
	_, csvAll := c.do("GET", "/api/export/inventory.csv", nil)
	if !strings.Contains(csvAll, "PC-1") || !strings.Contains(csvAll, "OptiPlex") || !strings.Contains(csvAll, ";3;") {
		t.Fatal("CSV gesamt:", csvAll)
	}
	_, csvSW := c.do("GET", "/api/export/inventory.csv?software=1", nil)
	if strings.Count(csvSW, "\n") != 4 {
		t.Fatal("CSV Software:", csvSW)
	}
	// Auftrag „inventory“ für einen „nur Rechner“-Agenten landet in dessen Warteschlange
	code, jb := c.do("POST", "/api/jobs", map[string]any{"kind": "inventory", "params": map[string]string{"machine": id}})
	if code != 200 {
		t.Fatal(code, jb)
	}
	if _, cfg := agent("GET", "/api/agent/config", nil, nil); strings.Contains(cfg, `"inventory"`) {
		t.Fatal("Standort-Agent bekommt den Auftrag eines anderen Rechners")
	}
	if _, cfg := agent("GET", "/api/agent/config", nil, map[string]string{"X-Agent-Mode": "sysonly", "X-Machine": "m1"}); !strings.Contains(cfg, `"kind":"inventory"`) {
		t.Fatal("sysonly-Agent bekommt den Auftrag nicht:", cfg)
	}
	// Rechner entfernen löscht auch das Inventar
	c.do("DELETE", "/api/machine?id="+id, nil)
	if code, _ := c.do("GET", "/api/machine/inventory?id="+id, nil); code != 404 {
		t.Fatal("Inventar nach Löschen")
	}
}

func TestAgentNoSelfHeal(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Büro"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "services": []string{"cron"}, "selfHeal": true, "restart": []string{"cron"}}})
	mk := func(no bool) *Agent {
		dir := t.TempDir()
		return &Agent{file: AgentFile{Server: ts.URL, Token: site.Token, NoSelfHeal: no}, dataDir: dir, hosts: map[string]*agentHost{},
			refresh: make(chan struct{}, 1), stopAll: make(chan struct{}), notify: func(string, string, bool) {}, changed: func() {},
			client: &http.Client{Timeout: 5 * time.Second}, machine: sysMachineID(dir)}
	}
	a := mk(false)
	a.fetchConfig()
	if !a.sysHeal || len(a.sysRestart) != 1 {
		t.Fatalf("Selbstheilung an: %v %v", a.sysHeal, a.sysRestart)
	}
	b := mk(true)
	b.fetchConfig()
	if b.sysHeal {
		t.Fatal("agent.json noSelfHeal wird ignoriert")
	}
	// Standort-Schalter aus → auch im Agent aus
	c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "services": []string{"cron"}, "selfHeal": false, "restart": []string{"cron"}}})
	a.fetchConfig()
	if a.sysHeal {
		t.Fatal("Standort-Schalter aus wird ignoriert")
	}
}

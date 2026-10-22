package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rawGet(t *testing.T, c *client, path string) (int, []byte, http.Header) {
	req, _ := http.NewRequest("GET", c.ts.URL+path, nil)
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func rawPost(t *testing.T, c *client, path string, body []byte) (int, string) {
	req, _ := http.NewRequest("POST", c.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func resetFrozen(t *testing.T) {
	t.Cleanup(func() { storeFrozen.Store(false) })
}

func TestBackupRestoreRoundtripFiles(t *testing.T) {
	resetFrozen(t)
	t.Setenv("BACKUP_KEY", "")
	restarted := make(chan bool, 1)
	old := restartFn
	restartFn = func() { restarted <- true }
	t.Cleanup(func() { restartFn = old })

	s, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")
	if code, body := admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde A", "plan": "pro", "ownerEmail": "a@kunde-a.de"}); code != 200 {
		t.Fatal(code, body)
	}
	_, body := admin.do("POST", "/api/sites", map[string]string{"name": "Zuhause"})
	var site SiteCfg
	mustJSON(t, body, &site)
	code, body := admin.do("POST", "/api/hosts", map[string]any{"name": "Router", "addr": "192.168.1.1", "site": site.ID})
	if code != 200 {
		t.Fatal(code, body)
	}
	var host HostCfg
	mustJSON(t, body, &host)
	fsStore := s.m.store.(*FileStore)
	now := time.Now().Add(-time.Hour).UnixMilli()
	for i := 0; i < 30; i++ {
		fsStore.Append(host.ID, Sample{T: now + int64(i)*1000, R: 123, F: 0})
	}
	fsStore.Flush()
	fsStore.AppendEvent(Event{ID: 4711, HostID: host.ID, HostName: "Router", Type: "outage", Start: now, End: now + 5000})
	s.m.mu.Lock()
	s.m.cfg.Machines = append(s.m.cfg.Machines, MachineCfg{ID: site.ID + ":pc1", Site: site.ID, Org: defaultOrgID, Name: "PC1"})
	s.m.saveConfigLocked()
	s.m.mu.Unlock()
	fsStore.SaveBlob(invKey(site.ID+":pc1"), []byte(`{"hw":{"model":"X1"}}`))

	code, data, hdr := rawGet(t, admin, "/api/admin/backup")
	if code != 200 || len(data) < 50 {
		t.Fatalf("Sicherung: %d %d", code, len(data))
	}
	if !strings.Contains(hdr.Get("Content-Disposition"), "pingwacht-backup-") || hdr.Get("X-Sha256") == "" {
		t.Fatalf("Header: %v", hdr)
	}

	// Vorschau
	code, body = rawPost(t, admin, "/api/admin/restore?dryRun=1", data)
	if code != 200 {
		t.Fatal(code, body)
	}
	var dr struct{ Summary backupSummary }
	mustJSON(t, body, &dr)
	if dr.Summary.Orgs != 2 || dr.Summary.Hosts != 1 || dr.Summary.Events < 1 || dr.Summary.Machines != 1 || dr.Summary.Store != "files" || dr.Summary.Counts["file"] < 1 || dr.Summary.Counts["blob"] != 1 {
		t.Fatalf("Vorschau: %+v", dr.Summary)
	}
	if storeFrozen.Load() {
		t.Fatal("Vorschau darf nichts einfrieren")
	}

	// Stand ändern, dann ohne Bestätigung → abgelehnt
	admin.do("POST", "/api/hosts", map[string]any{"name": "Neu", "addr": "10.0.0.1", "site": "cloud"})
	if code, _ := rawPost(t, admin, "/api/admin/restore", data); code != 400 {
		t.Fatalf("ohne Bestätigung: %d", code)
	}
	// kaputte Datei → nichts geändert, nicht eingefroren
	if code, _ := rawPost(t, admin, "/api/admin/restore?confirm=WIEDERHERSTELLEN", data[:len(data)/2]); code != 400 || storeFrozen.Load() {
		t.Fatalf("kaputte Datei: %d frozen=%v", code, storeFrozen.Load())
	}

	code, body = rawPost(t, admin, "/api/admin/restore?confirm=WIEDERHERSTELLEN", data)
	if code != 200 || !strings.Contains(body, `"restart":true`) {
		t.Fatal(code, body)
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("kein Neustart")
	}
	if !storeFrozen.Load() {
		t.Fatal("Speicher nicht eingefroren")
	}
	// laufender Prozess darf nichts mehr überschreiben
	s.m.mu.Lock()
	s.m.saveConfigLocked()
	s.m.mu.Unlock()

	// Neustart simulieren: neuer Monitor auf demselben Ordner
	storeFrozen.Store(false)
	m2 := NewMonitor(NewFileStore(fsStore.dir))
	if len(m2.cfg.Hosts) != 1 || m2.cfg.Hosts[0].Name != "Router" || len(m2.cfg.Orgs) != 2 || len(m2.cfg.Machines) != 1 {
		t.Fatalf("Konfiguration nach Wiederherstellung: %d Hosts, %d Orgs", len(m2.cfg.Hosts), len(m2.cfg.Orgs))
	}
	if n := len(m2.store.ReadDay(host.ID, dayOf(now))); n != 30 {
		t.Fatalf("Messwerte: %d", n)
	}
	found := false
	for _, e := range m2.store.LoadEvents(0) {
		if e.ID == 4711 {
			found = true
		}
	}
	if !found {
		t.Fatal("Ereignis fehlt")
	}
	if b, err := m2.store.(*FileStore).LoadBlob(invKey(site.ID + ":pc1")); err != nil || !strings.Contains(string(b), "X1") {
		t.Fatalf("Inventar: %v %s", err, b)
	}
	olds, _ := filepath.Glob(filepath.Join(fsStore.dir, ".restore-old-*"))
	if len(olds) != 1 {
		t.Fatalf("alter Stand nicht beiseitegelegt: %v", olds)
	}
}

func TestBackupEncryption(t *testing.T) {
	resetFrozen(t)
	s, _ := newTestServer(t)
	t.Setenv("BACKUP_KEY", "geheim-1234")
	var buf bytes.Buffer
	if _, err := s.m.writeBackup(&buf); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if !bytes.HasPrefix(data, []byte(backupMagic)) {
		t.Fatal("nicht verschlüsselt")
	}
	if bytes.Contains(data, []byte("passHash")) {
		t.Fatal("Klartext in verschlüsselter Sicherung")
	}
	sum, err := scanBackup(bytes.NewReader(data), nil)
	if err != nil || !sum.Enc {
		t.Fatalf("entschlüsseln: %v", err)
	}
	t.Setenv("BACKUP_KEY", "falsch")
	if _, err := scanBackup(bytes.NewReader(data), nil); !errors.Is(err, errBackupKey) {
		t.Fatalf("falscher Schlüssel: %v", err)
	}
	t.Setenv("BACKUP_KEY", "")
	if _, err := scanBackup(bytes.NewReader(data), nil); !errors.Is(err, errBackupKey) {
		t.Fatalf("ohne Schlüssel: %v", err)
	}
	t.Setenv("BACKUP_KEY", "geheim-1234")
	bad := append([]byte(nil), data...)
	bad[len(bad)-10] ^= 0xff
	if _, err := scanBackup(bytes.NewReader(bad), nil); err == nil {
		t.Fatal("manipulierte Datei angenommen")
	}
	if _, err := scanBackup(bytes.NewReader(data[:len(data)-30]), nil); err == nil {
		t.Fatal("abgeschnittene Datei angenommen")
	}
	// unverschlüsselt: abgeschnitten / leer / Fremdformat
	t.Setenv("BACKUP_KEY", "")
	buf.Reset()
	s.m.writeBackup(&buf)
	plain := buf.Bytes()
	if _, err := scanBackup(bytes.NewReader(plain[:len(plain)-8]), nil); err == nil {
		t.Fatal("abgeschnittene gzip-Datei angenommen")
	}
	if _, err := scanBackup(bytes.NewReader(nil), nil); err == nil {
		t.Fatal("leere Datei angenommen")
	}
	if _, err := scanBackup(strings.NewReader("hallo welt"), nil); err == nil {
		t.Fatal("Fremdformat angenommen")
	}
}

func TestBackupRights(t *testing.T) {
	resetFrozen(t)
	_, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")
	var a struct{ ID, TempPassword string }
	_, body := admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde A", "plan": "pro", "ownerEmail": "a@kunde-a.de"})
	mustJSON(t, body, &a)
	ca := newClient(t, ts)
	ca.login("a@kunde-a.de", a.TempPassword)
	if code, _, _ := rawGet(t, ca, "/api/admin/backup"); code != http.StatusForbidden {
		t.Fatalf("Kunde lädt Sicherung: %d", code)
	}
	if code, _ := rawPost(t, ca, "/api/admin/restore?dryRun=1", []byte("x")); code != http.StatusForbidden {
		t.Fatalf("Kunde stellt wieder her: %d", code)
	}
	if code, _ := ca.do("GET", "/api/admin/backup/targets", nil); code != http.StatusForbidden {
		t.Fatalf("Kunde sieht Ziele: %d", code)
	}
}

func TestAgentBackupEndpointAndTargets(t *testing.T) {
	resetFrozen(t)
	t.Setenv("BACKUP_KEY", "")
	s, ts := newTestServer(t)
	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")
	_, body := admin.do("POST", "/api/sites", map[string]string{"name": "Zuhause"})
	var site SiteCfg
	mustJSON(t, body, &site)
	ag := newClient(t, ts)
	ag.tok = site.Token

	if code, _, _ := rawGet(t, ag, "/api/agent/backup"); code != http.StatusForbidden {
		t.Fatalf("ohne Freigabe: %d", code)
	}
	bad := newClient(t, ts)
	bad.tok = "falsch"
	if code, _, _ := rawGet(t, bad, "/api/agent/backup"); code != http.StatusUnauthorized {
		t.Fatalf("falsches Token: %d", code)
	}
	if code, body := admin.do("POST", "/api/admin/backup/targets", map[string]any{"site": site.ID, "on": true, "keep": 3}); code != 200 {
		t.Fatal(code, body)
	}
	_, body = ag.do("GET", "/api/agent/config", nil)
	if !strings.Contains(body, `"backup":{"keep":3,"on":true}`) {
		t.Fatalf("Agent-Konfiguration: %s", body)
	}
	code, data, hdr := rawGet(t, ag, "/api/agent/backup")
	if code != 200 || hdr.Get("X-Sha256") == "" || len(data) < 50 {
		t.Fatalf("Agent-Sicherung: %d", code)
	}
	if code, _ := ag.do("POST", "/api/agent/backup", map[string]any{"ok": true, "size": len(data), "file": "pingwacht-backup-x.json.gz", "count": 2}); code != 200 {
		t.Fatal(code)
	}
	_, body = admin.do("GET", "/api/admin/backup/targets", nil)
	var tv struct {
		Targets []backupTargetView
	}
	mustJSON(t, body, &tv)
	if len(tv.Targets) != 1 || !tv.Targets[0].On || tv.Targets[0].Keep != 3 || tv.Targets[0].Status == nil || !tv.Targets[0].Status.OK || tv.Targets[0].Status.Count != 2 {
		t.Fatalf("Ziele: %s", body)
	}
	// Fehlermeldung behält letzte erfolgreiche Sicherung
	ag.do("POST", "/api/agent/backup", map[string]any{"ok": false, "err": "Platte voll"})
	_, body = admin.do("GET", "/api/admin/backup/targets", nil)
	mustJSON(t, body, &tv)
	if st := tv.Targets[0].Status; st.OK || st.LastOK == 0 || st.Err != "Platte voll" || st.Count != 2 {
		t.Fatalf("Fehlerstatus: %+v", st)
	}

	// Standorte eines Kunden sind kein Sicherungsziel
	var a struct{ ID, TempPassword string }
	_, body = admin.do("POST", "/api/admin/orgs", map[string]any{"name": "Kunde A", "plan": "pro", "ownerEmail": "a@kunde-a.de"})
	mustJSON(t, body, &a)
	ca := newClient(t, ts)
	ca.login("a@kunde-a.de", a.TempPassword)
	_, body = ca.do("POST", "/api/sites", map[string]string{"name": "Kundenstandort"})
	var csite SiteCfg
	mustJSON(t, body, &csite)
	if code, _ := admin.do("POST", "/api/admin/backup/targets", map[string]any{"site": csite.ID, "on": true}); code != 400 {
		t.Fatalf("Kundenstandort als Ziel: %d", code)
	}
	s.m.mu.Lock()
	if s.m.cfg.BackupTargets == nil {
		s.m.cfg.BackupTargets = map[string]*BackupCfg{}
	}
	s.m.cfg.BackupTargets[csite.ID] = &BackupCfg{On: true} // selbst wenn es so in der Konfiguration stünde
	s.m.mu.Unlock()
	cag := newClient(t, ts)
	cag.tok = csite.Token
	if code, _, _ := rawGet(t, cag, "/api/agent/backup"); code != http.StatusForbidden {
		t.Fatalf("Kunden-Agent lädt Sicherung: %d", code)
	}
}

func TestAgentRunBackupAndRotation(t *testing.T) {
	payload := []byte(strings.Repeat("x", 200))
	sum := ""
	{
		h := sha256Hex(payload)
		sum = h
	}
	wrong := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="pingwacht-backup-2030-01-01-0300.json.gz"`)
		if wrong {
			w.Header().Set("X-Sha256", strings.Repeat("0", 64))
		} else {
			w.Header().Set("X-Sha256", sum)
		}
		w.Write(payload)
	}))
	defer srv.Close()
	dir := t.TempDir()
	a := &Agent{file: AgentFile{Server: srv.URL, Token: "tok", BackupDir: filepath.Join(dir, "b")}, dataDir: dir, client: &http.Client{Timeout: 5 * time.Second}}
	os.MkdirAll(a.backupDir(), 0o700)
	for _, n := range []string{"pingwacht-backup-2020-01-01-0300.json.gz", "pingwacht-backup-2020-01-02-0300.json.gz", "pingwacht-backup-2020-01-03-0300.json.gz", "andere-datei.txt"} {
		os.WriteFile(filepath.Join(a.backupDir(), n), []byte("alt"), 0o600)
	}
	name, size, count, err := a.runBackup(2)
	if err != nil || name != "pingwacht-backup-2030-01-01-0300.json.gz" || size != 200 || count != 2 {
		t.Fatalf("runBackup: %v %s %d %d", err, name, size, count)
	}
	files := listBackups(a.backupDir())
	if len(files) != 2 || files[1] != name || files[0] != "pingwacht-backup-2020-01-03-0300.json.gz" {
		t.Fatalf("Rotation: %v", files)
	}
	if _, err := os.Stat(filepath.Join(a.backupDir(), "andere-datei.txt")); err != nil {
		t.Fatal("fremde Datei gelöscht")
	}
	// falsche Prüfsumme → nichts abgelegt
	wrong = true
	os.Remove(filepath.Join(a.backupDir(), name))
	if _, _, _, err := a.runBackup(2); err == nil {
		t.Fatal("falsche Prüfsumme angenommen")
	}
	if fs := listBackups(a.backupDir()); len(fs) != 1 {
		t.Fatalf("nach Prüfsummenfehler: %v", fs)
	}
	tmps, _ := filepath.Glob(filepath.Join(a.backupDir(), "*.tmp"))
	if len(tmps) != 0 {
		t.Fatalf("temporäre Datei liegen geblieben: %v", tmps)
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestBackupDue(t *testing.T) {
	loc := time.Local
	day := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	cases := []struct {
		now, last time.Time
		want      bool
	}{
		{day(22, 3, 10), time.Time{}, true},     // noch nie
		{day(22, 12, 0), day(20, 12, 0), true},  // älter als 24 h
		{day(22, 12, 0), day(22, 3, 5), false},  // heute schon
		{day(23, 3, 10), day(22, 3, 5), true},   // neuer Tag, 3-Uhr-Fenster
		{day(23, 3, 40), day(23, 3, 10), false}, // heute im Fenster schon erledigt
		{day(23, 2, 50), day(22, 3, 5), false},  // vor dem Fenster, < 24 h
		{day(23, 3, 10), day(23, 1, 0), true},   // nachts vor 3 Uhr gesichert → im Fenster nochmal
		{day(23, 4, 10), day(22, 12, 0), false}, // nach dem Fenster, < 24 h
	}
	for i, c := range cases {
		if got := backupDue(c.now, c.last); got != c.want {
			t.Errorf("Fall %d: %v, erwartet %v", i, got, c.want)
		}
	}
}

func TestBackupOverdueAlert(t *testing.T) {
	resetFrozen(t)
	s, ts := newTestServer(t)
	mails := 0
	old := backupOpsMail
	backupOpsMail = func(string, string) { mails++ }
	t.Cleanup(func() { backupOpsMail = old })
	admin := newClient(t, ts)
	admin.login("", "admin-pass-123")
	_, body := admin.do("POST", "/api/sites", map[string]string{"name": "Zuhause"})
	var site SiteCfg
	mustJSON(t, body, &site)
	admin.do("POST", "/api/admin/backup/targets", map[string]any{"site": site.ID, "on": true})
	drain := func() []notice {
		var out []notice
		for {
			select {
			case n := <-s.m.notices:
				if n.kind == "backup" {
					out = append(out, n)
				}
			default:
				return out
			}
		}
	}
	drain()
	now := time.Now()
	s.m.checkBackups(now)
	if n := drain(); len(n) != 0 {
		t.Fatalf("zu früh gemeldet: %v", n)
	}
	later := now.Add(37 * time.Hour)
	s.m.checkBackups(later)
	n := drain()
	if len(n) != 1 || !strings.Contains(n[0].title, "Sicherung ausgeblieben") || n[0].org != defaultOrgID || mails != 1 {
		t.Fatalf("Meldung: %+v mails=%d", n, mails)
	}
	s.m.checkBackups(later.Add(time.Hour))
	if n := drain(); len(n) != 0 {
		t.Fatal("doppelt gemeldet")
	}
	s.m.setBackupStatus(site.ID, BackupStatus{At: later.UnixMilli(), OK: true, LastOK: later.UnixMilli()})
	s.m.checkBackups(later.Add(2 * time.Hour))
	n = drain()
	if len(n) != 1 || !strings.Contains(n[0].title, "wieder erfolgreich") {
		t.Fatalf("Entwarnung: %+v", n)
	}
	b, _ := json.Marshal(s.m.cfg.BackupStatus)
	if strings.Contains(string(b), `"alerted":true`) {
		t.Fatal("Alarm nicht zurückgesetzt")
	}
}

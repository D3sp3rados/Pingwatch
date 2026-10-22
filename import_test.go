package main

import (
	"strings"
	"testing"
)

func TestImport(t *testing.T) {
	_, ts := newTestServer(t)
	a := newClient(t, ts)
	a.login("admin", "admin-pass-123")
	rows := []map[string]any{
		{"name": "Antenne MKT", "addr": "185.51.186.213", "type": "PING", "group": "Oli Pfeiffer", "paused": true},
		{"name": "rst.de", "addr": "https://www.rst.de", "type": "CONT", "expect": "Hochrhein", "group": "RST"},
		{"name": "imapx", "addr": "imapx.rst.de", "type": "SCKT", "port": "143", "channel": "Gibtsnicht"},
		{"name": "kaputt", "addr": "x", "type": "SCKT"},
	}
	code, body := a.do("POST", "/api/import", map[string]any{"rows": rows, "dryRun": true})
	if code != 200 || !strings.Contains(body, `"new":3`) || !strings.Contains(body, "Port fehlt") {
		t.Fatal(code, body)
	}
	_, st := a.do("GET", "/api/hosts", nil)
	if strings.Contains(st, "Antenne") {
		t.Fatal("Probelauf darf nichts anlegen")
	}
	code, body = a.do("POST", "/api/import", map[string]any{"rows": rows})
	if code != 200 || !strings.Contains(body, `"new":3`) || !strings.Contains(body, "nicht gefunden") {
		t.Fatal(code, body)
	}
	_, st = a.do("GET", "/api/hosts", nil)
	if !strings.Contains(st, "Antenne MKT") || !strings.Contains(st, `"expect":"Hochrhein"`) || !strings.Contains(st, `"port":143`) {
		t.Fatal(st)
	}
	_, g := a.do("GET", "/api/groups", nil)
	if !strings.Contains(g, "Oli Pfeiffer") {
		t.Fatal("Gruppe fehlt", g)
	}
	_, body = a.do("POST", "/api/import", map[string]any{"rows": rows[:1]})
	if !strings.Contains(body, "vorhanden") {
		t.Fatal("doppelt", body)
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestMikrotikActions(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		if u != "act" || p != "pw" {
			w.WriteHeader(401)
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+mtStr(b, "numbers"))
		mu.Unlock()
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	i := strings.LastIndex(addr, ":")
	port, _ := strconv.Atoi(addr[i+1:])
	hc := HostCfg{Addr: addr[:i], Mt: true, MtPort: port, MtActUser: "act", MtActPass: "pw", MtActPorts: "ether5, ether6", MtActReboot: true}
	if !mtActPortOK(hc, "ether6") || mtActPortOK(hc, "sfp-sfpplus1") {
		t.Fatal("Port-Freigabe")
	}
	if _, err := mtRunAction(hc, "poe", "ether5"); err != nil {
		t.Fatal(err)
	}
	if _, err := mtRunAction(hc, "reboot", ""); err != nil {
		t.Fatal(err)
	}
	if calls[0] != "POST /rest/interface/ethernet/poe/power-cycle ether5" || calls[1] != "POST /rest/system/reboot " {
		t.Fatalf("Aufrufe: %v", calls)
	}
	// Server: nicht freigegebener Port wird abgelehnt
	sv, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("admin", "admin-pass-123")
	sv.m.mu.Lock()
	sv.m.cfg.Sites = append(sv.m.cfg.Sites, SiteCfg{ID: "s1", Name: "Büro", Org: "default"})
	sv.m.mu.Unlock()
	h := sv.m.UpsertHost(HostCfg{Name: "FW", Addr: "10.0.0.1", Site: "s1", Org: "default", Mt: true, MtUser: "r", MtPass: "r", MtActUser: "act", MtActPass: "pw", MtActPorts: "ether5"})
	code, body := c.do("POST", "/api/mikrotik/action", map[string]string{"host": h.ID, "action": "bounce", "port": "sfp-sfpplus1"})
	if code != 403 {
		t.Fatalf("erwartet 403, %d %s", code, body)
	}
	code, _ = c.do("POST", "/api/mikrotik/action", map[string]string{"host": h.ID, "action": "reboot"})
	if code != 403 {
		t.Fatalf("Neustart nicht freigegeben: %d", code)
	}
	if publicHost(h).MtActPass != "" {
		t.Fatal("Passwort im Browser")
	}
}

package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFritzRunActions(t *testing.T) {
	seen := map[string]string{}
	f := &fakeFritz{user: "pw", pass: "geheim", conn: "Connected"}
	f.more = func(act, body string) map[string]string {
		a := act[strings.Index(act, "#")+1:]
		switch a {
		case "Reboot", "ForceTermination", "SetEnable", "X_AVM-DE_WakeOnLANByMACAddress", "X_AVM-DE_DoUpdate", "SetDeflectionEnable":
			seen[a] = act + "|" + body
			return map[string]string{"ok": "1"}
		case "GetInfo":
			if strings.Contains(act, "UserInterface") {
				return map[string]string{"NewUpgradeAvailable": "1"}
			}
		}
		return nil
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := newTR064(srv.URL, "pw", "geheim")
	cases := []struct {
		act  string
		p    map[string]string
		key  string
		want string
	}{
		{"reboot", nil, "Reboot", "DeviceConfig:1"},
		{"reconnect", nil, "ForceTermination", "WANPPPConnection"},
		{"wlan", map[string]string{"idx": "3", "on": "0"}, "SetEnable", "<NewEnable>0</NewEnable>"},
		{"wol", map[string]string{"mac": "aa-bb-cc-dd-ee-ff"}, "X_AVM-DE_WakeOnLANByMACAddress", "AA:BB:CC:DD:EE:FF"},
		{"update", nil, "X_AVM-DE_DoUpdate", "UserInterface:1"},
		{"defl", map[string]string{"id": "2", "on": "1"}, "SetDeflectionEnable", "<NewDeflectionId>2</NewDeflectionId>"},
	}
	for _, k := range cases {
		if k.p == nil {
			k.p = map[string]string{}
		}
		if err := fritzActionCheck(k.act, k.p); err != nil {
			t.Fatal(k.act, err)
		}
		msg, err := fritzRunAction(c, k.act, k.p)
		if err != nil || msg == "" {
			t.Fatal(k.act, msg, err)
		}
		if !strings.Contains(seen[k.key], k.want) {
			t.Fatalf("%s: %q", k.act, seen[k.key])
		}
	}
	if !strings.Contains(seen["SetEnable"], "WLANConfiguration:3") {
		t.Fatal("falsches WLAN:", seen["SetEnable"])
	}
	for _, bad := range []struct {
		a string
		p map[string]string
	}{{"wlan", map[string]string{"idx": "9", "on": "1"}}, {"wol", map[string]string{"mac": "x; reboot"}}, {"defl", map[string]string{"id": "a", "on": "1"}}, {"factoryreset", map[string]string{}}, {"wlan", map[string]string{"idx": "1"}}} {
		if fritzActionCheck(bad.a, bad.p) == nil {
			t.Fatal("ungültig angenommen:", bad)
		}
	}
}

func TestFritzActionAPI(t *testing.T) {
	sv, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("admin", "admin-pass-123")
	sv.m.mu.Lock()
	sv.m.cfg.Sites = append(sv.m.cfg.Sites, SiteCfg{ID: "s1", Name: "Büro", Org: "default"})
	sv.m.mu.Unlock()
	h := sv.m.UpsertHost(HostCfg{Name: "FB", Addr: "192.168.178.1", Site: "s1", Org: "default", Fritz: true, FritzUser: "u", FritzPass: "p"})
	child := sv.m.UpsertHost(HostCfg{Name: "NAS", Addr: "192.168.178.20", Site: "s1", Org: "default", Parent: h.ID})
	if code, _ := c.do("POST", "/api/fritz/action", map[string]string{"host": h.ID, "action": "reboot"}); code != 403 {
		t.Fatal("ohne Freigabe erlaubt:", code)
	}
	cfg := h
	cfg.FritzAct = true
	sv.m.UpsertHost(cfg)
	if code, _ := c.do("POST", "/api/fritz/action", map[string]string{"host": h.ID, "action": "wol", "mac": "kaputt"}); code != 400 {
		t.Fatal("ungültige MAC:", code)
	}
	// Agent offline → 409
	if code, _ := c.do("POST", "/api/fritz/action", map[string]string{"host": h.ID, "action": "reboot"}); code != 409 {
		t.Fatal("Agent offline:", code)
	}
	agentSeen(sv, "s1")
	code, body := c.do("POST", "/api/fritz/action", map[string]string{"host": h.ID, "action": "reboot"})
	if code != 200 || !strings.Contains(body, `"muteMin":8`) {
		t.Fatal(code, body)
	}
	now := time.Now()
	if !sv.m.inMaint("default", h.ID, now) || !sv.m.inMaint("default", child.ID, now) || !sv.m.inMaintSite("default", "s1", now) {
		t.Fatal("Box, abhängiger Host und Standort nicht stumm")
	}
	jobs.Lock()
	q := jobs.queue["s1"]
	jobs.Unlock()
	if len(q) == 0 || q[len(q)-1].Kind != "fritzaction" || q[len(q)-1].Params["action"] != "reboot" {
		t.Fatal("kein Auftrag:", q)
	}
	// WLAN schalten: keine Stummschaltung
	n := len(sv.m.cfg.Maints)
	if code, _ := c.do("POST", "/api/fritz/action", map[string]string{"host": h.ID, "action": "wlan", "idx": "1", "on": "0"}); code != 200 || len(sv.m.cfg.Maints) != n {
		t.Fatal("WLAN:", code)
	}
}

func agentSeen(sv *Server, site string) {
	sv.m.mu.Lock()
	st := sv.m.sites[site]
	if st == nil {
		st = &siteState{}
		sv.m.sites[site] = st
	}
	sv.m.mu.Unlock()
	st.mu.Lock()
	st.lastSeen = time.Now().UnixMilli()
	st.mu.Unlock()
}

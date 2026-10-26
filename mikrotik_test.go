package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeRouterOS(t *testing.T, uptime *string, psu *string, ppp *int) *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		if u != "pw" || p != "geheim" {
			w.WriteHeader(401)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/rest")
		var out any
		switch path {
		case "/system/resource":
			out = map[string]any{"uptime": *uptime, "version": "7.16.1 (stable)", "cpu-load": "12", "free-memory": "500", "total-memory": "1000", "board-name": "CCR2116-12G-4S+"}
		case "/system/routerboard":
			out = map[string]any{"model": "CCR2116-12G-4S+", "current-firmware": "7.16.1", "upgrade-firmware": "7.16.1"}
		case "/system/identity":
			out = map[string]any{"name": "Leibstadt"}
		case "/system/health":
			out = []map[string]any{{"name": "cpu-temperature", "value": "52", "type": "C"}, {"name": "psu1-state", "value": *psu}, {"name": "psu2-state", "value": "ok"}}
		case "/interface":
			out = []map[string]any{
				{".id": "*1", "name": "sfp-sfpplus1", "type": "ether", "running": "true", "rx-byte": "1000", "tx-byte": "2000", "comment": "Uplink"},
				{".id": "*2", "name": "ether1", "type": "ether", "running": "false", "rx-byte": "0", "tx-byte": "0"},
				{".id": "*A", "name": "bridge", "type": "bridge", "running": "true", "rx-byte": "5", "tx-byte": "5"},
			}
		case "/interface/ethernet/monitor":
			out = []map[string]any{{"name": "sfp-sfpplus1", "rate": "10Gbps", "sfp-rx-power": "-7.5", "sfp-tx-power": "-2.1", "sfp-temperature": "40"}}
		case "/routing/bgp/session":
			out = []map[string]any{{"name": "upstream-1", "remote.address": "1.2.3.4", "remote.as": "3320", "established": "true", "uptime": "1w2d03:04:05", "prefix-count": "950000"}}
		case "/ppp/active":
			l := []map[string]any{}
			for i := 0; i < *ppp; i++ {
				l = append(l, map[string]any{".id": "*1"})
			}
			out = l
		case "/ip/firewall/connection/tracking":
			out = map[string]any{"total-entries": "12345"}
		case "/log":
			out = []map[string]any{{".id": "*10", "time": "12:00:01", "topics": "system,info", "message": "address changed by pw"}, {".id": "*11", "time": "12:00:02", "topics": "dhcp,info", "message": "ok"}}
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":404,"message":"Not Found"}`))
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestMikrotikQueryAndIngest(t *testing.T) {
	up, psu, ppp := "2d00:00:00", "ok", 100
	srv := fakeRouterOS(t, &up, &psu, &ppp)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	host, port := addr[:strings.LastIndex(addr, ":")], addr[strings.LastIndex(addr, ":")+1:]
	var pn int
	for _, ch := range port {
		pn = pn*10 + int(ch-'0')
	}
	c := newMtClient(host, pn, "pw", "geheim")
	st := &mtAgentState{}
	s := mtQuery(c, st)
	if s.Err != "" || s.Res["identity"] != "Leibstadt" || s.PPP != 100 || s.Conns != 12345 || len(s.BGP) != 1 || s.BGP[0].Uptime != 9*86400+3*3600+4*60+5 {
		t.Fatalf("Abfrage: %+v", s)
	}
	if len(s.Ifs) != 3 || s.Ifs[0].Speed != 10000 || s.Ifs[2].Idx != 10 || !s.IfX[0].Sfp || s.IfX[0].SfpRx != -7.5 {
		t.Fatalf("Ports: %+v %+v", s.Ifs, s.IfX)
	}
	if len(s.Log) != 1 {
		t.Fatalf("Log: %+v", s.Log)
	}
	bad := mtQuery(newMtClient(host, pn, "pw", "falsch"), &mtAgentState{})
	if !strings.Contains(bad.Err, "Anmeldung") {
		t.Fatalf("falsches Passwort: %q", bad.Err)
	}

	sv, _ := newTestServer(t)
	m := sv.m
	m.mu.Lock()
	m.cfg.Sites = append(m.cfg.Sites, SiteCfg{ID: "s1", Name: "Büro", Org: "default"})
	m.mu.Unlock()
	hc := m.UpsertHost(HostCfg{Name: "Leibstadt", Addr: "192.168.215.1", Site: "s1", Org: "default", Mt: true, MtUser: "pw", MtPass: "geheim"})
	if !hc.Mt || hc.MtPort != 443 {
		t.Fatalf("Konfiguration: %+v", hc)
	}
	t0 := time.Now().Add(-3 * time.Minute).UnixMilli()
	s.T = t0
	m.ingestMikrotik(hc, []mtSnap{s})
	// Neustart + Netzteil weg + Sitzungen eingebrochen
	up, psu, ppp = "00:01:00", "fail", 40
	s2 := mtQuery(c, st)
	s2.T = t0 + 120000
	m.ingestMikrotik(hc, []mtSnap{s2})
	v := m.mtHostView(hc, 24)
	if v.Boots != 1 || len(v.Open) != 2 || len(v.Hist) != 2 || v.Cur.Res["identity"] != "Leibstadt" {
		t.Fatalf("Ansicht: boots=%d open=%v hist=%d", v.Boots, v.Open, len(v.Hist))
	}
	if ih := getIfHost(hc.ID); len(ih.info) != 3 {
		t.Fatalf("Ports nicht in der Statistik: %d", len(ih.info))
	}
	if publicHost(hc).MtPass != "" {
		t.Fatal("Passwort im Browser")
	}
}

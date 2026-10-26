package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeNetonix(t *testing.T, bounced *string) *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.php" {
			r.ParseForm()
			if r.Form.Get("username") == "pw" && r.Form.Get("password") == "geheim" {
				http.SetCookie(w, &http.Cookie{Name: "sess", Value: "ok"})
				w.Header().Set("Location", "/main.html")
			} else {
				w.Header().Set("Location", "/index.html?err")
			}
			w.WriteHeader(302)
			return
		}
		if c, err := r.Cookie("sess"); err != nil || c.Value != "ok" {
			w.Header().Set("Location", "/")
			w.WriteHeader(302)
			return
		}
		switch r.URL.Path {
		case "/api/v1/bootid":
			w.Write([]byte(`"12345"`))
		case "/api/v1/status/30sec":
			json.NewEncoder(w).Encode(map[string]any{"Uptime": "3600.0", "CPU_Usage": "30", "CPU_Temp": "70", "Board_Temp": "39", "PHY_Temp": "69", "Board_48V": "49.3", "Board_24V": "24.5", "Model": "WS-12-250-AC", "Firmware_Version": "1.5.16",
				"Ports": []map[string]any{{"Number": "1", "Link": "1G", "PoE": "48V", "Power": "6.7", "TxOctets": "1000", "RxOctets": "2000", "RxErrors": "0", "TxErrors": "0"}, {"Number": "2", "Link": "100M-F", "PoE": "24V", "Power": "3.1", "TxOctets": "1", "RxOctets": "1"}}})
		case "/api/v1/config":
			json.NewEncoder(w).Encode(map[string]any{"Switch_Name": "Netonix-Vogelbach", "Credentials_Password": "x", "Ports": []map[string]any{{"Number": 1, "Name": "60GHZ Nier", "Enable": true}, {"Number": 2, "Name": "Ersatz", "Enable": true}}})
		case "/api/v1/portbounce", "/api/v1/portbouncelink":
			*bounced = r.URL.Path + "?" + r.URL.RawQuery
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestNetonix(t *testing.T) {
	var bounced string
	srv := fakeNetonix(t, &bounced)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	i := strings.LastIndex(addr, ":")
	port, _ := strconv.Atoi(addr[i+1:])
	s := nxQuery(newNxClient(addr[:i], port, "pw", "geheim"))
	if s.Err != "" || s.Name != "Netonix-Vogelbach" || s.CPUT != 70 || len(s.Ports) != 2 || s.Ports[0].Name != "60GHZ Nier" || s.Ifs[1].Speed != 100 || s.Uptime != 3600 {
		t.Fatalf("%+v", s)
	}
	if bad := nxQuery(newNxClient(addr[:i], port, "pw", "falsch")); !strings.Contains(bad.Err, "Anmeldung") {
		t.Fatalf("falsches Passwort: %q", bad.Err)
	}
	sv, _ := newTestServer(t)
	m := sv.m
	m.mu.Lock()
	m.cfg.Sites = append(m.cfg.Sites, SiteCfg{ID: "s1", Name: "Büro", Org: "default"})
	m.mu.Unlock()
	hc := m.UpsertHost(HostCfg{Name: "Netonix Vogelbach", Addr: addr[:i], Site: "s1", Org: "default", Nx: true, NxUser: "pw", NxPass: "geheim", NxPort: port, NxActPorts: "2"})
	if !hc.Nx || hc.NxPort != port || publicHost(hc).NxPass != "" {
		t.Fatalf("cfg %+v", hc)
	}
	s.T = time.Now().Add(-time.Minute).UnixMilli()
	m.ingestNetonix(hc, []nxSnap{s})
	v := m.nxHostView(hc, 24)
	if v.Cur == nil || len(v.Hist) != 1 || len(getIfHost(hc.ID).info) != 2 {
		t.Fatalf("view %+v", v)
	}
	// Aktion über den Agenten-Code
	a := &Agent{hosts: map[string]*agentHost{hc.ID: {cfg: hc}}}
	if _, err := a.nxActionJob(agentJob{Params: map[string]string{"host": hc.ID, "action": "poe", "port": "1"}}); err == nil {
		t.Fatal("Port 1 ist nicht freigegeben")
	}
	if _, err := a.nxActionJob(agentJob{Params: map[string]string{"host": hc.ID, "action": "poe", "port": "2"}}); err != nil || bounced != "/api/v1/portbounce?port=2" {
		t.Fatalf("%v %q", err, bounced)
	}
}

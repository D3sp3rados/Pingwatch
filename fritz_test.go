package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFritz simuliert eine Fritzbox mit TR-064 (Digest-Anmeldung, SOAP).
type fakeFritz struct {
	mu         sync.Mutex
	user, pass string
	conn       string
	connUp     int64
	boxUp      int64
	ipMode     bool // Verbindung über WANIPConnection statt PPP
	calls      int
}

func (f *fakeFritz) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	const realm, nonce = "F!Box SOAP-Auth", "4F2A1C9E"
	auth := parseDigest(r.Header.Get("Authorization"))
	ok := false
	if auth["nonce"] == nonce && auth["username"] == f.user {
		ha1 := md5hex(f.user + ":" + realm + ":" + f.pass)
		ha2 := md5hex(r.Method + ":" + auth["uri"])
		want := md5hex(ha1 + ":" + nonce + ":" + auth["nc"] + ":" + auth["cnonce"] + ":" + auth["qop"] + ":" + ha2)
		ok = auth["response"] == want && auth["uri"] == r.URL.Path
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5, qop="auth"`, realm, nonce))
		w.WriteHeader(401)
		return
	}
	act := r.Header.Get("SoapAction")
	io.Copy(io.Discard, r.Body)
	vals := map[string]string{}
	switch act {
	case svcDeviceInfo + "#GetInfo":
		vals = map[string]string{"NewModelName": "FRITZ!Box 7590 AX", "NewSoftwareVersion": "164.08.00", "NewUpTime": fmt.Sprint(f.boxUp)}
	case svcCommonIf + "#GetCommonLinkProperties":
		vals = map[string]string{"NewWANAccessType": "DSL", "NewPhysicalLinkStatus": "Up", "NewLayer1DownstreamMaxBitRate": "300000000", "NewLayer1UpstreamMaxBitRate": "50000000"}
	case svcPPP + "#GetStatusInfo":
		if f.ipMode {
			vals = map[string]string{"NewConnectionStatus": "Unconfigured"}
		} else {
			vals = map[string]string{"NewConnectionStatus": f.conn, "NewUptime": fmt.Sprint(f.connUp), "NewLastConnectionError": "ERROR_NONE"}
		}
	case svcIP + "#GetStatusInfo":
		if f.ipMode {
			vals = map[string]string{"NewConnectionStatus": f.conn, "NewUptime": fmt.Sprint(f.connUp)}
		} else {
			vals = map[string]string{"NewConnectionStatus": "Unconfigured"}
		}
	case svcPPP + "#GetExternalIPAddress", svcIP + "#GetExternalIPAddress":
		vals = map[string]string{"NewExternalIPAddress": "203.0.113.7"}
	case svcDSL + "#GetInfo":
		vals = map[string]string{"NewDownstreamCurrRate": "116789", "NewUpstreamCurrRate": "40123"}
	default:
		w.WriteHeader(500)
		fmt.Fprint(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><detail><UPnPError xmlns="urn:dslforum-org:control-1-0"><errorCode>401</errorCode><errorDescription>Invalid Action</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
		return
	}
	action := act[strings.Index(act, "#")+1:]
	var b strings.Builder
	for k, v := range vals {
		b.WriteString("<" + k + ">" + v + "</" + k + ">")
	}
	fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%sResponse xmlns:u="x">%s</u:%sResponse></s:Body></s:Envelope>`, action, b.String(), action)
}

func TestFritzBase(t *testing.T) {
	cases := map[string]string{
		fritzBase("192.168.178.1", 0):             "http://192.168.178.1:49000",
		fritzBase("fritz.box", 49443):             "https://fritz.box:49443",
		fritzBase("http://192.168.178.1/", 49000): "http://192.168.178.1:49000",
		fritzBase("fd00::1", 49000):               "http://[fd00::1]:49000",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("fritzBase: %q, erwartet %q", got, want)
		}
	}
}

func TestFritzQuery(t *testing.T) {
	f := &fakeFritz{user: "pingwacht", pass: "geheim", conn: "Connected", connUp: 3600, boxUp: 86400}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := newTR064(srv.URL, "pingwacht", "geheim")
	st, err := fritzQuery(c)
	if err != nil {
		t.Fatal(err)
	}
	if st.Model != "FRITZ!Box 7590 AX" || st.FW != "164.08.00" || st.BoxUptime != 86400 {
		t.Fatalf("Geräteinfo: %+v", st)
	}
	if !st.connected() || st.ConnUptime != 3600 || st.ExtIP != "203.0.113.7" || st.LastErr != "" {
		t.Fatalf("Verbindung: %+v", st)
	}
	if !st.DSL || st.Down != 116789000 || st.Up != 40123000 {
		t.Fatalf("DSL-Sync: %+v", st)
	}
	// zweite Abfrage nutzt die gespeicherte Nonce (keine 401 mehr)
	before := f.calls
	if _, err := fritzQuery(c); err != nil {
		t.Fatal(err)
	}
	if f.calls-before != 6 {
		t.Errorf("Anfragen bei bekannter Nonce: %d", f.calls-before)
	}
	// Kabel/Glasfaser: Verbindung über WANIPConnection
	f.ipMode = true
	st, err = fritzQuery(c)
	if err != nil || !st.connected() || st.ConnUptime != 3600 {
		t.Fatalf("IP-Verbindung: %+v %v", st, err)
	}
	// falsches Passwort
	if _, err := fritzQuery(newTR064(srv.URL, "pingwacht", "falsch")); !errors.Is(err, errFritzAuth) {
		t.Fatalf("falsches Passwort: %v", err)
	}
}

func TestParseDigest(t *testing.T) {
	d := parseDigest(`Digest realm="F!Box SOAP-Auth", nonce="AB,CD", algorithm=MD5, qop="auth,auth-int"`)
	if d["realm"] != "F!Box SOAP-Auth" || d["nonce"] != "AB,CD" || d["algorithm"] != "MD5" || d["qop"] != "auth,auth-int" {
		t.Fatalf("%v", d)
	}
}

func drainNotices(m *Monitor) []notice {
	var out []notice
	for {
		select {
		case n := <-m.notices:
			out = append(out, n)
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

func TestFritzIngest(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	m.notices = make(chan notice, 100)
	hc := m.UpsertHost(HostCfg{Name: "Fritzbox Müller", Addr: "192.168.178.1", Site: "s1", Fritz: true, FritzUser: "u", FritzPass: "p"})
	if !hc.Fritz || hc.FritzPort != 49000 {
		t.Fatalf("normalize: %+v", hc)
	}
	t0 := time.Now().Add(-20 * time.Minute).UnixMilli()
	min := int64(60000)
	st := func(conn string, connUp, boxUp int64) *fritzStatus {
		return &fritzStatus{Model: "7590", Conn: conn, ConnUptime: connUp, BoxUptime: boxUp, Down: 100e6, Up: 40e6}
	}
	// verbunden
	m.ingestFritz(hc, []fritzSample{{T: t0, S: st("Connected", 1000, 5000)}, {T: t0 + min, S: st("Connected", 1060, 5060)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("unerwartete Meldungen: %+v", n)
	}
	// Abbruch, nachgereicht (Agent konnte den Server nicht erreichen): keine „getrennt“-Meldung, aber „wieder verbunden“
	m.ingestFritz(hc, []fritzSample{
		{T: t0 + 2*min, S: st("Disconnected", 0, 5120)},
		{T: t0 + 3*min, S: st("Disconnected", 0, 5180)},
		{T: t0 + 4*min, S: st("Disconnected", 0, 5240)},
	})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("alte Messungen dürfen nicht melden: %+v", n)
	}
	now := time.Now().UnixMilli()
	m.ingestFritz(hc, []fritzSample{{T: now - 30000, S: st("Connected", 20, 6300)}})
	n := drainNotices(m)
	if len(n) != 1 || n[0].kind != "wanup" || n[0].hostID != hc.ID || !strings.Contains(n[0].body, "getrennt") || !strings.Contains(n[0].body, "192.168.178.1") {
		t.Fatalf("Wieder-verbunden-Meldung: %+v", n)
	}
	evs := m.queryEvents("", hc.ID, "", map[string]bool{"wan": true}, 0, 0, 10)
	if len(evs) != 1 || evs[0].End == 0 || evs[0].Start != t0+2*min {
		t.Fatalf("Ereignis: %+v", evs)
	}
	// aktueller Abbruch: Meldung „Internet getrennt“ nach zwei Messungen
	m.ingestFritz(hc, []fritzSample{{T: now - 20000, S: st("Disconnected", 0, 6310)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("nach einer Messung schon gemeldet: %+v", n)
	}
	m.ingestFritz(hc, []fritzSample{{T: now - 10000, S: st("Connecting", 0, 6320)}})
	n = drainNotices(m)
	if len(n) != 1 || n[0].kind != "wandown" || !strings.Contains(n[0].title, "Internet getrennt") || !strings.Contains(n[0].body, "Connecting") {
		t.Fatalf("Getrennt-Meldung: %+v", n)
	}
	m.ingestFritz(hc, []fritzSample{{T: now - 5000, S: st("Connected", 3, 6325)}})
	if n := drainNotices(m); len(n) != 1 || n[0].kind != "wanup" {
		t.Fatalf("Wieder-verbunden: %+v", n)
	}
	// kurze Neueinwahl (Zwangstrennung): nur Ereignis, keine Meldung
	m.ingestFritz(hc, []fritzSample{{T: now + 30000, S: st("Connected", 500, 6360)}, {T: now + 55000, S: st("Connected", 10, 6385)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("kurze Neueinwahl darf nicht melden: %+v", n)
	}
	if evs := m.queryEvents("", hc.ID, "", map[string]bool{"wan": true}, 0, 0, 10); len(evs) != 3 || !strings.Contains(evs[0].HostName, "Neueinwahl") {
		t.Fatalf("Neueinwahl-Ereignis: %+v", evs)
	}
	// Neustart der Box
	m.ingestFritz(hc, []fritzSample{{T: now + 58000, S: st("Connected", 30, 40)}})
	n = drainNotices(m)
	if len(n) != 1 || n[0].kind != "reboot" || !strings.Contains(n[0].title, "neu gestartet") {
		t.Fatalf("Neustart-Meldung: %+v", n)
	}
	v := m.fritzHostView(hc)
	if v.Status == nil || v.Drops24 < 3 || v.Boots24 != 1 || v.Avail <= 0 || v.Avail >= 100 {
		t.Fatalf("Ansicht: drops=%d boots=%d avail=%.1f", v.Drops24, v.Boots24, v.Avail)
	}
	// Wartung: keine Meldungen
	m.cfg.Maints = append(m.cfg.Maints, Maint{ID: "w", Org: hc.Org, Start: now - 1000, End: now + 3600000})
	m.ingestFritz(hc, []fritzSample{{T: now + 59000, S: st("Disconnected", 0, 100)}, {T: now + 59500, S: st("Disconnected", 0, 101)}})
	if n := drainNotices(m); len(n) != 0 {
		t.Fatalf("Meldung trotz Wartung: %+v", n)
	}
}

func TestFritzAPI(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	c.do("POST", "/api/sites", map[string]string{"name": "Kunde Y"})
	s.m.mu.RLock()
	site := s.m.cfg.Sites[0]
	s.m.mu.RUnlock()
	code, body := c.do("POST", "/api/hosts", map[string]any{"name": "Fritzbox", "addr": "192.168.178.1", "site": site.ID, "method": "icmp",
		"fritz": true, "fritzUser": "pw", "fritzPass": "streng-geheim"})
	if code != 200 || strings.Contains(body, "streng-geheim") {
		t.Fatal(code, body)
	}
	var hc HostCfg
	json.Unmarshal([]byte(body), &hc)
	// Bearbeiten ohne Passwort behält das alte
	hc.Name = "Fritzbox Büro"
	c.do("POST", "/api/hosts", hc)
	if h := s.m.host(hc.ID); h == nil || h.cfg.FritzPass != "streng-geheim" || h.cfg.Name != "Fritzbox Büro" {
		t.Fatal("Passwort beim Bearbeiten verloren")
	}
	_, st := c.do("GET", "/api/state", nil)
	if strings.Contains(st, "streng-geheim") || !strings.Contains(st, `"fritzPassSet":true`) {
		t.Fatal("Passwort im Dashboard:", st)
	}
	_, list := c.do("GET", "/api/hosts", nil)
	if strings.Contains(list, "streng-geheim") {
		t.Fatal("Passwort in /api/hosts")
	}
	agent := func(method, path string, v any) string {
		b, _ := json.Marshal(v)
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+site.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		return string(rb)
	}
	if cfg := agent("GET", "/api/agent/config", nil); !strings.Contains(cfg, `"fritzPass":"streng-geheim"`) {
		t.Fatal("Agent bekommt das Passwort nicht:", cfg)
	}
	now := time.Now().UnixMilli()
	agent("POST", "/api/agent/fritz", map[string]any{"host": hc.ID, "samples": []fritzSample{{T: now, S: &fritzStatus{Model: "FRITZ!Box 6660 Cable", Conn: "Connected", ConnUptime: 99, ExtIP: "198.51.100.4"}}}})
	_, fb := c.do("GET", "/api/fritz?host="+hc.ID, nil)
	if !strings.Contains(fb, "6660 Cable") || !strings.Contains(fb, "198.51.100.4") {
		t.Fatal("Fritzbox-Ansicht:", fb)
	}
	// Cloud-Hosts können keine Fritzbox auslesen
	_, body = c.do("POST", "/api/hosts", map[string]any{"name": "x", "addr": "203.0.113.1", "site": "cloud", "fritz": true, "fritzPass": "a"})
	if strings.Contains(body, `"fritz":true`) {
		t.Fatal("Fritzbox bei Cloud-Host:", body)
	}
}

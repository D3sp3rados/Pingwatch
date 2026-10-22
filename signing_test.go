package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return k, base64.StdEncoding.EncodeToString(der)
}

func signP1363(k *ecdsa.PrivateKey, msg string) string {
	h := sha256.Sum256([]byte(msg))
	r, s, _ := ecdsa.Sign(rand.Reader, k, h[:])
	b := make([]byte, 64)
	r.FillBytes(b[:32])
	s.FillBytes(b[32:])
	return base64.StdEncoding.EncodeToString(b)
}

func TestSignedActions(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	_, body := c.do("POST", "/api/totp", map[string]string{"action": "setup"})
	var su struct{ Secret string }
	json.Unmarshal([]byte(body), &su)
	code := func() string {
		totpState.Lock()
		totpState.used = map[string]uint64{}
		totpState.Unlock()
		return totpAt(su.Secret, uint64(time.Now().Unix()/30))
	}
	c.do("POST", "/api/totp", map[string]string{"action": "enable", "code": code()})
	c.do("POST", "/api/actions/consent", map[string]any{"accept": true})
	_, sb := c.do("POST", "/api/sites", map[string]any{"name": "Büro"})
	var site SiteCfg
	json.Unmarshal([]byte(sb), &site)
	site, _ = s.m.siteByID(site.ID)
	c.do("POST", "/api/sites", map[string]any{"id": site.ID, "sys": map[string]any{"on": true, "updates": true}})

	// Agent (Standort-Agent) mit eigener Rechner-Kennung
	dir := t.TempDir()
	a := &Agent{file: AgentFile{Server: ts.URL, Token: site.Token}, dataDir: dir, hosts: map[string]*agentHost{},
		refresh: make(chan struct{}, 1), stopAll: make(chan struct{}), notify: func(string, string, bool) {}, changed: func() {},
		client: &http.Client{Timeout: 5 * time.Second}, machine: sysMachineID(dir)}
	mc, _ := s.m.registerMachine(site, a.machine, "PC", false)

	// Schlüssel anlegen (erster: ohne Bestätigung), zweiter nur mit Bestätigung
	k1, pub1 := testKey(t)
	if code, b := c.do("POST", "/api/signkeys", map[string]any{"pub": pub1, "name": "Browser 1", "code": code()}); code != 200 {
		t.Fatal(code, b)
	}
	k2, pub2 := testKey(t)
	if code, _ := c.do("POST", "/api/signkeys", map[string]any{"pub": pub2, "name": "Browser 2", "code": code()}); code != 400 {
		t.Fatal("zweiter Schlüssel ohne Bestätigung", code)
	}
	end := spkiID(pub1) + ":" + signP1363(k1, endorseMsg("default", pub2))
	if code, b := c.do("POST", "/api/signkeys", map[string]any{"pub": pub2, "name": "Browser 2", "endorse": end, "code": code()}); code != 200 {
		t.Fatal(code, b)
	}
	a.fetchConfig()
	if len(a.trust.Keys) != 2 {
		t.Fatalf("Agent kennt %d Schlüssel", len(a.trust.Keys))
	}

	// ohne Signatur abgelehnt
	if code, _ := c.do("POST", "/api/actions", map[string]any{"kind": "reboot", "scope": "machine", "id": mc.ID, "code": code()}); code != 400 {
		t.Fatal("ohne Signatur", code)
	}
	// Vorschau → unterschreiben (mit Schlüssel 2) → auslösen
	_, pb := c.do("POST", "/api/actions", map[string]any{"kind": "reboot", "scope": "machine", "id": mc.ID, "preview": true})
	var pv struct {
		Payload string
		TS      int64
		Nonce   string
	}
	json.Unmarshal([]byte(pb), &pv)
	sig := signP1363(k2, pv.Payload)
	if code, b := c.do("POST", "/api/actions", map[string]any{"kind": "reboot", "scope": "machine", "id": mc.ID, "ts": pv.TS, "nonce": pv.Nonce, "sig": sig, "kid": spkiID(pub2), "code": code()}); code != 200 {
		t.Fatal("signiert", code, b)
	}
	jb := takeJobs(site.ID)
	if len(jb) != 1 {
		t.Fatal(jb)
	}
	if err := a.verifyJob(jb[0]); err != nil {
		t.Fatal("gültiger Auftrag abgelehnt:", err)
	}
	if err := a.verifyJob(jb[0]); err == nil || !strings.Contains(err.Error(), "Wiederholung") {
		t.Fatal("Wiederholung angenommen", err)
	}
	// gefälschter Auftrag (Server tauscht Art) → abgelehnt
	fake := jb[0]
	fake.Kind = "script"
	fake.Params = map[string]string{"sp": jb[0].Params["sp"], "sig": jb[0].Params["sig"], "kid": jb[0].Params["kid"], "script": "evil", "cat": "security", "reboot": "never"}
	if err := a.verifyJob(fake); err == nil {
		t.Fatal("gefälschter Auftrag angenommen")
	}
	// ganz ohne Signatur → abgelehnt
	if err := a.verifyJob(agentJob{Kind: "reboot", Params: map[string]string{"machine": mc.ID, "cat": "security", "reboot": "never"}}); err == nil {
		t.Fatal("unsignierter Auftrag angenommen")
	}
	// Server tauscht Schlüssel aus → Agent übernimmt ihn nicht
	k3, pub3 := testKey(t)
	_ = k3
	s.m.mu.Lock()
	for i := range s.m.cfg.Orgs {
		if s.m.cfg.Orgs[i].ID == "default" {
			s.m.cfg.Orgs[i].SignKeys = []SignKey{{ID: spkiID(pub3), Pub: pub3}}
		}
	}
	s.m.mu.Unlock()
	a.fetchConfig()
	for _, k := range a.trust.Keys {
		if k.ID == spkiID(pub3) {
			t.Fatal("untergeschobener Schlüssel übernommen")
		}
	}
}

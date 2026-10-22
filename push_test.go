package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPushRoundtrip(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	p8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	// alle Varianten, wie der Schlüssel in einer Umgebungsvariable landen kann
	for name, v := range map[string]string{
		"pem":        p8,
		"oneline":    strings.ReplaceAll(strings.TrimSpace(p8), "\n", " "),
		"escaped":    strings.ReplaceAll(p8, "\n", `\n`),
		"base64file": base64.StdEncoding.EncodeToString([]byte(p8)),
		"bodyonly":   base64.StdEncoding.EncodeToString(der),
	} {
		got, err := parseP8(v)
		if err != nil || !got.Equal(k) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var gotPath, gotAuth, gotTopic string
	var gotBody map[string]any
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotTopic = r.URL.Path, r.Header.Get("authorization"), r.Header.Get("apns-topic")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		if r.ProtoMajor != 2 {
			t.Errorf("kein HTTP/2: %s", r.Proto)
		}
		if strings.Contains(r.URL.Path, "dead") {
			w.WriteHeader(410)
			w.Write([]byte(`{"reason":"Unregistered"}`))
			return
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	p := &Pusher{keyID: "ABC123", teamID: "TEAM42", topic: "de.rst.pingwatch", key: k, throttle: map[string]time.Time{}}
	p.client = srv.Client()
	p.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	p.client.Transport.(*http.Transport).ForceAttemptHTTP2 = true
	apnsBase = srv.URL
	if err := p.send(Device{Token: "aabbcc", Sandbox: true}, notice{"outage", "h1", "Titel", "Text", "", ""}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/3/device/aabbcc" || gotTopic != "de.rst.pingwatch" {
		t.Fatalf("Pfad/Topic: %s %s", gotPath, gotTopic)
	}
	aps := gotBody["aps"].(map[string]any)
	if aps["interruption-level"] != "time-sensitive" || aps["alert"].(map[string]any)["title"] != "Titel" {
		t.Fatalf("payload %v", gotBody)
	}
	// JWT prüfen
	parts := strings.Split(strings.TrimPrefix(gotAuth, "bearer "), ".")
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if !strings.Contains(string(hdr), `"kid":"ABC123"`) || !strings.Contains(string(hdr), "ES256") {
		t.Fatalf("header %s", hdr)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&k.PublicKey, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("Signatur ungültig")
	}
	if err := p.send(Device{Token: "dead"}, notice{"outage", "h1", "x", "y", "", ""}); err != errBadToken {
		t.Fatalf("erwartet errBadToken, bekam %v", err)
	}
}

func TestOutagePushFlow(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	got := make(chan string, 10)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Kind string `json:"kind"`
			Aps  struct {
				Alert struct{ Title string } `json:"alert"`
			} `json:"aps"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		got <- b.Kind + "|" + b.Aps.Alert.Title
	}))
	defer srv.Close()
	apnsBase = srv.URL
	m := NewMonitor(NewFileStore(t.TempDir()))
	m.pusher = &Pusher{keyID: "K", teamID: "T", topic: "x", key: k, client: srv.Client(), throttle: map[string]time.Time{}}
	m.notices = make(chan notice, 100)
	go m.pushLoop()
	hc := m.UpsertHost(HostCfg{Name: "Antenne", Addr: "10.0.0.1", Site: "s1", Paused: false})
	m.cfg.Devices = []Device{{Token: strings.Repeat("a", 64), Name: "Test", Outage: true}}
	h := m.host(hc.ID)
	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		m.Ingest(h, now+int64(i)*1000, -1, "", "timeout")
	}
	m.Ingest(h, now+6000, 120, "", "")
	want := []string{"outage|🔴 Antenne antwortet nicht", "recovered|🟢 Antenne wieder erreichbar"}
	for _, w := range want {
		select {
		case g := <-got:
			if g != w {
				t.Fatalf("erwartet %q, bekam %q", w, g)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("keine Push-Nachricht für %q", w)
		}
	}
}

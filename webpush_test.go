package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Gegenstück des Browsers: entschlüsselt aes128gcm (RFC 8291).
func webpushDecrypt(t *testing.T, body []byte, ua *ecdh.PrivateKey, auth []byte) []byte {
	salt, idlen := body[:16], int(body[20])
	asPub := body[21 : 21+idlen]
	ct := body[21+idlen:]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := ua.ECDH(as)
	prkKey := hmacSHA256(auth, secret)
	keyInfo := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPub...)
	ikm := hmacSHA256(prkKey, append(keyInfo, 1))
	prk := hmacSHA256(salt, ikm)
	cek := hmacSHA256(prk, append([]byte("Content-Encoding: aes128gcm\x00"), 1))[:16]
	nonce := hmacSHA256(prk, append([]byte("Content-Encoding: nonce\x00"), 1))[:12]
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatal("Entschlüsselung:", err)
	}
	if pt[len(pt)-1] != 2 {
		t.Fatal("Padding-Trenner fehlt")
	}
	return pt[:len(pt)-1]
}

func TestWebPush(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("admin", "admin-pass-123")
	_, body := c.do("GET", "/api/webpush", nil)
	var kr struct{ Key string }
	mustJSON(t, body, &kr)
	if k, _ := b64urlDecode(kr.Key); len(k) != 65 {
		t.Fatal("VAPID-Schlüssel:", body)
	}
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var mu sync.Mutex
	var got [][]byte
	var hdr http.Header
	gone := false
	push := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if gone {
			w.WriteHeader(410)
			return
		}
		got = append(got, b)
		hdr = r.Header
		w.WriteHeader(201)
	}))
	defer push.Close()
	old := webpushClient
	webpushClient = push.Client()
	defer func() { webpushClient = old }()
	sub := map[string]any{"subscription": map[string]any{"endpoint": push.URL + "/fcm/abc",
		"keys": map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(auth)}},
		"name": "Chrome auf Android", "outage": true}
	if code, body := c.do("POST", "/api/webpush", sub); code != 200 {
		t.Fatal(code, body)
	}
	// Testnachricht ohne Apple-Push
	code, body := c.do("POST", "/api/app/pushtest", nil)
	if code != 200 || !strings.Contains(body, `"sent":1`) {
		t.Fatal("pushtest:", code, body)
	}
	mu.Lock()
	if len(got) != 1 || hdr.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(hdr.Get("Authorization"), "vapid t=") || !strings.Contains(hdr.Get("Authorization"), "k="+kr.Key) {
		t.Fatalf("Header: %v", hdr)
	}
	var msg map[string]any
	json.Unmarshal(webpushDecrypt(t, got[0], ua, auth), &msg)
	mu.Unlock()
	if msg["title"] != "PingWacht" || !strings.Contains(msg["body"].(string), "funktioniert") {
		t.Fatalf("Inhalt: %v", msg)
	}
	go s.m.pushLoop()
	// Ausfall über pushLoop, danach Abo abgelaufen → Gerät wird entfernt
	s.m.notify(notice{"outage", "h1", "🔴 Router antwortet nicht", "x", "default"})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := len(got)
	gone = true
	mu.Unlock()
	if n != 2 {
		t.Fatal("Ausfall nicht per Web-Push gesendet", n)
	}
	s.m.notify(notice{"outage", "h1", "nochmal", "x", "default"})
	time.Sleep(300 * time.Millisecond)
	s.m.mu.RLock()
	left := len(s.m.cfg.Devices)
	s.m.mu.RUnlock()
	if left != 0 {
		t.Fatal("abgelaufenes Abo nicht entfernt")
	}
}

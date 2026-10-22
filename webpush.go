package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Web-Push (RFC 8030/8291/8292): Benachrichtigungen an Browser und installierte Web-App
// – Android (Chrome, Firefox, Edge), Windows/macOS und iPhone (Web-App auf dem Home-Bildschirm, ab iOS 16.4).

type vapidKeys struct {
	priv *ecdsa.PrivateKey
	pub  string // base64url, unkomprimierter Punkt (65 Byte)
}

var vapid struct {
	sync.Mutex
	k *vapidKeys
}

func b64urlDecode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// vapidLocked: Schlüsselpaar aus der Konfiguration laden oder einmalig erzeugen (m.mu muss gehalten werden).
func (m *Monitor) vapidLocked() *vapidKeys {
	vapid.Lock()
	defer vapid.Unlock()
	if vapid.k != nil {
		return vapid.k
	}
	if m.cfg.VapidKey != "" {
		if der, err := base64.StdEncoding.DecodeString(m.cfg.VapidKey); err == nil {
			if k, err := x509.ParseECPrivateKey(der); err == nil {
				vapid.k = &vapidKeys{priv: k, pub: vapidPub(k)}
				return vapid.k
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil
	}
	der, _ := x509.MarshalECPrivateKey(k)
	m.cfg.VapidKey = base64.StdEncoding.EncodeToString(der)
	m.saveConfigLocked()
	vapid.k = &vapidKeys{priv: k, pub: vapidPub(k)}
	return vapid.k
}

func (m *Monitor) vapidKey() *vapidKeys {
	vapid.Lock()
	k := vapid.k
	vapid.Unlock()
	if k != nil {
		return k
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vapidLocked()
}

func vapidPub(k *ecdsa.PrivateKey) string {
	ek, err := k.PublicKey.ECDH()
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(ek.Bytes())
}

func (k *vapidKeys) jwt(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	hdr, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256"})
	sub := "mailto:" + envOr("PROVIDER_EMAIL", "pingwatch@rst.de")
	cl, _ := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": time.Now().Add(12 * time.Hour).Unix(), "sub": sub})
	unsigned := b64url(hdr) + "." + b64url(cl)
	h := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return unsigned + "." + b64url(sig), nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// webpushEncrypt verschlüsselt nach RFC 8291 (aes128gcm).
func webpushEncrypt(payload []byte, p256dh, authSecret []byte) ([]byte, error) {
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(p256dh)
	if err != nil {
		return nil, fmt.Errorf("p256dh ungültig: %v", err)
	}
	as, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	prkKey := hmacSHA256(authSecret, secret)
	keyInfo := append(append([]byte("WebPush: info\x00"), p256dh...), asPub...)
	ikm := hmacSHA256(prkKey, append(keyInfo, 1))
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	prk := hmacSHA256(salt, ikm)
	cek := hmacSHA256(prk, append([]byte("Content-Encoding: aes128gcm\x00"), 1))[:16]
	nonce := hmacSHA256(prk, append([]byte("Content-Encoding: nonce\x00"), 1))[:12]
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, append(append([]byte{}, payload...), 2), nil)
	var out bytes.Buffer
	out.Write(salt)
	rs := make([]byte, 4)
	binary.BigEndian.PutUint32(rs, 4096)
	out.Write(rs)
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(ct)
	return out.Bytes(), nil
}

var webpushClient = &http.Client{Timeout: 15 * time.Second}

var errGone = errors.New("Abo abgelaufen")

func (m *Monitor) sendWebPush(d Device, n notice) error {
	k := m.vapidKey()
	if k == nil {
		return errors.New("VAPID-Schlüssel fehlt")
	}
	p256, err := b64urlDecode(d.P256dh)
	if err != nil {
		return err
	}
	auth, err := b64urlDecode(d.Auth)
	if err != nil {
		return err
	}
	url := "/#/events"
	if n.hostID != "" {
		url = "/#/host/" + n.hostID
	}
	if n.kind == "request" {
		url = "/#/requests"
	}
	payload, _ := json.Marshal(map[string]any{"title": n.title, "body": n.body, "host": n.hostID, "kind": n.kind, "url": url,
		"urgent": n.kind == "outage" || n.kind == "offline" || n.kind == "devoff" || n.kind == "wandown"})
	body, err := webpushEncrypt(payload, p256, auth)
	if err != nil {
		return err
	}
	jwt, err := k.jwt(d.Endpoint)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("POST", d.Endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Topic", topicOf(n))
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+k.pub)
	resp, err := webpushClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 410 {
		return errGone
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("Web-Push HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// topicOf: gleiche Meldung für denselben Host ersetzt eine ältere, noch nicht zugestellte (max. 32 Zeichen).
func topicOf(n notice) string {
	h := sha256.Sum256([]byte(n.kind + "|" + n.hostID + "|" + n.title))
	return hex.EncodeToString(h[:])[:32]
}

// ---------- API ----------

// GET /api/webpush → {key} · POST /api/webpush {subscription, name, prefs} · DELETE /api/webpush?endpoint=
func (s *Server) handleWebPush(w http.ResponseWriter, r *http.Request) {
	m := s.m
	switch r.Method {
	case http.MethodGet:
		k := m.vapidKey()
		if k == nil {
			http.Error(w, "VAPID-Schlüssel fehlt", 500)
			return
		}
		writeJSON(w, map[string]string{"key": k.pub})
	case http.MethodPost:
		var req struct {
			Subscription struct {
				Endpoint string `json:"endpoint"`
				Keys     struct {
					P256dh string `json:"p256dh"`
					Auth   string `json:"auth"`
				} `json:"keys"`
			} `json:"subscription"`
			Name    string   `json:"name"`
			Outage  bool     `json:"outage"`
			Loss    bool     `json:"loss"`
			Spike   bool     `json:"spike"`
			Offline bool     `json:"offline"`
			AllOrgs bool     `json:"allOrgs"`
			Hosts   []string `json:"hosts"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		sub := req.Subscription
		if !strings.HasPrefix(sub.Endpoint, "https://") || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
			http.Error(w, "Push-Abo ungültig", 400)
			return
		}
		if b, err := b64urlDecode(sub.Keys.P256dh); err != nil || len(b) != 65 {
			http.Error(w, "Push-Schlüssel ungültig", 400)
			return
		}
		p := principal(r)
		h := sha256.Sum256([]byte(sub.Endpoint))
		d := Device{Token: "web-" + hex.EncodeToString(h[:])[:40], Kind: "web", Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth,
			Org: p.Org, Name: strings.TrimSpace(req.Name), Outage: req.Outage, Loss: req.Loss, Spike: req.Spike, Offline: req.Offline,
			AllOrgs: req.AllOrgs && p.Super, Hosts: req.Hosts, Added: time.Now().UnixMilli()}
		if d.Name == "" {
			d.Name = "Browser"
		}
		if len(d.Name) > 60 {
			d.Name = d.Name[:60]
		}
		if p.User != nil {
			d.User = p.User.ID
		}
		m.mu.Lock()
		found := false
		for i := range m.cfg.Devices {
			if m.cfg.Devices[i].Token == d.Token {
				d.Added = m.cfg.Devices[i].Added
				m.cfg.Devices[i] = d
				found = true
			}
		}
		if !found {
			m.cfg.Devices = append(m.cfg.Devices, d)
		}
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "id": d.Token[:12]})
	case http.MethodDelete:
		ep := r.URL.Query().Get("endpoint")
		h := sha256.Sum256([]byte(ep))
		tok := "web-" + hex.EncodeToString(h[:])[:40]
		org := principal(r).Org
		m.mu.Lock()
		out := m.cfg.Devices[:0]
		for _, d := range m.cfg.Devices {
			if !(d.Token == tok && d.Org == org) {
				out = append(out, d)
			}
		}
		m.cfg.Devices = out
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

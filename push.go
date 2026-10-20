package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Device ist ein registriertes iPhone/iPad der PingWatch-App.
type Device struct {
	Token    string   `json:"token"`
	Org      string   `json:"org,omitempty"`
	User     string   `json:"user,omitempty"`
	AllOrgs  bool     `json:"allOrgs,omitempty"` // Betreiber: Meldungen aller Kunden
	Name     string   `json:"name"`
	Sandbox  bool     `json:"sandbox"`
	Outage   bool     `json:"outage"`  // Ausfall + wieder erreichbar
	Loss     bool     `json:"loss"`    // Paketverlust
	Spike    bool     `json:"spike"`   // Latenzspitzen
	Offline  bool     `json:"offline"` // Standort offline / wieder online
	Hosts    []string `json:"hosts"`   // leer = alle
	Added    int64    `json:"added"`
	LastSent int64    `json:"lastSent"`
}

type notice struct {
	kind   string // outage, recovered, loss, spike, offline, online
	hostID string
	title  string
	body   string
	org    string
}

// Pusher versendet Benachrichtigungen über Apple Push Notification service (APNs).
type Pusher struct {
	keyID, teamID, topic string
	key                  *ecdsa.PrivateKey
	client               *http.Client
	mu                   sync.Mutex
	jwt                  string
	jwtAt                time.Time
	throttle             map[string]time.Time
}

// newPusherFromEnv liest APNS_KEY (Inhalt der .p8-Datei, auch Base64) oder APNS_KEY_FILE,
// APNS_KEY_ID, APNS_TEAM_ID und APNS_TOPIC (Bundle-ID der App).
func newPusherFromEnv() (*Pusher, error) {
	keyID, teamID := os.Getenv("APNS_KEY_ID"), os.Getenv("APNS_TEAM_ID")
	raw := os.Getenv("APNS_KEY")
	if raw == "" && os.Getenv("APNS_KEY_FILE") != "" {
		b, err := os.ReadFile(os.Getenv("APNS_KEY_FILE"))
		if err != nil {
			return nil, err
		}
		raw = string(b)
	}
	if raw == "" || keyID == "" || teamID == "" {
		return nil, nil // nicht konfiguriert
	}
	key, err := parseP8(raw)
	if err != nil {
		return nil, fmt.Errorf("APNS_KEY: %v", err)
	}
	return &Pusher{keyID: keyID, teamID: teamID, topic: envOr("APNS_TOPIC", "de.rst.pingwatch"), key: key,
		client: &http.Client{Timeout: 15 * time.Second}, throttle: map[string]time.Time{}}, nil
}

func parseP8(raw string) (*ecdsa.PrivateKey, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, `\n`, "\n")
	if !strings.Contains(raw, "BEGIN") {
		// Base64 der ganzen Datei oder nur der Schlüsselteil
		if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
			if strings.Contains(string(b), "BEGIN") {
				raw = string(b)
			} else {
				raw = "-----BEGIN PRIVATE KEY-----\n" + raw + "\n-----END PRIVATE KEY-----"
			}
		}
	}
	// Zeilenumbrüche können beim Einfügen in Umgebungsvariablen verloren gehen
	if !strings.Contains(raw, "\n") {
		raw = strings.Replace(raw, "-----BEGIN PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----\n", 1)
		raw = strings.Replace(raw, "-----END PRIVATE KEY-----", "\n-----END PRIVATE KEY-----", 1)
		parts := strings.SplitN(raw, "\n", 3)
		if len(parts) == 3 {
			parts[1] = strings.ReplaceAll(parts[1], " ", "\n")
			raw = strings.Join(parts, "\n")
		}
	}
	blk, _ := pem.Decode([]byte(raw))
	if blk == nil {
		return nil, errors.New("kein gültiger PEM-Schlüssel")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("kein EC-Schlüssel")
	}
	return ek, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (p *Pusher) token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jwt != "" && time.Since(p.jwtAt) < 45*time.Minute {
		return p.jwt, nil
	}
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": p.keyID})
	now := time.Now()
	cl, _ := json.Marshal(map[string]any{"iss": p.teamID, "iat": now.Unix()})
	unsigned := b64url(hdr) + "." + b64url(cl)
	h := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, p.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	p.jwt, p.jwtAt = unsigned+"."+b64url(sig), now
	return p.jwt, nil
}

var errBadToken = errors.New("Gerätetoken ungültig")

// apnsBase überschreibt den Apple-Server (nur für Tests).
var apnsBase = ""

func (p *Pusher) send(d Device, n notice) error {
	jwt, err := p.token()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"aps": map[string]any{
			"alert":     map[string]string{"title": n.title, "body": n.body},
			"sound":     "default",
			"thread-id": n.hostID,
		},
		"host": n.hostID,
		"kind": n.kind,
	}
	if n.kind == "outage" || n.kind == "offline" {
		payload["aps"].(map[string]any)["interruption-level"] = "time-sensitive"
	}
	body, _ := json.Marshal(payload)
	host := "https://api.push.apple.com"
	if d.Sandbox {
		host = "https://api.sandbox.push.apple.com"
	}
	if apnsBase != "" {
		host = apnsBase
	}
	req, _ := http.NewRequest("POST", host+"/3/device/"+d.Token, bytes.NewReader(body))
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", p.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	var r struct {
		Reason string `json:"reason"`
	}
	json.Unmarshal(b, &r)
	if resp.StatusCode == 410 || r.Reason == "BadDeviceToken" || r.Reason == "Unregistered" {
		return errBadToken
	}
	if r.Reason == "ExpiredProviderToken" {
		p.mu.Lock()
		p.jwt = ""
		p.mu.Unlock()
	}
	return fmt.Errorf("APNs %d %s", resp.StatusCode, r.Reason)
}

func (d Device) wants(n notice) bool {
	switch n.kind {
	case "outage", "recovered":
		if !d.Outage {
			return false
		}
	case "loss":
		if !d.Loss {
			return false
		}
	case "spike":
		if !d.Spike {
			return false
		}
	case "offline", "online":
		return d.Offline
	}
	if len(d.Hosts) == 0 {
		return true
	}
	for _, h := range d.Hosts {
		if h == n.hostID {
			return true
		}
	}
	return false
}

// ---- Anbindung an den Monitor ----

func (m *Monitor) notify(n notice) {
	if m.notices == nil {
		return
	}
	select {
	case m.notices <- n:
	default:
	}
}

func (m *Monitor) pushLoop() {
	for n := range m.notices {
		m.dispatchAlerts(n)
		if m.pusher == nil {
			continue
		}
		m.mu.RLock()
		devs := append([]Device(nil), m.cfg.Devices...)
		m.mu.RUnlock()
		var bad []string
		for _, d := range devs {
			if !d.AllOrgs && d.Org != n.org {
				continue
			}
			if !d.wants(n) {
				continue
			}
			// Paketverlust und Spitzen höchstens alle 10 Minuten pro Host und Gerät
			if n.kind == "loss" || n.kind == "spike" {
				k := d.Token + "|" + n.hostID + "|" + n.kind
				m.pusher.mu.Lock()
				last := m.pusher.throttle[k]
				if time.Since(last) < 10*time.Minute {
					m.pusher.mu.Unlock()
					continue
				}
				m.pusher.throttle[k] = time.Now()
				m.pusher.mu.Unlock()
			}
			if err := m.pusher.send(d, n); err != nil {
				if errors.Is(err, errBadToken) {
					bad = append(bad, d.Token)
				}
				log.Printf("Push an %s: %v", d.Name, err)
			}
		}
		if len(bad) > 0 {
			m.mu.Lock()
			out := m.cfg.Devices[:0]
			for _, d := range m.cfg.Devices {
				keep := true
				for _, t := range bad {
					if d.Token == t {
						keep = false
					}
				}
				if keep {
					out = append(out, d)
				}
			}
			m.cfg.Devices = out
			m.saveConfigLocked()
			m.mu.Unlock()
		}
	}
}

func fmtDurShort(ms int64) string {
	s := ms / 1000
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		return fmt.Sprintf("%d min", s/60)
	case s < 86400:
		return fmt.Sprintf("%d h %d min", s/3600, (s%3600)/60)
	}
	return fmt.Sprintf("%d T %d h", s/86400, (s%86400)/3600)
}

func (m *Monitor) siteNameOf(id string) string {
	if id == "cloud" {
		return "Cloud"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.cfg.Sites {
		if s.ID == id {
			return s.Name
		}
	}
	return ""
}

func (m *Monitor) notifyEventStart(e Event) {
	if e.quiet() {
		return
	}
	at := time.UnixMilli(e.Start).Format("15:04:05")
	switch e.Type {
	case "outage":
		m.notify(notice{"outage", e.HostID, "🔴 " + e.HostName + " antwortet nicht",
			fmt.Sprintf("Seit %s keine Antwort (%s)", at, m.siteNameOf(e.Site)), e.Org})
	case "offline":
		m.notify(notice{"offline", "", "⚪️ Standort " + e.HostName + " offline",
			fmt.Sprintf("Agent meldet sich seit %s nicht mehr", at), e.Org})
	}
}

func (m *Monitor) notifyEventEnd(e Event) {
	if e.quiet() {
		return
	}
	dur := fmtDurShort(e.End - e.Start)
	switch e.Type {
	case "outage":
		if !e.Notified {
			return
		}
		m.notify(notice{"recovered", e.HostID, "🟢 " + e.HostName + " wieder erreichbar",
			fmt.Sprintf("Ausfall %s – %s (%s)", time.UnixMilli(e.Start).Format("15:04"), time.UnixMilli(e.End).Format("15:04"), dur), e.Org})
	case "loss":
		m.notify(notice{"loss", e.HostID, "🟠 Paketverlust bei " + e.HostName,
			fmt.Sprintf("%d Pings verloren um %s", e.Count, time.UnixMilli(e.Start).Format("15:04")), e.Org})
	case "spike":
		avg := int64(0)
		if e.Count > 0 {
			avg = e.Sum / int64(e.Count)
		}
		m.notify(notice{"spike", e.HostID, "🟡 Hohe Antwortzeit bei " + e.HostName,
			fmt.Sprintf("%d× über Schwelle, max %s ms, Ø %s ms (%s)", e.Count, fmtR(e.Max), fmtR(int32(avg)), time.UnixMilli(e.Start).Format("15:04")), e.Org})
	case "offline":
		m.notify(notice{"online", "", "🟢 Standort " + e.HostName + " wieder online",
			fmt.Sprintf("Agent war %s offline", dur), e.Org})
	}
}

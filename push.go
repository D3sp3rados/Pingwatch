package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Device ist ein registriertes iPhone/iPad der PingWacht-App.
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
	// Web-Push (Browser/Android): Kind = "web"
	Kind     string `json:"kind,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	P256dh   string `json:"p256dh,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

type notice struct {
	kind   string // outage, recovered, loss, spike, offline, online
	hostID string
	title  string
	body   string
	org    string
	site   string // Standort bei Meldungen ohne Host (Agent offline, UniFi) – für Kunden-Filter
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
		if raw == "" && keyID == "" && teamID == "" {
			return nil, nil // nicht konfiguriert
		}
		var miss []string
		if raw == "" {
			miss = append(miss, "APNS_KEY")
		}
		if keyID == "" {
			miss = append(miss, "APNS_KEY_ID")
		}
		if teamID == "" {
			miss = append(miss, "APNS_TEAM_ID")
		}
		return nil, fmt.Errorf("es fehlt: %s", strings.Join(miss, ", "))
	}
	key, err := parseP8(raw)
	if err != nil {
		return nil, fmt.Errorf("APNS_KEY: %v", err)
	}
	return &Pusher{keyID: keyID, teamID: teamID, topic: envOr("APNS_TOPIC", "de.rst.pingwatch"), key: key,
		client: &http.Client{Timeout: 15 * time.Second}, throttle: map[string]time.Time{}}, nil
}

func parseP8(raw string) (*ecdsa.PrivateKey, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, `\n`, "\n"))
	// ganze Datei als Base64?
	if !strings.Contains(raw, "BEGIN") {
		if b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(raw), "")); err == nil && strings.Contains(string(b), "BEGIN") {
			raw = string(b)
		}
	}
	// Inhalt zwischen BEGIN/END herauslösen – Zeilenumbrüche, Leerzeichen oder Anführungszeichen egal
	body := raw
	if i := strings.Index(body, "BEGIN"); i >= 0 {
		if k := strings.Index(body[i:], "-----"); k >= 0 {
			body = body[i+k+5:]
		}
	}
	if i := strings.Index(body, "-----END"); i >= 0 {
		body = body[:i]
	} else if i := strings.Index(body, "END PRIVATE"); i >= 0 {
		body = body[:i]
	}
	var b strings.Builder
	for _, r := range body {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	der, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("kein gültiger Schlüssel (Base64-Fehler, %d Zeichen erkannt)", len(clean))
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("Schlüssel unvollständig oder falsch kopiert (%d Zeichen erkannt, eine .p8-Datei hat ca. 184) – bitte den kompletten Inhalt der .p8-Datei einfügen", len(clean))
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("kein EC-Schlüssel – bitte den APNs-Schlüssel (.p8) verwenden")
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
	case "cert":
		if !d.Outage {
			return false
		}
	case "portdown", "portup":
		if !d.Outage {
			return false
		}
	case "devoff", "devon", "reboot", "wandown", "wanup", "fritzdev", "fritzfw":
		return d.Outage || d.Offline
	case "syswarn", "sysok", "sysoff", "syson":
		return d.Outage || d.Offline
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

var pushThrottle = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

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
		m.personalMails(n)
		m.mu.RLock()
		devs := append([]Device(nil), m.cfg.Devices...)
		group := m.hostGroupLocked(n.hostID)
		prefs := map[string]*NotifyPrefs{}
		users := map[string]User{}
		for _, u := range m.cfg.Users {
			users[u.ID] = u
			if u.Notify != nil {
				cp := *u.Notify
				prefs[u.ID] = &cp
			}
		}
		m.mu.RUnlock()
		seen := map[string]bool{} // Benutzer → darf Meldung sehen (einmal je Meldung berechnet)
		visible := func(uid string) bool {
			u, ok := users[uid]
			if !ok {
				return true
			}
			if v, ok := seen[uid]; ok {
				return v
			}
			v := m.noticeVisible(&u, n)
			seen[uid] = v
			return v
		}
		now := time.Now().UnixMilli()
		var bad []string
		for _, d := range devs {
			if !d.AllOrgs && d.Org != n.org {
				continue
			}
			if p := prefs[d.User]; p != nil && (p.NoPush || p.suppressed(group, now)) {
				continue
			}
			if !d.AllOrgs && !visible(d.User) {
				continue
			}
			if !d.wants(n) {
				continue
			}
			if d.Kind != "web" && m.pusher == nil {
				continue
			}
			// Paketverlust und Spitzen höchstens alle 10 Minuten pro Host und Gerät
			if n.kind == "loss" || n.kind == "spike" {
				k := d.Token + "|" + n.hostID + "|" + n.kind
				pushThrottle.Lock()
				last := pushThrottle.m[k]
				if time.Since(last) < 10*time.Minute {
					pushThrottle.Unlock()
					continue
				}
				pushThrottle.m[k] = time.Now()
				pushThrottle.Unlock()
			}
			var err error
			if d.Kind == "web" {
				err = m.sendWebPush(d, n)
			} else {
				err = m.pusher.send(d, n)
			}
			if err != nil {
				if errors.Is(err, errBadToken) || errors.Is(err, errGone) {
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

// hostInfo: „IP · Kunde“ und „Geprüft von …“ für die Meldungstexte.
func (m *Monitor) hostInfo(hostID, site string) (head, by string) {
	m.mu.RLock()
	var addr, group string
	for _, h := range m.cfg.Hosts {
		if h.ID == hostID {
			addr = h.Addr
			if h.Method == "tcp" && h.Port > 0 {
				addr += ":" + strconv.Itoa(h.Port)
			}
			for _, g := range m.cfg.Groups {
				if g.ID == h.Group {
					group = g.Name
				}
			}
			if site == "" {
				site = h.Site
			}
		}
	}
	m.mu.RUnlock()
	head = addr
	if group != "" {
		if head != "" {
			head += " · "
		}
		head += "Kunde " + group
	}
	if site == "cloud" || site == "" {
		by = "Geprüft von: Cloud-Server"
	} else if n := m.siteNameOf(site); n != "" {
		by = "Geprüft von: Agent " + n
	}
	return
}

// withInfo setzt Adresse/Kunde vor und „Geprüft von“ unter den Text.
func (m *Monitor) withInfo(hostID, site, text string) string {
	head, by := m.hostInfo(hostID, site)
	out := text
	if head != "" {
		out = head + "\n" + out
	}
	if by != "" {
		out += "\n" + by
	}
	return out
}

func (m *Monitor) notifyEventStart(e Event) {
	if e.quiet() {
		return
	}
	at := time.UnixMilli(e.Start).Format("15:04:05")
	switch e.Type {
	case "outage":
		m.notify(notice{"outage", e.HostID, "🔴 " + e.HostName + " antwortet nicht",
			m.withInfo(e.HostID, e.Site, "Seit "+at+" keine Antwort"), e.Org, e.Site})
	case "offline":
		m.notify(notice{"offline", "", "⚪️ Agent " + e.HostName + " nicht erreichbar",
			fmt.Sprintf("Der Agent meldet sich seit %s nicht mehr (PC aus, kein Internet oder Dienst gestoppt?).\n%d Hosts dieses Standorts werden solange nicht geprüft.", at, m.siteHostCount(e.Site)), e.Org, e.Site})
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
			m.withInfo(e.HostID, e.Site, fmt.Sprintf("Ausfall %s – %s (%s)", time.UnixMilli(e.Start).Format("15:04"), time.UnixMilli(e.End).Format("15:04"), dur)), e.Org, e.Site})
	case "loss":
		m.notify(notice{"loss", e.HostID, "🟠 Paketverlust bei " + e.HostName,
			m.withInfo(e.HostID, e.Site, fmt.Sprintf("%d Pings verloren um %s", e.Count, time.UnixMilli(e.Start).Format("15:04"))), e.Org, e.Site})
	case "spike":
		avg := int64(0)
		if e.Count > 0 {
			avg = e.Sum / int64(e.Count)
		}
		m.notify(notice{"spike", e.HostID, "🟡 Hohe Antwortzeit bei " + e.HostName,
			m.withInfo(e.HostID, e.Site, fmt.Sprintf("%d× über Schwelle, max %s ms, Ø %s ms (%s)", e.Count, fmtR(e.Max), fmtR(int32(avg)), time.UnixMilli(e.Start).Format("15:04"))), e.Org, e.Site})
	case "offline":
		m.notify(notice{"online", "", "🟢 Agent " + e.HostName + " wieder online",
			fmt.Sprintf("Der Agent war %s nicht erreichbar, die Prüfungen laufen wieder.", dur), e.Org, e.Site})
	}
}

func (m *Monitor) siteHostCount(site string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, h := range m.cfg.Hosts {
		if h.Site == site && !h.Paused {
			n++
		}
	}
	return n
}

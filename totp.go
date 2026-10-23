package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// qrDataURL: QR-Code als PNG-Data-URL (zum Abscannen mit der Authenticator-App).
func qrDataURL(s string) string {
	png, err := qrcode.Encode(s, qrcode.Medium, 220)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// Zwei-Faktor-Code (TOTP, RFC 6238) – Pflicht für Updates und Neustarts über PingWacht.
// Jede Aktion verlangt einen frischen Code aus der Authenticator-App (Microsoft/Google Authenticator …).

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

func totpAt(secret string, counter uint64) string {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return ""
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	s := h.Sum(nil)
	o := s[len(s)-1] & 0x0f
	v := binary.BigEndian.Uint32(s[o:o+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// totpState: Sperre nach Fehlversuchen und Schutz gegen doppelt verwendete Codes je Anmeldung.
var totpState = struct {
	sync.Mutex
	fails map[string]int
	until map[string]time.Time
	used  map[string]uint64
}{fails: map[string]int{}, until: map[string]time.Time{}, used: map[string]uint64{}}

// totpCheck prüft einen Code (±30 s). subject = Benutzer-ID bzw. "admin".
func totpCheck(subject, secret, code string, now time.Time) error {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	totpState.Lock()
	defer totpState.Unlock()
	if now.Before(totpState.until[subject]) {
		return fmt.Errorf("Zu viele falsche Codes – bitte in %d Minuten erneut versuchen", int(time.Until(totpState.until[subject]).Minutes())+1)
	}
	if secret == "" || len(code) != 6 {
		return fmt.Errorf("Bitte den 6-stelligen Code aus der Authenticator-App eingeben")
	}
	c := uint64(now.Unix() / 30)
	for _, d := range []int64{0, -1, 1} {
		cc := uint64(int64(c) + d)
		if hmac.Equal([]byte(totpAt(secret, cc)), []byte(code)) {
			if cc <= totpState.used[subject] {
				return fmt.Errorf("Dieser Code wurde schon verwendet – bitte den nächsten abwarten")
			}
			totpState.used[subject] = cc
			totpState.fails[subject] = 0
			return nil
		}
	}
	totpState.fails[subject]++
	if totpState.fails[subject] >= 5 {
		totpState.fails[subject] = 0
		totpState.until[subject] = now.Add(5 * time.Minute)
	}
	return fmt.Errorf("Code falsch")
}

// totpOf liefert Kennung, aktives Geheimnis und (bei Einrichtung) das vorläufige Geheimnis.
func (m *Monitor) totpOf(p *Principal) (subject, secret, pending string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p.User == nil {
		return "admin", m.cfg.AdminTOTP, m.cfg.AdminTOTPPending
	}
	for _, u := range m.cfg.Users {
		if u.ID == p.User.ID {
			return u.ID, u.TOTP, u.TOTPPending
		}
	}
	return p.User.ID, "", ""
}

func (m *Monitor) setTOTP(p *Principal, secret, pending string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.User == nil {
		m.cfg.AdminTOTP, m.cfg.AdminTOTPPending = secret, pending
	} else {
		for i := range m.cfg.Users {
			if m.cfg.Users[i].ID == p.User.ID {
				m.cfg.Users[i].TOTP, m.cfg.Users[i].TOTPPending = secret, pending
			}
		}
	}
	m.saveConfigLocked()
}

// GET /api/totp → {on} · POST {action: setup|enable|disable, code}
func (s *Server) handleTOTP(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	sub, sec, pend := s.m.totpOf(p)
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]bool{"on": sec != ""})
		return
	}
	var req struct{ Action, Code string }
	if !readJSON(w, r, &req) {
		return
	}
	switch req.Action {
	case "setup":
		if sec != "" {
			http.Error(w, "Zwei-Faktor-Anmeldung ist schon eingerichtet", 409)
			return
		}
		ns := newTOTPSecret()
		s.m.setTOTP(p, "", ns)
		acct := "admin"
		if p.User != nil {
			acct = p.User.Email
		}
		uri := "otpauth://totp/" + url.PathEscape("PingWacht:"+acct) + "?secret=" + ns + "&issuer=PingWacht&digits=6&period=30"
		writeJSON(w, map[string]string{"secret": ns, "uri": uri, "qr": qrDataURL(uri)})
	case "enable":
		if pend == "" {
			http.Error(w, "Bitte zuerst „Einrichten“ wählen", 400)
			return
		}
		if err := totpCheck(sub, pend, req.Code, time.Now()); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		s.m.setTOTP(p, pend, "")
		writeJSON(w, map[string]bool{"ok": true})
	case "disable":
		if sec == "" {
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if err := totpCheck(sub, sec, req.Code, time.Now()); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		s.m.setTOTP(p, "", "")
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Aktion unbekannt", 400)
	}
}

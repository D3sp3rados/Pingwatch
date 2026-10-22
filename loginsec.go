package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Anmelde-Absicherung: Sperre nach Fehlversuchen, Zwei-Faktor-Code beim Login (wenn eingerichtet)
// und E-Mail bei Anmeldung von einem neuen Gerät.

var errNeed2FA = errors.New("Zwei-Faktor-Code erforderlich")

var loginGuard = struct {
	sync.Mutex
	fails map[string][]time.Time
	until map[string]time.Time
}{fails: map[string][]time.Time{}, until: map[string]time.Time{}}

const (
	loginMaxPerAccount = 5  // Fehlversuche je Konto in 15 Minuten
	loginMaxPerIP      = 30 // Fehlversuche je Anschluss in 15 Minuten
	loginLock          = 15 * time.Minute
)

func loginBlocked(keys ...string) error {
	loginGuard.Lock()
	defer loginGuard.Unlock()
	for _, k := range keys {
		if u := loginGuard.until[k]; time.Now().Before(u) {
			return fmt.Errorf("Zu viele Fehlversuche – Anmeldung für %d Minuten gesperrt", int(time.Until(u).Minutes())+1)
		}
	}
	return nil
}

func loginFailed(account, ip string) {
	loginGuard.Lock()
	defer loginGuard.Unlock()
	now := time.Now()
	for _, x := range []struct {
		k   string
		max int
	}{{"a:" + account, loginMaxPerAccount}, {"ip:" + ip, loginMaxPerIP}} {
		l := loginGuard.fails[x.k][:0:0]
		for _, t := range loginGuard.fails[x.k] {
			if now.Sub(t) < loginLock {
				l = append(l, t)
			}
		}
		l = append(l, now)
		loginGuard.fails[x.k] = l
		if len(l) >= x.max {
			loginGuard.until[x.k] = now.Add(loginLock)
			loginGuard.fails[x.k] = nil
			log.Printf("Anmeldung gesperrt (%s) nach %d Fehlversuchen", x.k, len(l))
		}
	}
}

func loginSucceeded(account string) {
	loginGuard.Lock()
	delete(loginGuard.fails, "a:"+account)
	loginGuard.Unlock()
}

// loginSecure: Passwort, Sperre und Zwei-Faktor-Code. device = Kennung des Geräts (für die Neu-Anmelde-Mail).
func (s *Server) loginSecure(email, pw, code, device string, r *http.Request) (string, *User, error) {
	acct := strings.ToLower(strings.TrimSpace(email))
	if acct == "" {
		acct = "admin"
	}
	ip := clientIP(r)
	if err := loginBlocked("a:"+acct, "ip:"+ip); err != nil {
		return "", nil, err
	}
	// Zwei-Faktor-Code darf in Apps ohne eigenes Feld direkt ans Passwort angehängt werden
	tok, u, err := s.login(email, pw)
	if err != nil && code == "" && len(pw) > 6 && isDigits(pw[len(pw)-6:]) {
		if t2, u2, e2 := s.login(email, pw[:len(pw)-6]); e2 == nil {
			tok, u, err, code = t2, u2, nil, pw[len(pw)-6:]
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "falsch") {
			loginFailed(acct, ip)
		}
		return "", nil, err
	}
	sub, sec := "admin", ""
	s.m.mu.RLock()
	if u == nil {
		sec = s.m.cfg.AdminTOTP
	} else {
		for _, x := range s.m.cfg.Users {
			if x.ID == u.ID {
				sub, sec = x.ID, x.TOTP
			}
		}
	}
	s.m.mu.RUnlock()
	if sec != "" {
		if strings.TrimSpace(code) == "" {
			return "", u, errNeed2FA
		}
		if err := totpCheck(sub, sec, code, time.Now()); err != nil {
			loginFailed(acct, ip)
			return "", u, err
		}
	}
	loginSucceeded(acct)
	if u != nil {
		s.noteLoginDevice(u, device, r)
	}
	return tok, u, nil
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// noteLoginDevice merkt sich das Gerät; bei einem neuen Gerät (nicht beim allerersten) gibt es eine E-Mail.
func (s *Server) noteLoginDevice(u *User, device string, r *http.Request) {
	if device == "" {
		return
	}
	h := sha256.Sum256([]byte(u.ID + "|" + device))
	key := hex.EncodeToString(h[:8])
	isNew, first := false, false
	s.m.mu.Lock()
	for i := range s.m.cfg.Users {
		x := &s.m.cfg.Users[i]
		if x.ID != u.ID {
			continue
		}
		found := false
		for _, d := range x.Devices {
			found = found || d == key
		}
		if !found {
			first = len(x.Devices) == 0
			x.Devices = append(x.Devices, key)
			if len(x.Devices) > 20 {
				x.Devices = x.Devices[len(x.Devices)-20:]
			}
			isNew = true
			s.m.saveConfigLocked()
		}
	}
	s.m.mu.Unlock()
	if !isNew || first {
		return
	}
	ua := r.UserAgent()
	if len(ua) > 160 {
		ua = ua[:160]
	}
	ip := clientIP(r)
	go trialMail([]string{u.Email}, "PingWacht: Anmeldung von einem neuen Gerät",
		fmt.Sprintf("Guten Tag,\n\nIhr PingWacht-Konto %s wurde soeben von einem neuen Gerät aus angemeldet:\n\nZeit: %s\nIP-Adresse: %s\nGerät/Browser: %s\n\nWaren Sie das? Dann ist alles in Ordnung.\nFalls nicht: Bitte sofort unter %s/#/settings/konto das Passwort ändern und die Zwei-Faktor-Anmeldung einschalten – und uns per Antwort auf diese E-Mail informieren.\n",
			u.Email, time.Now().Format("02.01.2006 15:04"), ip, ua, publicBase(r)))
}

// loginDevice: Geräte-Kennung aus Cookie (Browser) bzw. Kopfzeile (App); setzt bei Bedarf ein neues Cookie.
func loginDevice(w http.ResponseWriter, r *http.Request) string {
	if d := r.Header.Get("X-Device"); d != "" {
		return "app:" + d
	}
	if c, err := r.Cookie("pw_dev"); err == nil && len(c.Value) >= 16 {
		return c.Value
	}
	d := randHex(16)
	http.SetCookie(w, &http.Cookie{Name: "pw_dev", Value: d, Path: "/", MaxAge: 5 * 365 * 24 * 3600, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	return d
}

// ---- zweiter Schritt im Browser: nach richtigem Passwort ein kurzlebiges Cookie, dann nur noch der Code ----

func (s *Server) set2FAPending(w http.ResponseWriter, r *http.Request, email string) {
	exp := time.Now().Add(5 * time.Minute).Unix()
	payload := email + "|" + fmt.Sprint(exp)
	// das Passwort selbst wird nicht gespeichert; die Signatur bindet den Zwischenschritt an den
	// aktuellen Passwort-Hash des Kontos (Passwortwechsel macht ihn ungültig)
	ref := s.pwRefFor(email)
	tok := payload + "|" + s.sign("2fa|"+payload+"|"+ref)
	http.SetCookie(w, &http.Cookie{Name: "pw_2fa", Value: tok, Path: "/login", MaxAge: 300, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode})
}

// pending2FA: E-Mail aus dem Zwischenschritt-Cookie (gültig 5 Minuten).
func (s *Server) pending2FA(r *http.Request) string {
	c, err := r.Cookie("pw_2fa")
	if err != nil {
		return ""
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return ""
	}
	var exp int64
	fmt.Sscan(parts[1], &exp)
	if time.Now().Unix() > exp {
		return ""
	}
	payload := parts[0] + "|" + parts[1]
	if s.sign("2fa|"+payload+"|"+s.pwRefFor(parts[0])) != parts[2] {
		return ""
	}
	return parts[0]
}

func (s *Server) pwRefFor(email string) string {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" || e == "admin" {
		return s.adminRef()
	}
	if u := s.m.userByEmail(e); u != nil {
		return userRef(u)
	}
	return "-"
}

// finish2FA: zweiter Schritt – Code zum gemerkten Konto prüfen.
func (s *Server) finish2FA(email, code, device string, r *http.Request) (string, error) {
	acct := strings.ToLower(strings.TrimSpace(email))
	if acct == "" {
		acct = "admin"
	}
	ip := clientIP(r)
	if err := loginBlocked("a:"+acct, "ip:"+ip); err != nil {
		return "", err
	}
	var u *User
	sub, sec, tok := "admin", "", ""
	if acct == "admin" {
		s.m.mu.RLock()
		sec = s.m.cfg.AdminTOTP
		s.m.mu.RUnlock()
		tok = s.makeToken("admin", s.adminRef())
	} else {
		u = s.m.userByEmail(acct)
		if u == nil {
			return "", errors.New("Anmeldung abgelaufen – bitte erneut anmelden")
		}
		if err := accessError(u, s.m.orgByID(u.Org)); err != nil {
			return "", err
		}
		sub, sec, tok = u.ID, u.TOTP, s.makeToken(u.ID, userRef(u))
	}
	if sec == "" {
		return "", errors.New("Anmeldung abgelaufen – bitte erneut anmelden")
	}
	if err := totpCheck(sub, sec, code, time.Now()); err != nil {
		loginFailed(acct, ip)
		return "", err
	}
	loginSucceeded(acct)
	if u != nil {
		s.m.mu.Lock()
		for i := range s.m.cfg.Users {
			if s.m.cfg.Users[i].ID == u.ID {
				s.m.cfg.Users[i].LastLogin = time.Now().UnixMilli()
			}
		}
		s.m.saveConfigLocked()
		s.m.mu.Unlock()
		s.noteLoginDevice(u, device, r)
	}
	return tok, nil
}

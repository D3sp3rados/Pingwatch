package main

// Passkeys (WebAuthn): Anmelden ohne Passwort und Bestätigen von Updates/Neustarts/Scripts
// statt mit dem Authenticator-Code. Browser (navigator.credentials) und iPhone-App
// (AuthenticationServices) nutzen dieselben Endpunkte.
//
// Die Relying-Party-ID ist der Hostname (oder PASSKEY_RPID, z. B. app.pingwacht.de).

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Passkey struct {
	ID      string `json:"id"`  // Credential-ID, base64url
	Key     []byte `json:"key"` // COSE-Schlüssel
	Count   uint32 `json:"count,omitempty"`
	Name    string `json:"name,omitempty"`
	Created int64  `json:"created"`
	Last    int64  `json:"last,omitempty"`
}

var b64u = base64.RawURLEncoding

func b64dec(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return b64u.DecodeString(s)
}

// ---------- CBOR (nur was WebAuthn braucht) ----------

func cborDecode(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errors.New("cbor: leer")
	}
	major, info := b[0]>>5, b[0]&0x1f
	b = b[1:]
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info == 24:
		if len(b) < 1 {
			return nil, nil, errors.New("cbor: kurz")
		}
		n, b = uint64(b[0]), b[1:]
	case info == 25:
		if len(b) < 2 {
			return nil, nil, errors.New("cbor: kurz")
		}
		n, b = uint64(binary.BigEndian.Uint16(b)), b[2:]
	case info == 26:
		if len(b) < 4 {
			return nil, nil, errors.New("cbor: kurz")
		}
		n, b = uint64(binary.BigEndian.Uint32(b)), b[4:]
	case info == 27:
		if len(b) < 8 {
			return nil, nil, errors.New("cbor: kurz")
		}
		n, b = binary.BigEndian.Uint64(b), b[8:]
	default:
		return nil, nil, errors.New("cbor: Länge nicht unterstützt")
	}
	switch major {
	case 0:
		return int64(n), b, nil
	case 1:
		return -1 - int64(n), b, nil
	case 2, 3:
		if uint64(len(b)) < n {
			return nil, nil, errors.New("cbor: kurz")
		}
		if major == 2 {
			return append([]byte(nil), b[:n]...), b[n:], nil
		}
		return string(b[:n]), b[n:], nil
	case 4:
		arr := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			v, rest, err := cborDecode(b)
			if err != nil {
				return nil, nil, err
			}
			arr, b = append(arr, v), rest
		}
		return arr, b, nil
	case 5:
		m := map[any]any{}
		for i := uint64(0); i < n; i++ {
			k, rest, err := cborDecode(b)
			if err != nil {
				return nil, nil, err
			}
			v, rest2, err := cborDecode(rest)
			if err != nil {
				return nil, nil, err
			}
			m[k], b = v, rest2
		}
		return m, b, nil
	case 7:
		switch info {
		case 20:
			return false, b, nil
		case 21:
			return true, b, nil
		case 22:
			return nil, b, nil
		}
	}
	return nil, nil, errors.New("cbor: Typ nicht unterstützt")
}

// ---------- Schlüssel ----------

func coseVerify(key []byte, data, sig []byte) error {
	v, _, err := cborDecode(key)
	if err != nil {
		return err
	}
	m, ok := v.(map[any]any)
	if !ok {
		return errors.New("COSE-Schlüssel ungültig")
	}
	h := sha256.Sum256(data)
	switch m[int64(3)] {
	case int64(-7): // ES256
		x, _ := m[int64(-2)].([]byte)
		y, _ := m[int64(-3)].([]byte)
		if len(x) != 32 || len(y) != 32 {
			return errors.New("EC-Schlüssel ungültig")
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !ecdsa.VerifyASN1(pub, h[:], sig) {
			return errors.New("Signatur ungültig")
		}
		return nil
	case int64(-257): // RS256
		nb, _ := m[int64(-1)].([]byte)
		eb, _ := m[int64(-2)].([]byte)
		if len(nb) < 128 || len(eb) == 0 {
			return errors.New("RSA-Schlüssel ungültig")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig) != nil {
			return errors.New("Signatur ungültig")
		}
		return nil
	}
	return errors.New("Schlüsselart nicht unterstützt")
}

type authData struct {
	rpHash [32]byte
	flags  byte
	count  uint32
	credID []byte
	key    []byte // COSE, nur bei der Registrierung
}

func parseAuthData(b []byte) (*authData, error) {
	if len(b) < 37 {
		return nil, errors.New("authenticatorData zu kurz")
	}
	a := &authData{flags: b[32], count: binary.BigEndian.Uint32(b[33:37])}
	copy(a.rpHash[:], b[:32])
	if a.flags&0x40 != 0 { // attested credential data
		r := b[37:]
		if len(r) < 18 {
			return nil, errors.New("Credential-Daten zu kurz")
		}
		l := int(binary.BigEndian.Uint16(r[16:18]))
		r = r[18:]
		if len(r) < l {
			return nil, errors.New("Credential-ID zu kurz")
		}
		a.credID = append([]byte(nil), r[:l]...)
		r = r[l:]
		_, rest, err := cborDecode(r)
		if err != nil {
			return nil, err
		}
		a.key = append([]byte(nil), r[:len(r)-len(rest)]...)
	}
	return a, nil
}

// ---------- Relying Party ----------

func passkeyRPID(r *http.Request) string {
	if v := strings.TrimSpace(os.Getenv("PASSKEY_RPID")); v != "" {
		return v
	}
	h := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		h = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(h)
}

func originOK(origin, rpID string) bool {
	if origin == "https://"+rpID || strings.HasPrefix(origin, "https://"+rpID+":") {
		return true
	}
	// Entwicklung: http://localhost / 127.0.0.1
	return (rpID == "localhost" || rpID == "127.0.0.1") && strings.HasPrefix(origin, "http://"+rpID)
}

type pkChallenge struct {
	purpose string // register, login, stepup
	subject string // Benutzer-ID oder "admin" (nicht bei login)
	rpID    string
	exp     time.Time
}

var pkState = struct {
	sync.Mutex
	ch     map[string]pkChallenge
	stepup map[string]time.Time // subject → Bestätigung per Passkey (einmal verwendbar)
}{ch: map[string]pkChallenge{}, stepup: map[string]time.Time{}}

func newChallenge(purpose, subject, rpID string) string {
	c := b64u.EncodeToString([]byte(randHex(32)))[:43]
	pkState.Lock()
	for k, v := range pkState.ch {
		if time.Now().After(v.exp) {
			delete(pkState.ch, k)
		}
	}
	pkState.ch[c] = pkChallenge{purpose: purpose, subject: subject, rpID: rpID, exp: time.Now().Add(5 * time.Minute)}
	pkState.Unlock()
	return c
}

func takeChallenge(c string) (pkChallenge, bool) {
	pkState.Lock()
	defer pkState.Unlock()
	v, ok := pkState.ch[c]
	delete(pkState.ch, c)
	return v, ok && time.Now().Before(v.exp)
}

// checkClientData prüft Typ, Challenge und Herkunft; Rückgabe: Challenge-Eintrag.
func checkClientData(raw []byte, typ, purpose string) (pkChallenge, error) {
	var cd struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Origin    string `json:"origin"`
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return pkChallenge{}, errors.New("clientDataJSON ungültig")
	}
	if cd.Type != typ {
		return pkChallenge{}, errors.New("falscher Vorgang")
	}
	ch, ok := takeChallenge(strings.TrimRight(cd.Challenge, "="))
	if !ok || ch.purpose != purpose {
		return pkChallenge{}, errors.New("Anfrage abgelaufen – bitte erneut versuchen")
	}
	if !originOK(cd.Origin, ch.rpID) {
		return pkChallenge{}, fmt.Errorf("falsche Herkunft %q", cd.Origin)
	}
	return ch, nil
}

// passkeysOf / setPasskeys: Passkeys eines Benutzers bzw. des Betreiber-Logins.
func (m *Monitor) passkeysOf(subject string) []Passkey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if subject == "admin" {
		return append([]Passkey(nil), m.cfg.AdminPasskeys...)
	}
	for _, u := range m.cfg.Users {
		if u.ID == subject {
			return append([]Passkey(nil), u.Passkeys...)
		}
	}
	return nil
}

func (m *Monitor) setPasskeysLocked(subject string, l []Passkey) {
	if subject == "admin" {
		m.cfg.AdminPasskeys = l
		return
	}
	for i := range m.cfg.Users {
		if m.cfg.Users[i].ID == subject {
			m.cfg.Users[i].Passkeys = l
		}
	}
}

func subjectOf(p *Principal) string {
	if p.User == nil {
		return "admin"
	}
	return p.User.ID
}

// stepUpOK: Passkey-Bestätigung der letzten 2 Minuten verbrauchen.
func stepUpOK(subject string) bool {
	pkState.Lock()
	defer pkState.Unlock()
	t, ok := pkState.stepup[subject]
	delete(pkState.stepup, subject)
	return ok && time.Since(t) < 2*time.Minute
}

// secondFactor: Code aus der Authenticator-App oder (code == "passkey") eine frische Passkey-Bestätigung.
func (m *Monitor) secondFactor(p *Principal, code string) (int, error) {
	sub, sec, _ := m.totpOf(p)
	if strings.TrimSpace(code) == "passkey" {
		if stepUpOK(sub) {
			return 0, nil
		}
		return 403, errors.New("Passkey-Bestätigung fehlt oder ist abgelaufen – bitte erneut bestätigen")
	}
	if sec == "" {
		if len(m.passkeysOf(sub)) > 0 {
			return 403, errors.New("Bitte mit Passkey bestätigen")
		}
		return 412, errors.New("Bitte zuerst unter Einstellungen → Mein Konto die Zwei-Faktor-Anmeldung oder einen Passkey einrichten")
	}
	if err := totpCheck(sub, sec, code, time.Now()); err != nil {
		return 403, err
	}
	return 0, nil
}

// ---------- API ----------

type pkAssertion struct {
	ID         string `json:"id"`
	ClientData string `json:"clientDataJSON"`
	AuthData   string `json:"authenticatorData"`
	Signature  string `json:"signature"`
	UserHandle string `json:"userHandle"`
}

// verifyAssertion prüft eine Anmeldung/Bestätigung. subject leer = beliebiger Benutzer (Anmeldung).
func (s *Server) verifyAssertion(a pkAssertion, purpose, subject string) (string, error) {
	cdRaw, err := b64dec(a.ClientData)
	if err != nil {
		return "", errors.New("clientDataJSON fehlt")
	}
	ch, err := checkClientData(cdRaw, "webauthn.get", purpose)
	if err != nil {
		return "", err
	}
	if subject != "" && ch.subject != subject {
		return "", errors.New("Anfrage gehört zu einem anderen Konto")
	}
	ad, err := b64dec(a.AuthData)
	if err != nil {
		return "", errors.New("authenticatorData fehlt")
	}
	sig, err := b64dec(a.Signature)
	if err != nil {
		return "", errors.New("Signatur fehlt")
	}
	auth, err := parseAuthData(ad)
	if err != nil {
		return "", err
	}
	if auth.rpHash != sha256.Sum256([]byte(ch.rpID)) {
		return "", errors.New("Passkey gehört zu einer anderen Adresse")
	}
	if auth.flags&0x01 == 0 || auth.flags&0x04 == 0 {
		return "", errors.New("Bestätigung am Gerät (Face ID, Fingerabdruck, PIN) fehlt")
	}
	id := strings.TrimRight(a.ID, "=")
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	find := func(l []Passkey) int {
		for i := range l {
			if l[i].ID == id {
				return i
			}
		}
		return -1
	}
	sub, idx := "", -1
	var list []Passkey
	if i := find(m.cfg.AdminPasskeys); i >= 0 {
		sub, idx, list = "admin", i, m.cfg.AdminPasskeys
	}
	for _, u := range m.cfg.Users {
		if i := find(u.Passkeys); i >= 0 {
			sub, idx, list = u.ID, i, u.Passkeys
		}
	}
	if idx < 0 {
		return "", errors.New("Passkey unbekannt – bitte zuerst unter Mein Konto anlegen")
	}
	if subject != "" && sub != subject {
		return "", errors.New("Passkey gehört zu einem anderen Konto")
	}
	cdHash := sha256.Sum256(cdRaw)
	if err := coseVerify(list[idx].Key, append(append([]byte(nil), ad...), cdHash[:]...), sig); err != nil {
		return "", err
	}
	pk := &list[idx]
	if pk.Count > 0 && auth.count != 0 && auth.count <= pk.Count {
		return "", errors.New("Passkey wurde möglicherweise kopiert (Zähler) – bitte neu anlegen")
	}
	pk.Count, pk.Last = auth.count, time.Now().UnixMilli()
	m.setPasskeysLocked(sub, list)
	m.saveConfigLocked()
	return sub, nil
}

// POST /api/passkey/options {purpose: register|stepup} · GET /api/passkey → Liste · POST /api/passkey {action, id, name}
func (s *Server) handlePasskey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p == nil {
		http.Error(w, "nicht angemeldet", 401)
		return
	}
	sub := subjectOf(p)
	m := s.m
	switch {
	case r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, k := range m.passkeysOf(sub) {
			out = append(out, map[string]any{"id": k.ID, "name": k.Name, "created": k.Created, "last": k.Last})
		}
		writeJSON(w, out)
	case strings.HasSuffix(r.URL.Path, "/options"):
		var req struct {
			Purpose string `json:"purpose"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		rp := passkeyRPID(r)
		var allow []map[string]any
		for _, k := range m.passkeysOf(sub) {
			allow = append(allow, map[string]any{"type": "public-key", "id": k.ID})
		}
		switch req.Purpose {
		case "register":
			name, disp := "admin", "Betreiber"
			if p.User != nil {
				name, disp = p.User.Email, p.User.Name
				if disp == "" {
					disp = p.User.Email
				}
			}
			writeJSON(w, map[string]any{
				"challenge":              newChallenge("register", sub, rp),
				"rp":                     map[string]string{"id": rp, "name": "PingWacht"},
				"user":                   map[string]string{"id": b64u.EncodeToString([]byte(sub)), "name": name, "displayName": disp},
				"pubKeyCredParams":       []map[string]any{{"type": "public-key", "alg": -7}, {"type": "public-key", "alg": -257}},
				"timeout":                120000,
				"attestation":            "none",
				"authenticatorSelection": map[string]any{"residentKey": "required", "requireResidentKey": true, "userVerification": "required"},
				"excludeCredentials":     allow,
			})
		case "stepup":
			if len(allow) == 0 {
				http.Error(w, "Kein Passkey eingerichtet", 412)
				return
			}
			writeJSON(w, map[string]any{"challenge": newChallenge("stepup", sub, rp), "rpId": rp, "timeout": 120000, "userVerification": "required", "allowCredentials": allow})
		default:
			http.Error(w, "unbekannter Zweck", 400)
		}
	case strings.HasSuffix(r.URL.Path, "/register"):
		var req struct {
			Name       string `json:"name"`
			ClientData string `json:"clientDataJSON"`
			Attest     string `json:"attestationObject"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		cdRaw, err := b64dec(req.ClientData)
		if err != nil {
			http.Error(w, "clientDataJSON fehlt", 400)
			return
		}
		ch, err := checkClientData(cdRaw, "webauthn.create", "register")
		if err != nil || ch.subject != sub {
			if err == nil {
				err = errors.New("Anfrage gehört zu einem anderen Konto")
			}
			http.Error(w, err.Error(), 400)
			return
		}
		attRaw, err := b64dec(req.Attest)
		if err != nil {
			http.Error(w, "attestationObject fehlt", 400)
			return
		}
		att, _, err := cborDecode(attRaw)
		am, ok := att.(map[any]any)
		if err != nil || !ok {
			http.Error(w, "attestationObject ungültig", 400)
			return
		}
		adb, _ := am["authData"].([]byte)
		auth, err := parseAuthData(adb)
		if err != nil || auth.credID == nil {
			http.Error(w, "Passkey-Daten ungültig", 400)
			return
		}
		if auth.rpHash != sha256.Sum256([]byte(ch.rpID)) {
			http.Error(w, "Passkey gehört zu einer anderen Adresse", 400)
			return
		}
		if auth.flags&0x01 == 0 || auth.flags&0x04 == 0 {
			http.Error(w, "Bestätigung am Gerät fehlt", 400)
			return
		}
		if err := coseVerify(auth.key, []byte("probe"), nil); err != nil && !strings.Contains(err.Error(), "Signatur") {
			http.Error(w, "Schlüssel: "+err.Error(), 400)
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = "Passkey"
		}
		if len(name) > 60 {
			name = name[:60]
		}
		k := Passkey{ID: b64u.EncodeToString(auth.credID), Key: auth.key, Count: auth.count, Name: name, Created: time.Now().UnixMilli()}
		m.mu.Lock()
		l := append([]Passkey(nil), m.passkeysOfLocked(sub)...)
		for _, x := range l {
			if x.ID == k.ID {
				m.mu.Unlock()
				http.Error(w, "Dieser Passkey ist schon eingerichtet", 409)
				return
			}
		}
		if len(l) >= 10 {
			m.mu.Unlock()
			http.Error(w, "Höchstens 10 Passkeys je Konto", 400)
			return
		}
		m.setPasskeysLocked(sub, append(l, k))
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	case strings.HasSuffix(r.URL.Path, "/stepup"):
		var a pkAssertion
		if !readJSON(w, r, &a) {
			return
		}
		if _, err := s.verifyAssertion(a, "stepup", sub); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		pkState.Lock()
		pkState.stepup[sub] = time.Now()
		pkState.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default: // POST /api/passkey {action: delete|rename, id, name}
		var req struct {
			Action string `json:"action"`
			ID     string `json:"id"`
			Name   string `json:"name"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		m.mu.Lock()
		l := append([]Passkey(nil), m.passkeysOfLocked(sub)...)
		out := l[:0]
		for _, k := range l {
			if k.ID == req.ID {
				if req.Action == "delete" {
					continue
				}
				if req.Action == "rename" && strings.TrimSpace(req.Name) != "" {
					k.Name = strings.TrimSpace(req.Name)
				}
			}
			out = append(out, k)
		}
		m.setPasskeysLocked(sub, out)
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func (m *Monitor) passkeysOfLocked(subject string) []Passkey {
	if subject == "admin" {
		return m.cfg.AdminPasskeys
	}
	for _, u := range m.cfg.Users {
		if u.ID == subject {
			return u.Passkeys
		}
	}
	return nil
}

// POST /passkey/login/options → Challenge (ohne Konto) · POST /passkey/login {Assertion, app?}
func (s *Server) handlePasskeyLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/options") {
		rp := passkeyRPID(r)
		writeJSON(w, map[string]any{"challenge": newChallenge("login", "", rp), "rpId": rp, "timeout": 120000, "userVerification": "required", "allowCredentials": []any{}})
		return
	}
	var req struct {
		pkAssertion
		App bool `json:"app"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	if err := loginBlocked("ip:" + ip); err != nil {
		http.Error(w, err.Error(), 429)
		return
	}
	sub, err := s.verifyAssertion(req.pkAssertion, "login", "")
	if err != nil {
		loginFailed("passkey", ip)
		time.Sleep(500 * time.Millisecond)
		http.Error(w, err.Error(), 401)
		return
	}
	var tok string
	if sub == "admin" {
		tok = s.makeToken("admin", s.adminRef())
	} else {
		u := s.m.userByID(sub)
		if u == nil {
			http.Error(w, "Konto nicht gefunden", 401)
			return
		}
		if err := accessError(u, s.m.orgByID(u.Org)); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		tok = s.makeToken(u.ID, userRef(u))
		dev := r.Header.Get("X-Device")
		if dev == "" && !req.App {
			dev = loginDevice(w, r)
		}
		s.noteLoginDevice(u, "passkey:"+dev, r)
	}
	if req.App {
		writeJSON(w, map[string]any{"token": tok, "version": version})
		return
	}
	s.setSession(w, r, tok)
	writeJSON(w, map[string]bool{"ok": true})
}

// /.well-known/apple-app-site-association: erlaubt der iPhone-App, Passkeys dieser Adresse zu nutzen.
func handleAASA(w http.ResponseWriter, r *http.Request) {
	team := strings.TrimSpace(os.Getenv("APPLE_TEAM_ID"))
	if team == "" {
		team = "A4FKEDK5GH"
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"webcredentials":{"apps":["%s.de.rst.pingwatch"]}}`, team)
}

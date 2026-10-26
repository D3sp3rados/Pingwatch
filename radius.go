package main

// Eigener RADIUS-Server im Agenten – für die Anmeldung an Netonix-Switches (und andere Geräte,
// die RADIUS können). Jeder Techniker bekommt einen eigenen Benutzer; jede Anmeldung (auch
// fehlgeschlagene) wird protokolliert; Benutzer lassen sich zentral sperren.
// PingWacht selbst bekommt einen Dienst-Benutzer, mit dem der Agent die Netonix ausliest.
// Passwörter liegen nur als PBKDF2-Hash vor (außer dem zufälligen Dienst-Passwort).
// Protokoll: RADIUS (RFC 2865) Access-Request mit PAP, UDP 1812, Message-Authenticator (RFC 3579).

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type RadiusUser struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Salt      string `json:"salt,omitempty"`
	Hash      string `json:"hash,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Service   bool   `json:"service,omitempty"` // Dienst-Benutzer von PingWacht
	Created   int64  `json:"created"`
	LastLogin int64  `json:"lastLogin,omitempty"`
	LastFrom  string `json:"lastFrom,omitempty"`
}

type RadiusCfg struct {
	On          bool         `json:"on"`
	Site        string       `json:"site"`   // Agent-Standort, auf dem der RADIUS läuft
	Secret      string       `json:"secret"` // gemeinsames Secret (in den Geräten eintragen)
	Users       []RadiusUser `json:"users"`
	ServicePass string       `json:"servicePass,omitempty"` // Klartext nur für den Dienst-Benutzer
}

const radiusServiceUser = "PingWacht"

func radiusHash(pw, salt string) string {
	k, _ := pbkdf2.Key(sha256.New, pw, []byte(salt), 20000, 32)
	return hex.EncodeToString(k)
}

func radiusCheck(u RadiusUser, pw string) bool {
	return subtle.ConstantTimeCompare([]byte(radiusHash(pw, u.Salt)), []byte(u.Hash)) == 1
}

// ---------- Paket ----------

const (
	radAccessRequest = 1
	radAccessAccept  = 2
	radAccessReject  = 3
	attrUserName     = 1
	attrUserPassword = 2
	attrNASIP        = 4
	attrServiceType  = 6
	attrReplyMessage = 18
	attrNASIdent     = 32
	attrMsgAuth      = 80
)

type radAttr struct {
	T byte
	V []byte
}

func radParse(b []byte) (code, id byte, auth []byte, attrs []radAttr, err error) {
	if len(b) < 20 {
		return 0, 0, nil, nil, errors.New("zu kurz")
	}
	l := int(binary.BigEndian.Uint16(b[2:4]))
	if l < 20 || l > len(b) {
		return 0, 0, nil, nil, errors.New("Länge")
	}
	code, id, auth = b[0], b[1], b[4:20]
	for p := 20; p+2 <= l; {
		t, al := b[p], int(b[p+1])
		if al < 2 || p+al > l {
			return 0, 0, nil, nil, errors.New("Attribut")
		}
		attrs = append(attrs, radAttr{t, b[p+2 : p+al]})
		p += al
	}
	return
}

func radAttrVal(attrs []radAttr, t byte) []byte {
	for _, a := range attrs {
		if a.T == t {
			return a.V
		}
	}
	return nil
}

// radPAP entschlüsselt User-Password (RFC 2865 5.2).
func radPAP(enc []byte, secret string, auth []byte) string {
	if len(enc) == 0 || len(enc)%16 != 0 {
		return ""
	}
	out := make([]byte, len(enc))
	prev := auth
	for i := 0; i < len(enc); i += 16 {
		h := md5.Sum(append([]byte(secret), prev...))
		for j := 0; j < 16; j++ {
			out[i+j] = enc[i+j] ^ h[j]
		}
		prev = enc[i : i+16]
	}
	return string(bytes.TrimRight(out, "\x00"))
}

// radMsgAuthOK prüft ein vorhandenes Message-Authenticator-Attribut der Anfrage.
func radMsgAuthOK(pkt []byte, secret string) bool {
	l := int(binary.BigEndian.Uint16(pkt[2:4]))
	cp := append([]byte(nil), pkt[:l]...)
	for p := 20; p+2 <= l; {
		t, al := cp[p], int(cp[p+1])
		if t == attrMsgAuth && al == 18 {
			got := append([]byte(nil), cp[p+2:p+18]...)
			for i := p + 2; i < p+18; i++ {
				cp[i] = 0
			}
			mac := hmac.New(md5.New, []byte(secret))
			mac.Write(cp)
			return hmac.Equal(got, mac.Sum(nil))
		}
		p += al
	}
	return true // nicht vorhanden
}

func radReply(code, id byte, reqAuth []byte, secret, msg string) []byte {
	var attrs []byte
	if code == radAccessAccept {
		attrs = append(attrs, attrServiceType, 6, 0, 0, 0, 6) // Administrative-User
	}
	if msg != "" {
		m := []byte(msg)
		if len(m) > 200 {
			m = m[:200]
		}
		attrs = append(attrs, attrReplyMessage, byte(2+len(m)))
		attrs = append(attrs, m...)
	}
	maPos := 20 + len(attrs)
	attrs = append(attrs, attrMsgAuth, 18)
	attrs = append(attrs, make([]byte, 16)...)
	l := 20 + len(attrs)
	pkt := make([]byte, l)
	pkt[0], pkt[1] = code, id
	binary.BigEndian.PutUint16(pkt[2:4], uint16(l))
	copy(pkt[4:20], reqAuth)
	copy(pkt[20:], attrs)
	mac := hmac.New(md5.New, []byte(secret))
	mac.Write(pkt)
	copy(pkt[maPos+2:maPos+18], mac.Sum(nil))
	h := md5.New()
	h.Write(pkt)
	h.Write([]byte(secret))
	copy(pkt[4:20], h.Sum(nil))
	return pkt
}

// ---------- Agent ----------

type radiusAgentCfg struct {
	Secret string `json:"secret"`
	Users  []struct {
		Name     string `json:"n"`
		Salt     string `json:"s"`
		Hash     string `json:"h"`
		Disabled bool   `json:"d,omitempty"`
	} `json:"users"`
}

type radiusLog struct {
	T      int64  `json:"t"`
	User   string `json:"user"`
	From   string `json:"from"` // IP des Geräts
	NAS    string `json:"nas,omitempty"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

var radiusAgent = struct {
	sync.Mutex
	cfg     *radiusAgentCfg
	running bool
	err     string
	pending []radiusLog
}{}

func (a *Agent) radiusApply(c *radiusAgentCfg) {
	radiusAgent.Lock()
	radiusAgent.cfg = c
	start := c != nil && !radiusAgent.running
	if start {
		radiusAgent.running = true
	}
	radiusAgent.Unlock()
	if start {
		radiusFirewall()
		go a.radiusServe()
	}
}

func (a *Agent) radiusServe() {
	pc, err := net.ListenPacket("udp4", ":1812")
	if err != nil {
		radiusAgent.Lock()
		radiusAgent.running, radiusAgent.err = false, err.Error()
		radiusAgent.Unlock()
		log.Printf("RADIUS: %v", err)
		return
	}
	log.Printf("RADIUS-Server läuft auf UDP 1812")
	go a.radiusFlush()
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		radiusAgent.Lock()
		c := radiusAgent.cfg
		radiusAgent.Unlock()
		if c == nil || c.Secret == "" {
			continue // ausgeschaltet
		}
		pkt := append([]byte(nil), buf[:n]...)
		code, id, auth, attrs, err := radParse(pkt)
		if err != nil || code != radAccessRequest || !radMsgAuthOK(pkt, c.Secret) {
			continue // ungültig oder falsches Secret – nicht antworten
		}
		user := string(radAttrVal(attrs, attrUserName))
		pw := radPAP(radAttrVal(attrs, attrUserPassword), c.Secret, auth)
		ip := from.String()
		if h, _, e := net.SplitHostPort(ip); e == nil {
			ip = h
		}
		ok, reason := false, "unbekannter Benutzer"
		for _, u := range c.Users {
			if strings.EqualFold(u.Name, user) {
				switch {
				case u.Disabled:
					reason = "gesperrt"
				case pw == "":
					reason = "kein Passwort (nur PAP wird unterstützt)"
				case radiusCheck(RadiusUser{Salt: u.Salt, Hash: u.Hash}, pw):
					ok, reason = true, ""
				default:
					reason = "falsches Passwort"
				}
				break
			}
		}
		code = radAccessReject
		if ok {
			code = radAccessAccept
		}
		pc.WriteTo(radReply(code, id, auth, c.Secret, ""), from)
		radiusAgent.Lock()
		radiusAgent.pending = append(radiusAgent.pending, radiusLog{T: time.Now().UnixMilli(), User: user, From: ip, NAS: string(radAttrVal(attrs, attrNASIdent)), OK: ok, Reason: reason})
		if len(radiusAgent.pending) > 500 {
			radiusAgent.pending = radiusAgent.pending[len(radiusAgent.pending)-500:]
		}
		radiusAgent.Unlock()
	}
}

func (a *Agent) radiusFlush() {
	for range time.Tick(10 * time.Second) {
		radiusAgent.Lock()
		p := radiusAgent.pending
		radiusAgent.pending = nil
		radiusAgent.Unlock()
		if len(p) == 0 {
			continue
		}
		b, _ := json.Marshal(map[string]any{"log": p})
		resp, err := a.req("POST", "/api/agent/radius", bytes.NewReader(b), false)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			continue
		}
		if resp != nil {
			resp.Body.Close()
		}
		radiusAgent.Lock() // später nochmal
		radiusAgent.pending = append(p, radiusAgent.pending...)
		radiusAgent.Unlock()
	}
}

// ---------- Server ----------

// radiusForSite: Konfiguration für den Agenten dieses Standorts (nil = kein RADIUS dort).
func (m *Monitor) radiusForSite(site SiteCfg) *radiusAgentCfg {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rc := m.cfg.Radius[site.Org]
	if rc == nil || !rc.On || rc.Site != site.ID || rc.Secret == "" {
		return nil
	}
	out := &radiusAgentCfg{Secret: rc.Secret}
	for _, u := range rc.Users {
		out.Users = append(out.Users, struct {
			Name     string `json:"n"`
			Salt     string `json:"s"`
			Hash     string `json:"h"`
			Disabled bool   `json:"d,omitempty"`
		}{u.Name, u.Salt, u.Hash, u.Disabled})
	}
	return out
}

// radiusService: Benutzer/Passwort des Dienst-Benutzers für die Netonix-Abfrage (leer = kein RADIUS).
func (m *Monitor) radiusService(org string) (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rc := m.cfg.Radius[org]
	if rc == nil || !rc.On || rc.ServicePass == "" {
		return "", ""
	}
	return radiusServiceUser, rc.ServicePass
}

func randPass(n int) string {
	const al = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = al[int(b[i])%len(al)]
	}
	return string(b)
}

func newRadiusUser(name, pw string, service bool) RadiusUser {
	salt := randHex(8)
	return RadiusUser{ID: randHex(5), Name: name, Salt: salt, Hash: radiusHash(pw, salt), Service: service, Created: time.Now().UnixMilli()}
}

// POST /api/agent/radius {log:[…]}
func (s *Server) handleAgentRadius(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Log []radiusLog `json:"log"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	m := s.m
	// Gerätenamen über die Hosts des Kontos
	names := map[string]string{}
	for _, h := range m.hostsOf(site.Org) {
		h.mu.Lock()
		names[strings.TrimSpace(h.cfg.Addr)] = h.cfg.Name
		h.mu.Unlock()
	}
	m.mu.Lock()
	rc := m.cfg.Radius[site.Org]
	if rc == nil || rc.Site != site.ID {
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	type ev struct {
		l       radiusLog
		service bool
	}
	var evs []ev
	for _, l := range req.Log {
		svc := false
		for i := range rc.Users {
			if strings.EqualFold(rc.Users[i].Name, l.User) {
				svc = rc.Users[i].Service
				if l.OK && l.T > rc.Users[i].LastLogin {
					rc.Users[i].LastLogin, rc.Users[i].LastFrom = l.T, l.From
				}
			}
		}
		evs = append(evs, ev{l, svc})
	}
	m.saveConfigLocked()
	m.mu.Unlock()
	for _, e := range evs {
		if e.service && e.l.OK {
			continue // Abfragen von PingWacht selbst nicht ins Protokoll
		}
		dev := names[e.l.From]
		if dev == "" {
			dev = e.l.From
		}
		txt := "RADIUS: " + e.l.User + " @ " + dev
		if e.l.OK {
			txt += " – angemeldet"
		} else {
			txt += " – abgelehnt (" + e.l.Reason + ")"
		}
		m.closeSiteEvent(m.newSiteEvent(site, "radius", txt, e.l.T, true), e.l.T)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type radiusView struct {
	On      bool         `json:"on"`
	Site    string       `json:"site"`
	Secret  string       `json:"secret"`
	Users   []RadiusUser `json:"users"`
	Log     []Event      `json:"log"`
	Running bool         `json:"running"`
}

// GET/POST /api/radius
func (s *Server) handleRadius(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	if r.Method == http.MethodPost {
		if !p.canEdit() {
			http.Error(w, "keine Berechtigung", 403)
			return
		}
		var req struct {
			Action, Site, ID, Name, Password string
			On, Disabled, NewSecret          bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		m.mu.Lock()
		if m.cfg.Radius == nil {
			m.cfg.Radius = map[string]*RadiusCfg{}
		}
		rc := m.cfg.Radius[p.Org]
		if rc == nil {
			rc = &RadiusCfg{}
			m.cfg.Radius[p.Org] = rc
		}
		fail := func(msg string, code int) { m.mu.Unlock(); http.Error(w, msg, code) }
		switch req.Action {
		case "save":
			if req.On {
				sc, ok := m.siteByIDLocked(req.Site)
				if !ok || sc.Org != p.Org || sc.Kind == siteKindUniFi || sc.ID == "cloud" {
					fail("Bitte einen Standort mit Agent wählen", 400)
					return
				}
			}
			rc.On, rc.Site = req.On, req.Site
			if rc.Secret == "" || req.NewSecret {
				rc.Secret = randPass(24)
			}
			has := false
			for _, u := range rc.Users {
				if u.Service {
					has = true
				}
			}
			if !has {
				rc.ServicePass = randPass(24)
				rc.Users = append(rc.Users, newRadiusUser(radiusServiceUser, rc.ServicePass, true))
			}
		case "adduser":
			name := strings.TrimSpace(req.Name)
			if name == "" || len(name) > 40 || strings.ContainsAny(name, " \t") {
				fail("Benutzername ungültig (ohne Leerzeichen, höchstens 40 Zeichen)", 400)
				return
			}
			if len(req.Password) < 8 {
				fail("Passwort mindestens 8 Zeichen", 400)
				return
			}
			for _, u := range rc.Users {
				if strings.EqualFold(u.Name, name) {
					fail("Benutzer gibt es schon", 409)
					return
				}
			}
			rc.Users = append(rc.Users, newRadiusUser(name, req.Password, false))
		case "setpw", "toggle", "del":
			idx := -1
			for i, u := range rc.Users {
				if u.ID == req.ID {
					idx = i
				}
			}
			if idx < 0 {
				fail("Benutzer unbekannt", 404)
				return
			}
			u := &rc.Users[idx]
			switch req.Action {
			case "setpw":
				if len(req.Password) < 8 {
					fail("Passwort mindestens 8 Zeichen", 400)
					return
				}
				u.Salt = randHex(8)
				u.Hash = radiusHash(req.Password, u.Salt)
				if u.Service { // Dienst-Benutzer: Passwort braucht der Agent im Klartext für die Netonix-Abfrage
					rc.ServicePass = req.Password
				}
			case "toggle":
				u.Disabled = req.Disabled
			case "del":
				if u.Service {
					fail("Der Dienst-Benutzer wird für die Netonix-Abfrage gebraucht", 400)
					return
				}
				rc.Users = append(rc.Users[:idx], rc.Users[idx+1:]...)
			}
		default:
			fail("unbekannte Aktion", 400)
			return
		}
		m.saveConfigLocked()
		m.mu.Unlock()
	}
	m.mu.RLock()
	rc := m.cfg.Radius[p.Org]
	v := radiusView{Users: []RadiusUser{}, Log: []Event{}}
	if rc != nil {
		v.On, v.Site, v.Secret = rc.On, rc.Site, rc.Secret
		for _, u := range rc.Users {
			u.Salt, u.Hash = "", ""
			v.Users = append(v.Users, u)
		}
	}
	m.mu.RUnlock()
	if !p.canEdit() {
		v.Secret = ""
	}
	sort.SliceStable(v.Users, func(i, j int) bool { return v.Users[i].Service && !v.Users[j].Service })
	m.evMu.Lock()
	for i := len(m.events) - 1; i >= 0 && len(v.Log) < 200; i-- {
		if e := m.events[i]; e.Type == "radius" && e.Org == p.Org {
			v.Log = append(v.Log, *e)
		}
	}
	m.evMu.Unlock()
	if v.On {
		v.Running, _ = m.siteOnline(v.Site)
	}
	writeJSON(w, v)
}

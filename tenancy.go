package main

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------- Datenmodell ----------

// Org ist ein Kunde (Mandant). Hosts, Standorte, Geräte und Benutzer gehören zu genau einer Org.
type Org struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Plan       string `json:"plan"`              // free, basis, pro, partner, internal
	Sites      int    `json:"sites"`             // gebuchte Standorte (Abo-Menge)
	Billing    string `json:"billing,omitempty"` // stripe, invoice, ""
	Created    int64  `json:"created"`
	Email      string `json:"email,omitempty"`  // Rechnungs-/Kontakt-E-Mail
	Status     string `json:"status,omitempty"` // active, past_due, canceled
	PaidUntil  int64  `json:"paidUntil,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
	ReportSent string `json:"reportSent,omitempty"`
	Expires    int64  `json:"expires,omitempty"` // Testzugang bis (ms), 0 = unbegrenzt
	// Testphase nach Selbst-Registrierung: Pro bis TrialUntil, danach automatisch Free (siehe trial.go)
	TrialUntil  int64      `json:"trialUntil,omitempty"`
	TrialWarned bool       `json:"trialWarned,omitempty"`
	TrialEnded  bool       `json:"trialEnded,omitempty"`
	StatusPage  StatusPage `json:"statusPage,omitempty"` // Monat (YYYY-MM) des zuletzt versandten Monatsberichts

	StripeCustomer string `json:"stripeCustomer,omitempty"`
	StripeSub      string `json:"stripeSub,omitempty"`
	StripeItem     string `json:"stripeItem,omitempty"`
	// einmalige Zustimmung beim ersten Einschalten von Updates/Neustarts/Scripts
	ActionsConsent *Consent `json:"actionsConsent,omitempty"`
}

type User struct {
	ID        string       `json:"id"`
	Email     string       `json:"email"`
	Name      string       `json:"name"`
	Org       string       `json:"org"`
	Role      string       `json:"role"` // owner, admin, viewer
	PassHash  string       `json:"passHash"`
	Created   int64        `json:"created"`
	LastLogin int64        `json:"lastLogin,omitempty"`
	Super     bool         `json:"super,omitempty"`   // Betreiber-Admin (nur Benutzer der Betreiber-Org)
	Expires   int64        `json:"expires,omitempty"` // Zugang gültig bis (ms), 0 = unbegrenzt
	Notify    *NotifyPrefs `json:"notify,omitempty"`
	TempPW    bool         `json:"tempPw,omitempty"` // vorläufiges Passwort, muss noch geändert werden
	// Zwei-Faktor-Code (TOTP) für Updates/Neustarts: aktives bzw. noch nicht bestätigtes Geheimnis
	TOTP        string `json:"totp,omitempty"`
	TOTPPending string `json:"totpPending,omitempty"`
	Terms *Consent `json:"terms,omitempty"` // Zustimmung zu den Nutzungsbedingungen
}

// Plan beschreibt ein Paket. Preise netto in Cent pro Standort und Monat.
type Plan struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	PriceCents   int    `json:"priceCents"`
	MinSites     int    `json:"minSites"`
	MaxSites     int    `json:"maxSites"`     // 0 = laut Buchung
	HostsPerSite int    `json:"hostsPerSite"` // 0 = unbegrenzt
	// Rechner-Überwachung: Rechner je Standort (-1 = unbegrenzt, 0 = nicht enthalten)
	MachinesPerSite int      `json:"machinesPerSite"`
	Retention       int      `json:"retention"`   // Tage
	MinInterval     int      `json:"minInterval"` // ms
	Features        []string `json:"features"`
	Public          bool     `json:"public"`
	Text            string   `json:"text"`
}

var plans = []Plan{
	{ID: "free", Name: "Free", PriceCents: 0, MinSites: 0, MaxSites: 1, HostsPerSite: 5, Retention: 7, MinInterval: 5000,
		Features: []string{"push"}, Public: true, Text: "1 Standort, 5 Hosts, 7 Tage Verlauf, Push aufs iPhone"},
	{ID: "basis", Name: "Basis", PriceCents: 980, MinSites: 1, HostsPerSite: 25, MachinesPerSite: 5, Retention: 90, MinInterval: 1000,
		Features: []string{"push", "alerts", "assistant"}, Public: true, Text: "25 Hosts und 5 überwachte Rechner (mit Inventar) pro Standort, 90 Tage Verlauf, Push, E-Mail, Teams, Telegram, UniFi auch ohne Agent über die UniFi-Cloud, KI-Assistent"},
	{ID: "pro", Name: "Pro", PriceCents: 1980, MinSites: 1, HostsPerSite: 100, MachinesPerSite: 25, Retention: 365, MinInterval: 500,
		Features: []string{"push", "alerts", "report", "traceroute", "status", "checks", "assistant", "requests"}, Public: true,
		Text: "100 Hosts und 25 überwachte Rechner (mit Inventar) pro Standort, 1 Jahr Verlauf, Monatsbericht (PDF), Traceroute, Statusseite, KI-Assistent mit Änderungswünschen"},
	{ID: "partner", Name: "Partner", PriceCents: 390, MinSites: 10, HostsPerSite: 25, MachinesPerSite: 5, Retention: 90, MinInterval: 1000,
		// nicht öffentlich: Partnerkonditionen nur auf Anfrage, vom Betreiber zugewiesen
		Features: []string{"push", "alerts", "report", "assistant"}, Public: false, Text: "Ab 10 Standorten: wie Basis (25 Hosts, 5 Rechner je Standort), inkl. Monatsbericht"},
	{ID: "internal", Name: "Intern", PriceCents: 0, HostsPerSite: 0, MachinesPerSite: -1, Retention: 0, MinInterval: 500,
		Features: []string{"push", "alerts", "report", "traceroute", "status", "checks", "assistant", "requests"}, Text: "Ohne Begrenzung"},
}

func planByID(id string) Plan {
	for _, p := range plans {
		if p.ID == id {
			return p
		}
	}
	return plans[0]
}

func (p Plan) has(f string) bool {
	for _, x := range p.Features {
		if x == f {
			return true
		}
	}
	return false
}

// Limits einer Org (0 = unbegrenzt)
func (o Org) siteLimit() int {
	p := planByID(o.Plan)
	switch {
	case p.ID == "internal":
		return 0
	case p.MaxSites > 0:
		return p.MaxSites
	}
	if o.Sites < p.MinSites {
		return p.MinSites
	}
	return o.Sites
}

func (o Org) hostLimit() int {
	p := planByID(o.Plan)
	if p.HostsPerSite == 0 {
		return 0
	}
	n := o.siteLimit()
	if n < 1 {
		n = 1
	}
	return p.HostsPerSite * n
}

func (o Org) retention(global int) int {
	if r := planByID(o.Plan).Retention; r > 0 {
		return r
	}
	return global
}

// ---------- Passwörter ----------

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	k, _ := pbkdf2.Key(sha256.New, pw, salt, 210_000, 32)
	return "pbkdf2$210000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(k)
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, _ := strconv.Atoi(parts[1])
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || iter < 1000 {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(k, want) == 1
}

func validEmail(e string) bool {
	a, err := mail.ParseAddress(e)
	return err == nil && a.Address == e && strings.Contains(e, ".")
}

func tempPassword() string {
	const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 12)
	rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// ---------- Sitzungen ----------

// Principal ist der angemeldete Benutzer einer Anfrage.
type Principal struct {
	User  *User  // nil beim Betreiber-Login (ADMIN_PASSWORD)
	Super bool   // Betreiber: sieht und verwaltet alle Kunden
	Org   string // aktuell gewählte Org
	Role  string // owner, admin, viewer
}

func (p *Principal) canEdit() bool { return p.Super || p.Role == "owner" || p.Role == "admin" }
func (p *Principal) isOwner() bool { return p.Super || p.Role == "owner" }

type ctxKey struct{}

func principal(r *http.Request) *Principal {
	p, _ := r.Context().Value(ctxKey{}).(*Principal)
	return p
}

const sessionDays = 180

// Token-Aufbau: base64(subject|ablauf).hmac — subject ist "admin" oder die Benutzer-ID.
// In die Signatur fließt der Passwort-Hash ein, damit eine Passwortänderung alte Sitzungen beendet.
func (s *Server) makeToken(subject, pwRef string) string {
	exp := time.Now().Add(sessionDays * 24 * time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(subject + "|" + strconv.FormatInt(exp, 10)))
	return payload + "." + s.sign(payload+"|"+pwRef)
}

func (s *Server) sign(v string) string {
	s.m.mu.RLock()
	key := s.m.cfg.Secret
	s.m.mu.RUnlock()
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(v))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) adminRef() string {
	if s.envPass != "" {
		h := sha256.Sum256([]byte("admin|" + s.envPass))
		return hex.EncodeToString(h[:8])
	}
	s.m.mu.RLock()
	defer s.m.mu.RUnlock()
	return s.m.cfg.PassHash
}

func userRef(u *User) string {
	h := sha256.Sum256([]byte(u.PassHash))
	return hex.EncodeToString(h[:8])
}

func (s *Server) parseToken(tok string) *Principal {
	i := strings.IndexByte(tok, '.')
	if i <= 0 {
		return nil
	}
	payload, sig := tok[:i], tok[i+1:]
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return nil
	}
	exp, _ := strconv.ParseInt(parts[1], 10, 64)
	if exp < time.Now().Unix() {
		return nil
	}
	if parts[0] == "admin" {
		if !hmac.Equal([]byte(sig), []byte(s.sign(payload+"|"+s.adminRef()))) {
			return nil
		}
		return &Principal{Super: true, Role: "owner"}
	}
	u := s.m.userByID(parts[0])
	if u == nil || !hmac.Equal([]byte(sig), []byte(s.sign(payload+"|"+userRef(u)))) {
		return nil
	}
	if accessError(u, s.m.orgByID(u.Org)) != nil {
		return nil
	}
	if u.isSuper() {
		return &Principal{User: u, Super: true, Role: "owner"}
	}
	return &Principal{User: u, Org: u.Org, Role: u.Role}
}

// authenticate liest Token aus Header oder Cookie und legt den Principal in den Kontext.
func (s *Server) authenticate(r *http.Request) *Principal {
	tok := bearer(r)
	if tok == "" {
		if c, err := r.Cookie("pw_session"); err == nil {
			tok = c.Value
		}
	}
	if tok == "" {
		return nil
	}
	p := s.parseToken(tok)
	if p == nil {
		return nil
	}
	if p.Super {
		// Betreiber wählt den Kunden per Cookie oder Parameter
		org := r.URL.Query().Get("org")
		if org == "" {
			org = r.Header.Get("X-Org")
		}
		if org == "" {
			if c, err := r.Cookie("pw_org"); err == nil {
				org = c.Value
			}
		}
		if s.m.orgByID(org) == nil {
			org = defaultOrgID
		}
		p.Org = org
	}
	return p
}

func (s *Server) withPrincipal(r *http.Request, p *Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{Name: "pw_session", Value: tok, Path: "/",
		MaxAge: sessionDays * 24 * 3600, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
}

// login prüft E-Mail + Passwort; leere E-Mail oder "admin" = Betreiber-Passwort.
func (s *Server) login(email, pw string) (string, *User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || email == "admin" {
		if s.passwordOK(pw) {
			return s.makeToken("admin", s.adminRef()), nil, nil
		}
		return "", nil, errors.New("Passwort falsch")
	}
	u := s.m.userByEmail(email)
	if u == nil || !checkPassword(u.PassHash, pw) {
		return "", nil, errors.New("E-Mail oder Passwort falsch")
	}
	if err := accessError(u, s.m.orgByID(u.Org)); err != nil {
		return "", nil, err
	}
	s.m.mu.Lock()
	for i := range s.m.cfg.Users {
		if s.m.cfg.Users[i].ID == u.ID {
			s.m.cfg.Users[i].LastLogin = time.Now().UnixMilli()
		}
	}
	s.m.saveConfigLocked()
	s.m.mu.Unlock()
	return s.makeToken(u.ID, userRef(u)), u, nil
}

func (u *User) isSuper() bool { return u.Super && u.Org == defaultOrgID }

// accessError: Zugang gesperrt oder abgelaufen?
func accessError(u *User, o *Org) error {
	now := time.Now().UnixMilli()
	switch {
	case o == nil || o.Disabled:
		return errors.New("Zugang gesperrt – bitte beim Anbieter melden")
	case o.Expires > 0 && now > o.Expires:
		return errors.New("Der Testzugang ist am " + time.UnixMilli(o.Expires).Format("02.01.2006") + " abgelaufen – bitte beim Anbieter melden")
	case u != nil && u.Expires > 0 && now > u.Expires:
		return errors.New("Ihr Zugang war bis " + time.UnixMilli(u.Expires).Format("02.01.2006") + " gültig")
	}
	return nil
}

// ---------- Zugriff auf Org-Daten ----------

const defaultOrgID = "default"

func (m *Monitor) orgByID(id string) *Org {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.cfg.Orgs {
		if m.cfg.Orgs[i].ID == id {
			cp := m.cfg.Orgs[i]
			return &cp
		}
	}
	return nil
}

func (m *Monitor) userByID(id string) *User {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.cfg.Users {
		if m.cfg.Users[i].ID == id {
			cp := m.cfg.Users[i]
			return &cp
		}
	}
	return nil
}

func (m *Monitor) userByEmail(email string) *User {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.cfg.Users {
		if strings.EqualFold(m.cfg.Users[i].Email, email) {
			cp := m.cfg.Users[i]
			return &cp
		}
	}
	return nil
}

// migrateTenancy legt beim ersten Start die Standard-Org an und ordnet vorhandene Daten zu.
func (m *Monitor) migrateTenancy() {
	if len(m.cfg.Orgs) == 0 {
		m.cfg.Orgs = []Org{{ID: defaultOrgID, Name: envOr("DEFAULT_ORG_NAME", envOr("PROVIDER_NAME", "RST-Datentechnik")),
			Plan: "internal", Created: time.Now().UnixMilli(), Status: "active"}}
	}
	for i := range m.cfg.Hosts {
		if m.cfg.Hosts[i].Org == "" {
			m.cfg.Hosts[i].Org = defaultOrgID
		}
	}
	for i := range m.cfg.Sites {
		if m.cfg.Sites[i].Org == "" {
			m.cfg.Sites[i].Org = defaultOrgID
		}
	}
	for i := range m.cfg.Devices {
		if m.cfg.Devices[i].Org == "" {
			m.cfg.Devices[i].Org = defaultOrgID
		}
	}
}

// orgOfEvent ergänzt fehlende Org-Zuordnung alter Ereignisse.
func (m *Monitor) fixEventOrgs() {
	hostOrg := map[string]string{}
	siteOrg := map[string]string{}
	for _, h := range m.cfg.Hosts {
		hostOrg[h.ID] = h.Org
	}
	for _, s := range m.cfg.Sites {
		siteOrg[s.ID] = s.Org
	}
	for _, e := range m.events {
		if e.Org != "" {
			continue
		}
		if o := hostOrg[e.HostID]; o != "" {
			e.Org = o
		} else if o := siteOrg[e.Site]; o != "" {
			e.Org = o
		} else {
			e.Org = defaultOrgID
		}
	}
}

// usage zählt Standorte und Hosts einer Org.
func (m *Monitor) usage(org string) (sites, hosts int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.cfg.Sites {
		if s.Org == org {
			sites++
		}
	}
	for _, h := range m.cfg.Hosts {
		if h.Org == org {
			hosts++
		}
	}
	return
}

// hostsOf liefert alle Hosts einer Org (org == "" → alle).
func (m *Monitor) hostsOf(org string) []*Host {
	var out []*Host
	for _, h := range m.hostList() {
		h.mu.Lock()
		o := h.cfg.Org
		h.mu.Unlock()
		if org == "" || o == org {
			out = append(out, h)
		}
	}
	return out
}

// hostFor liefert einen Host, wenn er zur Org des Benutzers gehört.
func (s *Server) hostFor(r *http.Request, id string) *Host {
	h := s.m.host(id)
	if h == nil {
		return nil
	}
	p := principal(r)
	h.mu.Lock()
	o := h.cfg.Org
	h.mu.Unlock()
	if p == nil || o != p.Org {
		return nil
	}
	return h
}

func (s *Server) siteFor(r *http.Request, id string) *SiteCfg {
	p := principal(r)
	s.m.mu.RLock()
	defer s.m.mu.RUnlock()
	for i := range s.m.cfg.Sites {
		if s.m.cfg.Sites[i].ID == id && p != nil && s.m.cfg.Sites[i].Org == p.Org {
			cp := s.m.cfg.Sites[i]
			return &cp
		}
	}
	return nil
}

// checkLimits prüft, ob ein weiterer Standort / Host erlaubt ist.
func (s *Server) checkLimits(org string, addSites, addHosts int) error {
	o := s.m.orgByID(org)
	if o == nil {
		return errors.New("Kunde unbekannt")
	}
	sites, hosts := s.m.usage(org)
	if l := o.siteLimit(); l > 0 && sites+addSites > l {
		return fmt.Errorf("Ihr Paket „%s“ umfasst %d Standort(e). Unter „Abo“ können Sie weitere buchen.", planByID(o.Plan).Name, l)
	}
	if l := o.hostLimit(); l > 0 && hosts+addHosts > l {
		return fmt.Errorf("Ihr Paket „%s“ umfasst %d Hosts. Unter „Abo“ können Sie erweitern.", planByID(o.Plan).Name, l)
	}
	return nil
}

func providerInfo() map[string]string {
	return map[string]string{
		"name":     envOr("PROVIDER_NAME", "RST Datentechnik GmbH"),
		"url":      envOr("PROVIDER_URL", "https://rst.de"),
		"imprint":  envOr("PROVIDER_IMPRINT_URL", "/impressum"),
		"privacy":  envOr("PROVIDER_PRIVACY_URL", "/datenschutz"),
		"terms":    envOr("PROVIDER_TERMS_URL", "/nutzungsbedingungen"),
		"email":    envOr("PROVIDER_EMAIL", ""),
		"signup":   strconv.FormatBool(os.Getenv("ALLOW_SIGNUP") == "1"),
		"currency": "EUR",
		"vatNote":  envOr("PRICE_NOTE", "zzgl. gesetzl. USt."),
	}
}

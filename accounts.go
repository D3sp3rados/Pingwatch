package main

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Pfade, die auch Nur-Lesen-Benutzer per POST/DELETE aufrufen dürfen
var readOnlyOK = map[string]bool{
	"/api/settings":       true, // eigenes Passwort
	"/api/events/ack":     true, // auch „Nur lesen“ darf quittieren
	"/api/app/device":     true,
	"/api/app/pushtest":   true,
	"/api/webpush":        true,
	"/api/me/notify":      true,
	"/api/me/profile":     true, // eigenes Konto
	"/api/assistant":      true, // Fragen stellen darf jeder
	"/api/billing/portal": false,
}

func (s *Server) registerTenantRoutes(mux *http.ServeMux, protect, super func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/signup", s.handleSignup)
	mux.HandleFunc("/api/plans", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"plans": publicPlans(), "provider": providerInfo()})
	})
	mux.HandleFunc("/api/me", protect(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.accountInfo(r)) }))
	mux.HandleFunc("/api/users", protect(s.handleUsers))
	mux.HandleFunc("/api/users/mail", protect(s.handleUserMail))
	mux.HandleFunc("/api/me/notify", protect(s.handleMyNotify))
	mux.HandleFunc("/api/me/profile", protect(s.handleMyProfile))
	mux.HandleFunc("/api/assistant", protect(s.handleAssistant))
	mux.HandleFunc("/api/requests", protect(s.handleRequests))
	mux.HandleFunc("/api/admin/orgs", super(s.handleAdminOrgs))
	mux.HandleFunc("/api/admin/switch", super(s.handleAdminSwitch))
	mux.HandleFunc("/api/admin/users", super(s.handleAdminUsers))
	mux.HandleFunc("/api/admin/events", super(s.handleAdminEvents))
	mux.HandleFunc("/api/billing/checkout", protect(s.handleBillingCheckout))
	mux.HandleFunc("/api/billing/change", protect(s.handleBillingChange))
	mux.HandleFunc("/api/billing/portal", protect(s.handleBillingPortal))
	mux.HandleFunc("/api/stripe/webhook", s.handleStripeWebhook)
	mux.HandleFunc("/api/alerts", protect(s.handleAlerts))
	mux.HandleFunc("/api/alerts/test", protect(s.handleAlertTest))
	mux.HandleFunc("/api/alerts/telegram", protect(s.handleTelegramChats))
	mux.HandleFunc("/api/report.pdf", protect(s.handleReport))
	mux.HandleFunc("/api/report/send", protect(s.handleReportSend))
}

func publicPlans() []Plan {
	var out []Plan
	for _, p := range plans {
		if p.Public {
			out = append(out, p)
		}
	}
	return out
}

// accountInfo: wer ist angemeldet, welche Org, welches Paket, welche Limits.
func (s *Server) accountInfo(r *http.Request) map[string]any {
	p := principal(r)
	me := map[string]any{"role": p.Role, "super": p.Super}
	if p.User != nil {
		me["email"] = p.User.Email
		me["name"] = p.User.Name
		me["id"] = p.User.ID
		if u := s.m.userByID(p.User.ID); u != nil && u.TempPW {
			me["tempPw"] = true
		}
	} else {
		me["email"] = "admin"
		me["name"] = "Betreiber"
	}
	out := map[string]any{"me": me, "provider": providerInfo(), "plans": publicPlans(),
		"billingEnabled": stripeEnabled()}
	if o := s.m.orgByID(p.Org); o != nil {
		sites, hosts := s.m.usage(o.ID)
		pl := planByID(o.Plan)
		out["org"] = map[string]any{
			"id": o.ID, "name": o.Name, "plan": o.Plan, "planName": pl.Name, "sites": o.Sites,
			"siteLimit": o.siteLimit(), "hostLimit": o.hostLimit(), "usedSites": sites, "usedHosts": hosts,
			"status": o.Status, "billing": o.Billing, "retention": o.retention(s.m.cfg.RetentionDays),
			"features": pl.Features, "minInterval": pl.MinInterval, "paidUntil": o.PaidUntil,
			"hasStripe": o.StripeCustomer != "", "email": o.Email,
		}
	}
	s.aiAccountInfo(p, s.m.orgByID(p.Org), out)
	if p.Super {
		var orgs []map[string]string
		s.m.mu.RLock()
		for _, o := range s.m.cfg.Orgs {
			orgs = append(orgs, map[string]string{"id": o.ID, "name": o.Name, "plan": o.Plan})
		}
		s.m.mu.RUnlock()
		sort.Slice(orgs, func(i, j int) bool { return strings.ToLower(orgs[i]["name"]) < strings.ToLower(orgs[j]["name"]) })
		out["orgs"] = orgs
	}
	return out
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "ungültige Anfrage", 400)
		return false
	}
	return true
}

// ---------- Benutzer der eigenen Org ----------

type userView struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Created   int64  `json:"created"`
	LastLogin int64  `json:"lastLogin"`
	Super     bool   `json:"super,omitempty"`
	Expires   int64  `json:"expires,omitempty"`
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		out := []userView{}
		m.mu.RLock()
		for _, u := range m.cfg.Users {
			if u.Org == p.Org {
				out = append(out, userView{u.ID, u.Email, u.Name, u.Role, u.Created, u.LastLogin, u.isSuper(), u.Expires})
			}
		}
		m.mu.RUnlock()
		writeJSON(w, out)
	case http.MethodPost:
		var req struct {
			ID, Email, Name, Role, Password string
			ResetPassword                   bool
			Super                           *bool  // Betreiber-Admin (nur in der Betreiber-Org)
			Expires                         *int64 // Zugang bis (ms), 0 = unbegrenzt
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Role != "owner" && req.Role != "admin" && req.Role != "viewer" {
			req.Role = "viewer"
		}
		if req.Role == "owner" && !p.isOwner() {
			http.Error(w, "Nur Inhaber können Inhaber festlegen", 403)
			return
		}
		email := strings.ToLower(strings.TrimSpace(req.Email))
		temp := ""
		m.mu.Lock()
		defer m.mu.Unlock()
		if req.ID == "" {
			if !validEmail(email) {
				http.Error(w, "E-Mail-Adresse ungültig", 400)
				return
			}
			for _, u := range m.cfg.Users {
				if strings.EqualFold(u.Email, email) {
					http.Error(w, "Diese E-Mail ist bereits vergeben", 409)
					return
				}
			}
			pw := req.Password
			if len(pw) < 8 {
				pw = tempPassword()
				temp = pw
			}
			u := User{ID: randHex(6), Email: email, Name: strings.TrimSpace(req.Name), Org: p.Org, Role: req.Role,
				PassHash: hashPassword(pw), Created: time.Now().UnixMilli(), TempPW: temp != ""}
			if req.Super != nil && p.Super && p.Org == defaultOrgID {
				u.Super = *req.Super
			}
			if req.Expires != nil {
				u.Expires = *req.Expires
			}
			m.cfg.Users = append(m.cfg.Users, u)
		} else {
			found := false
			for i := range m.cfg.Users {
				u := &m.cfg.Users[i]
				if u.ID != req.ID || u.Org != p.Org {
					continue
				}
				found = true
				if u.Role == "owner" && req.Role != "owner" && m.ownerCountLocked(p.Org) <= 1 {
					http.Error(w, "Mindestens ein Inhaber muss bleiben", 400)
					return
				}
				if u.Role == "owner" && !p.isOwner() {
					http.Error(w, "Nur Inhaber können Inhaber ändern", 403)
					return
				}
				u.Role = req.Role
				if n := strings.TrimSpace(req.Name); n != "" {
					u.Name = n
				}
				if req.ResetPassword {
					temp = tempPassword()
					u.PassHash = hashPassword(temp)
					u.TempPW = true
				}
				if req.Super != nil && p.Super && p.Org == defaultOrgID && (p.User == nil || p.User.ID != u.ID) {
					u.Super = *req.Super
				}
				if req.Expires != nil && (p.User == nil || p.User.ID != u.ID) {
					u.Expires = *req.Expires
				}
			}
			if !found {
				http.Error(w, "Benutzer unbekannt", 404)
				return
			}
		}
		m.saveConfigLocked()
		writeJSON(w, map[string]any{"ok": true, "tempPassword": temp})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		defer m.mu.Unlock()
		out := m.cfg.Users[:0]
		for _, u := range m.cfg.Users {
			if u.ID == id && u.Org == p.Org {
				if u.Role == "owner" && m.ownerCountLocked(p.Org) <= 1 {
					http.Error(w, "Der letzte Inhaber kann nicht gelöscht werden", 400)
					return
				}
				if p.User != nil && u.ID == p.User.ID {
					http.Error(w, "Sie können sich nicht selbst löschen", 400)
					return
				}
				continue
			}
			out = append(out, u)
		}
		m.cfg.Users = out
		m.saveConfigLocked()
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func (m *Monitor) ownerCountLocked(org string) int {
	n := 0
	for _, u := range m.cfg.Users {
		if u.Org == org && u.Role == "owner" {
			n++
		}
	}
	return n
}

// ---------- Registrierung ----------

var signupTmpl = template.Must(template.New("s").Parse(`<!doctype html><html lang="de"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>PingWacht – Konto erstellen</title>
<link rel="icon" href="/favicon.ico"><style>
:root{--bg:#f3f5f9;--card:#fff;--fg:#0f172a;--mut:#64748b;--line:#e2e8f0;--acc:#059669}
@media (prefers-color-scheme:dark){:root{--bg:#0b1120;--card:#111a2e;--fg:#e2e8f0;--mut:#8b9ab4;--line:#1e2a44;--acc:#34d399}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:15px/1.45 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
form{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:28px;width:100%;max-width:400px}
h1{font-size:20px;margin:0 0 4px}p{color:var(--mut);font-size:13.5px;margin:0 0 18px}label{font-size:13px;color:var(--mut)}
input{width:100%;margin:5px 0 14px;padding:10px 12px;border-radius:10px;border:1px solid var(--line);background:var(--bg);color:var(--fg);font-size:16px}
button{width:100%;padding:11px;border:0;border-radius:10px;background:var(--acc);color:#04130c;font-weight:600;font-size:15px;cursor:pointer}
.err{color:#ef4444;font-size:13px;margin:-4px 0 12px}.small{font-size:12px;color:var(--mut);margin-top:14px;text-align:center}a{color:var(--acc)}
.chk{display:flex;gap:8px;align-items:flex-start;font-size:12.5px;color:var(--mut);margin:-4px 0 14px}.chk input{width:auto;margin:3px 0 0}
</style></head><body><form method="post">
<h1>PingWacht – kostenlos starten</h1><p>Free-Paket: 1 Standort, 5 Hosts. Jederzeit erweiterbar.</p>
{{if .Err}}<div class="err">{{.Err}}</div>{{end}}
<label>Firma</label><input name="org" required maxlength="80" value="{{.Org}}">
<label>Ihr Name</label><input name="name" maxlength="80" value="{{.Name}}">
<label>E-Mail</label><input name="email" type="email" required value="{{.Email}}" autocomplete="email">
<label>Passwort (mind. 8 Zeichen)</label><input name="password" type="password" required minlength="8" autocomplete="new-password">
<label class="chk"><input type="checkbox" name="terms" required> <span>Ich akzeptiere die {{if .P.terms}}<a href="{{.P.terms}}" target="_blank">AGB</a> und die {{end}}<a href="{{.P.privacy}}" target="_blank">Datenschutzerklärung</a>.</span></label>
<button>Konto erstellen</button>
<div class="small">Schon registriert? <a href="/login">Anmelden</a> · Anbieter: {{.P.name}} · <a href="{{.P.imprint}}" target="_blank">Impressum</a></div>
</form></body></html>`))

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if providerInfo()["signup"] != "true" {
		http.Error(w, "Registrierung ist deaktiviert", 404)
		return
	}
	data := map[string]any{"P": providerInfo()}
	if r.Method == http.MethodPost {
		r.ParseForm()
		org := strings.TrimSpace(r.FormValue("org"))
		name := strings.TrimSpace(r.FormValue("name"))
		email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
		pw := r.FormValue("password")
		data["Org"], data["Name"], data["Email"] = org, name, email
		var err string
		switch {
		case org == "":
			err = "Bitte Firmennamen angeben."
		case !validEmail(email):
			err = "E-Mail-Adresse ungültig."
		case len(pw) < 8:
			err = "Passwort zu kurz (mind. 8 Zeichen)."
		case r.FormValue("terms") == "":
			err = "Bitte AGB und Datenschutz bestätigen."
		case s.m.userByEmail(email) != nil:
			err = "Diese E-Mail ist bereits registriert."
		}
		if err == "" {
			o := Org{ID: randHex(5), Name: org, Plan: "free", Created: time.Now().UnixMilli(), Email: email, Status: "active"}
			u := User{ID: randHex(6), Email: email, Name: name, Org: o.ID, Role: "owner", PassHash: hashPassword(pw), Created: o.Created}
			s.m.mu.Lock()
			s.m.cfg.Orgs = append(s.m.cfg.Orgs, o)
			s.m.cfg.Users = append(s.m.cfg.Users, u)
			s.m.saveConfigLocked()
			s.m.mu.Unlock()
			log.Printf("Neue Registrierung: %s (%s)", org, email)
			s.setSession(w, r, s.makeToken(u.ID, userRef(&u)))
			http.Redirect(w, r, "/#/settings", http.StatusFound)
			return
		}
		data["Err"] = err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	signupTmpl.Execute(w, data)
}

// ---------- Betreiber: Kundenverwaltung ----------

type orgView struct {
	Org
	UsedSites int    `json:"usedSites"`
	UsedHosts int    `json:"usedHosts"`
	Users     int    `json:"users"`
	Owner     string `json:"owner"`
	PlanName  string `json:"planName"`
	MRR       int    `json:"mrr"` // Cent pro Monat netto
}

func (s *Server) handleAdminOrgs(w http.ResponseWriter, r *http.Request) {
	m := s.m
	switch r.Method {
	case http.MethodGet:
		m.mu.RLock()
		orgs := append([]Org(nil), m.cfg.Orgs...)
		users := append([]User(nil), m.cfg.Users...)
		m.mu.RUnlock()
		out := []orgView{}
		for _, o := range orgs {
			v := orgView{Org: o, PlanName: planByID(o.Plan).Name}
			v.StripeCustomer, v.StripeSub, v.StripeItem = "", "", ""
			v.UsedSites, v.UsedHosts = m.usage(o.ID)
			for _, u := range users {
				if u.Org == o.ID {
					v.Users++
					if u.Role == "owner" && v.Owner == "" {
						v.Owner = u.Email
					}
				}
			}
			if o.Status != "canceled" {
				v.MRR = planByID(o.Plan).PriceCents * o.siteLimit()
			}
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
		if r.URL.Query().Get("format") == "csv" {
			// Abrechnungsliste für das eigene Rechnungsprogramm
			csvHeader(w, "pingwatch-abrechnung-"+time.Now().Format("2006-01")+".csv")
			fmt.Fprintln(w, "Kunde;Inhaber;Rechnungs-E-Mail;Paket;Abrechnung;Standorte gebucht;Standorte genutzt;Hosts;Preis je Standort (EUR);Betrag netto (EUR);Status;Kunde seit")
			for _, v := range out {
				if v.ID == defaultOrgID {
					continue
				}
				pl := planByID(v.Plan)
				st := v.Status
				if v.Disabled {
					st = "gesperrt"
				}
				fmt.Fprintf(w, "%s;%s;%s;%s;%s;%d;%d;%d;%s;%s;%s;%s\n", csvCell(v.Name), csvCell(v.Owner), csvCell(v.Email), pl.Name, v.Billing,
					v.siteLimit(), v.UsedSites, v.UsedHosts, euroDE(pl.PriceCents), euroDE(v.MRR), st, time.UnixMilli(v.Created).Format("02.01.2006"))
			}
			return
		}
		writeJSON(w, out)
	case http.MethodPost:
		var req struct {
			ID, Name, Plan, Billing, OwnerEmail, OwnerName, Email string
			Sites                                                 int
			Disabled                                              *bool
			Expires                                               *int64 // Testzugang bis (ms), 0 = unbegrenzt
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Plan != "" && planByID(req.Plan).ID != req.Plan {
			http.Error(w, "Paket unbekannt", 400)
			return
		}
		temp := ""
		m.mu.Lock()
		defer m.mu.Unlock()
		if req.ID == "" {
			if strings.TrimSpace(req.Name) == "" {
				http.Error(w, "Name fehlt", 400)
				return
			}
			o := Org{ID: randHex(5), Name: strings.TrimSpace(req.Name), Plan: req.Plan, Sites: req.Sites, Billing: req.Billing,
				Created: time.Now().UnixMilli(), Status: "active", Email: strings.TrimSpace(req.Email)}
			if req.Expires != nil {
				o.Expires = *req.Expires
			}
			if o.Plan == "" {
				o.Plan = "free"
			}
			if e := strings.ToLower(strings.TrimSpace(req.OwnerEmail)); e != "" {
				if !validEmail(e) {
					http.Error(w, "E-Mail des Inhabers ungültig", 400)
					return
				}
				for _, u := range m.cfg.Users {
					if strings.EqualFold(u.Email, e) {
						http.Error(w, "E-Mail ist bereits vergeben", 409)
						return
					}
				}
				temp = tempPassword()
				m.cfg.Users = append(m.cfg.Users, User{ID: randHex(6), Email: e, Name: req.OwnerName, Org: o.ID, Role: "owner",
					PassHash: hashPassword(temp), Created: o.Created, TempPW: true})
				if o.Email == "" {
					o.Email = e
				}
			}
			m.cfg.Orgs = append(m.cfg.Orgs, o)
			m.saveConfigLocked()
			writeJSON(w, map[string]any{"ok": true, "id": o.ID, "tempPassword": temp})
			return
		}
		for i := range m.cfg.Orgs {
			o := &m.cfg.Orgs[i]
			if o.ID != req.ID {
				continue
			}
			if n := strings.TrimSpace(req.Name); n != "" {
				o.Name = n
			}
			if req.Plan != "" {
				o.Plan = req.Plan
			}
			if req.Sites > 0 {
				o.Sites = req.Sites
			}
			if req.Billing != "" {
				o.Billing = req.Billing
			}
			if req.Disabled != nil && o.ID != defaultOrgID {
				o.Disabled = *req.Disabled
			}
			if req.Expires != nil && o.ID != defaultOrgID {
				o.Expires = *req.Expires
			}
			m.saveConfigLocked()
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		http.Error(w, "Kunde unbekannt", 404)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == defaultOrgID {
			http.Error(w, "Die Betreiber-Organisation kann nicht gelöscht werden", 400)
			return
		}
		if o := m.orgByID(id); o != nil && o.StripeSub != "" && o.Status != "canceled" {
			http.Error(w, "Kunde hat ein laufendes Abo – bitte zuerst in Stripe kündigen", 400)
			return
		}
		s.deleteOrg(id)
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func (s *Server) deleteOrg(id string) {
	m := s.m
	var hostIDs, siteIDs []string
	m.mu.RLock()
	for _, h := range m.cfg.Hosts {
		if h.Org == id {
			hostIDs = append(hostIDs, h.ID)
		}
	}
	for _, st := range m.cfg.Sites {
		if st.Org == id {
			siteIDs = append(siteIDs, st.ID)
		}
	}
	m.mu.RUnlock()
	for _, h := range hostIDs {
		m.DeleteHost(h)
	}
	for _, st := range siteIDs {
		m.DeleteSite(st)
	}
	if len(hostIDs) > 0 {
		m.store.DeleteSamples(hostIDs, 0, time.Now().Add(time.Hour).UnixMilli())
	}
	m.mu.Lock()
	keepU := m.cfg.Users[:0]
	for _, u := range m.cfg.Users {
		if u.Org != id {
			keepU = append(keepU, u)
		}
	}
	m.cfg.Users = keepU
	keepD := m.cfg.Devices[:0]
	for _, d := range m.cfg.Devices {
		if d.Org != id {
			keepD = append(keepD, d)
		}
	}
	m.cfg.Devices = keepD
	keepG := m.cfg.Groups[:0]
	for _, g := range m.cfg.Groups {
		if g.Org != id {
			keepG = append(keepG, g)
		}
	}
	m.cfg.Groups = keepG
	keepR := m.cfg.Requests[:0]
	for _, x := range m.cfg.Requests {
		if x.Org != id {
			keepR = append(keepR, x)
		}
	}
	m.cfg.Requests = keepR
	delete(m.cfg.UniFiKeys, id)
	keepM := m.cfg.Machines[:0]
	for _, x := range m.cfg.Machines {
		if x.Org != id {
			keepM = append(keepM, x)
		}
	}
	m.cfg.Machines = keepM
	keepC := m.cfg.Channels[:0]
	for _, c := range m.cfg.Channels {
		if c.Org != id {
			keepC = append(keepC, c)
		}
	}
	m.cfg.Channels = keepC
	keepO := m.cfg.Orgs[:0]
	for _, o := range m.cfg.Orgs {
		if o.ID != id {
			keepO = append(keepO, o)
		}
	}
	m.cfg.Orgs = keepO
	m.saveConfigLocked()
	m.mu.Unlock()
	m.evMu.Lock()
	var removed []int64
	keepE := m.events[:0]
	for _, e := range m.events {
		if e.Org == id {
			removed = append(removed, e.ID)
			continue
		}
		keepE = append(keepE, e)
	}
	m.events = keepE
	cp := append([]*Event(nil), keepE...)
	m.evMu.Unlock()
	m.store.DeleteEvents(removed)
	m.store.RewriteEvents(cp)
	log.Printf("Kunde %s gelöscht", id)
}

func (s *Server) handleAdminSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct{ Org string }
	if !readJSON(w, r, &req) {
		return
	}
	if s.m.orgByID(req.Org) == nil {
		http.Error(w, "Kunde unbekannt", 404)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "pw_org", Value: req.Org, Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- Aufbewahrung je Paket ----------

// applyOrgRetention löscht bei Paketen mit kürzerer Aufbewahrung ältere Daten.
func (m *Monitor) applyOrgRetention() {
	m.mu.RLock()
	global := m.cfg.RetentionDays
	type job struct {
		hosts []string
		ret   int
		org   string
	}
	var jobs []job
	for _, o := range m.cfg.Orgs {
		ret := o.retention(global)
		if ret >= global {
			continue
		}
		j := job{ret: ret, org: o.ID}
		for _, h := range m.cfg.Hosts {
			if h.Org == o.ID {
				j.hosts = append(j.hosts, h.ID)
			}
		}
		jobs = append(jobs, j)
	}
	m.mu.RUnlock()
	now := time.Now()
	for _, j := range jobs {
		cut := now.AddDate(0, 0, -j.ret).UnixMilli()
		if len(j.hosts) > 0 {
			m.store.DeleteSamples(j.hosts, now.AddDate(0, 0, -global-2).UnixMilli(), cut)
		}
		var removed []int64
		m.evMu.Lock()
		keep := m.events[:0]
		for _, e := range m.events {
			if e.Org == j.org && e.End > 0 && e.Start < cut {
				removed = append(removed, e.ID)
				continue
			}
			keep = append(keep, e)
		}
		m.events = keep
		m.evMu.Unlock()
		m.store.DeleteEvents(removed)
	}
}

func csvCell(s string) string {
	if strings.ContainsAny(s, ";\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func euroDE(cents int) string {
	return strings.Replace(fmt.Sprintf("%.2f", float64(cents)/100), ".", ",", 1)
}

// POST /api/users/mail {id, password} – Zugangsdaten per E-Mail an den Benutzer schicken.
func (s *Server) handleUserMail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p := principal(r)
	var u *User
	s.m.mu.RLock()
	for i := range s.m.cfg.Users {
		x := s.m.cfg.Users[i]
		if (x.Org == p.Org || p.Super) && (x.ID == req.ID || (req.ID == "" && strings.EqualFold(x.Email, req.Email))) {
			u = &x
		}
	}
	s.m.mu.RUnlock()
	if u == nil || req.Password == "" {
		http.Error(w, "Benutzer unbekannt", 404)
		return
	}
	o := s.m.orgByID(u.Org)
	orgName := ""
	if o != nil {
		orgName = o.Name
	}
	link := baseURL(r) + "/login"
	from := "Ihr Administrator"
	if p.User != nil {
		from = p.User.Email
		if p.User.Name != "" {
			from = p.User.Name
		}
	}
	role := map[string]string{"owner": "Inhaber", "admin": "Admin", "viewer": "Nur lesen"}[u.Role]
	exp := ""
	if u.Expires > 0 {
		exp = "\nDer Zugang ist gültig bis " + time.UnixMilli(u.Expires).Format("02.01.2006") + "."
	}
	txt := fmt.Sprintf("Guten Tag%s,\n\n%s hat Ihnen einen Zugang zu PingWacht (%s) eingerichtet.\n\nAnmeldung: %s\nE-Mail: %s\nVorläufiges Passwort: %s\nRolle: %s%s\n\nBitte ändern Sie das Passwort nach der ersten Anmeldung unter Einstellungen → Allgemein.\n",
		nameSuffix(u.Name), from, orgName, link, u.Email, req.Password, role, exp)
	htm := fmt.Sprintf(`<div style="font-family:-apple-system,Segoe UI,Arial,sans-serif;max-width:560px">Guten Tag%s,<br><br>%s hat Ihnen einen Zugang zu <b>PingWacht</b> (%s) eingerichtet.<br><br>
<table style="font-size:14px"><tr><td style="color:#64748b;padding-right:12px">Anmeldung</td><td><a href="%s">%s</a></td></tr><tr><td style="color:#64748b">E-Mail</td><td>%s</td></tr><tr><td style="color:#64748b">Vorläufiges Passwort</td><td><b style="font-family:monospace">%s</b></td></tr><tr><td style="color:#64748b">Rolle</td><td>%s</td></tr></table>%s
<p>Bitte ändern Sie das Passwort nach der ersten Anmeldung unter <i>Einstellungen → Allgemein</i>.</p></div>`,
		html.EscapeString(nameSuffix(u.Name)), html.EscapeString(from), html.EscapeString(orgName), link, link, html.EscapeString(u.Email),
		html.EscapeString(req.Password), role, strings.ReplaceAll(html.EscapeString(exp), "\n", "<br>"))
	if err := sendMail([]string{u.Email}, "Ihr Zugang zu PingWacht", txt, htm, nil); err != nil {
		http.Error(w, "E-Mail konnte nicht gesendet werden: "+err.Error(), 502)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func nameSuffix(n string) string {
	if n = strings.TrimSpace(n); n != "" {
		return " " + n
	}
	return ""
}

// GET /api/admin/events – Ereignisse aller Kunden (nur Betreiber), mit Kundennamen.
func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	limit := int(qInt(r, "limit", 2000))
	if limit < 1 || limit > 10000 {
		limit = 2000
	}
	evs := s.m.queryEvents("", "", "", parseTypes(r), qInt(r, "from", 0), qInt(r, "to", 0), limit)
	names := map[string]string{}
	s.m.mu.RLock()
	for _, o := range s.m.cfg.Orgs {
		names[o.ID] = o.Name
	}
	s.m.mu.RUnlock()
	writeJSON(w, map[string]any{"events": evs, "orgs": names})
}

// ---------- Betreiber: Benutzer eines Kunden anzeigen / Passwort zurücksetzen ----------
// GET /api/admin/users?org=<id> · POST /api/admin/users {id} → neues vorläufiges Passwort
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	m := s.m
	switch r.Method {
	case http.MethodGet:
		org := r.URL.Query().Get("org")
		type uv struct {
			ID        string `json:"id"`
			Email     string `json:"email"`
			Name      string `json:"name"`
			Role      string `json:"role"`
			LastLogin int64  `json:"lastLogin"`
			Super     bool   `json:"super"`
		}
		out := []uv{}
		m.mu.RLock()
		for _, u := range m.cfg.Users {
			if u.Org == org {
				out = append(out, uv{u.ID, u.Email, u.Name, u.Role, u.LastLogin, u.Super})
			}
		}
		m.mu.RUnlock()
		writeJSON(w, out)
	case http.MethodPost:
		var req struct {
			ID string `json:"id"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		temp := tempPassword()
		email := ""
		m.mu.Lock()
		for i := range m.cfg.Users {
			if m.cfg.Users[i].ID == req.ID {
				m.cfg.Users[i].PassHash = hashPassword(temp)
				m.cfg.Users[i].TempPW = true
				email = m.cfg.Users[i].Email
			}
		}
		if email != "" {
			m.saveConfigLocked()
		}
		m.mu.Unlock()
		if email == "" {
			http.Error(w, "Benutzer unbekannt", 404)
			return
		}
		log.Printf("Betreiber: Passwort für %s zurückgesetzt", email)
		writeJSON(w, map[string]any{"ok": true, "email": email, "tempPassword": temp})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

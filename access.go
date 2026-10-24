package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Rechteverwaltung:
//  - Ein Benutzer kann zu mehreren Konten gehören (User.Org = Hauptkonto, User.Memberships = weitere).
//  - Je Konto kann der Zugriff auf bestimmte Kunden, Standorte, Gruppen, Hosts oder Rechner beschränkt
//    werden (AccessRule), jeweils nur lesend oder als Admin. Ohne Regeln gilt die Rolle für das ganze Konto.
//  - Eingeschränkte Benutzer erreichen nur die Schnittstellen in scopedPaths; dort wird je Objekt geprüft.

type AccessRule struct {
	Kind  string `json:"kind"` // customer, site, group, host, machine
	ID    string `json:"id"`
	Admin bool   `json:"admin,omitempty"`
}

type Membership struct {
	Org    string       `json:"org"`
	Role   string       `json:"role"` // owner, admin, viewer
	Access []AccessRule `json:"access,omitempty"`
}

// membership liefert Rolle und Zugriffsregeln des Benutzers in einem Konto.
func (u *User) membership(org string) (role string, access []AccessRule, ok bool) {
	if u.Org == org {
		return u.Role, u.Access, true
	}
	for _, ms := range u.Memberships {
		if ms.Org == org {
			return ms.Role, ms.Access, true
		}
	}
	return "", nil, false
}

// orgIDs: alle Konten des Benutzers (Hauptkonto zuerst).
func (u *User) orgIDs() []string {
	out := []string{u.Org}
	for _, ms := range u.Memberships {
		if ms.Org != u.Org {
			out = append(out, ms.Org)
		}
	}
	return out
}

func validAccessKind(k string) bool {
	switch k {
	case "customer", "site", "group", "host", "machine":
		return true
	}
	return false
}

// scope: berechnete Rechte eines eingeschränkten Benutzers (0 = nichts, 1 = lesen, 2 = Admin).
type scope struct {
	host, machine         map[string]int
	site                  map[string]int // sichtbar (direkt oder über enthaltene Hosts/Rechner)
	siteDirect            map[string]int // Standort selbst freigegeben (UniFi, Agent, Rechner-Einstellungen)
	group, groupRule      map[string]int
	customer, customerRul map[string]int
}

func lvl(admin bool) int {
	if admin {
		return 2
	}
	return 1
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *Monitor) buildScope(org string, rules []AccessRule) *scope {
	sc := &scope{host: map[string]int{}, machine: map[string]int{}, site: map[string]int{}, siteDirect: map[string]int{},
		group: map[string]int{}, groupRule: map[string]int{}, customer: map[string]int{}, customerRul: map[string]int{}}
	rs, rh, rm := map[string]int{}, map[string]int{}, map[string]int{}
	for _, r := range rules {
		l := lvl(r.Admin)
		switch r.Kind {
		case "customer":
			sc.customerRul[r.ID] = maxi(sc.customerRul[r.ID], l)
		case "site":
			rs[r.ID] = maxi(rs[r.ID], l)
		case "group":
			sc.groupRule[r.ID] = maxi(sc.groupRule[r.ID], l)
		case "host":
			rh[r.ID] = maxi(rh[r.ID], l)
		case "machine":
			rm[r.ID] = maxi(rm[r.ID], l)
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	siteCust := map[string]string{}
	for _, s := range m.cfg.Sites {
		if s.Org != org {
			continue
		}
		siteCust[s.ID] = s.Customer
		if d := maxi(rs[s.ID], sc.customerRul[s.Customer]); d > 0 && (rs[s.ID] > 0 || s.Customer != "") {
			sc.siteDirect[s.ID] = d
			sc.site[s.ID] = d
		}
	}
	see := func(site, group string) {
		if sc.site[site] == 0 {
			sc.site[site] = 1
		}
		if group != "" && sc.group[group] == 0 {
			sc.group[group] = 1
		}
	}
	for _, h := range m.cfg.Hosts {
		if h.Org != org {
			continue
		}
		r := maxi(maxi(rh[h.ID], sc.groupRule[h.Group]), sc.siteDirect[h.Site])
		if r > 0 {
			sc.host[h.ID] = r
			see(h.Site, h.Group)
		}
	}
	for _, mc := range m.cfg.Machines {
		if mc.Org != org {
			continue
		}
		r := maxi(maxi(rm[mc.ID], sc.groupRule[mc.Group]), sc.siteDirect[mc.Site])
		if r > 0 {
			sc.machine[mc.ID] = r
			see(mc.Site, mc.Group)
		}
	}
	for id, l := range sc.groupRule {
		sc.group[id] = maxi(sc.group[id], l)
	}
	for id, l := range sc.customerRul {
		sc.customer[id] = maxi(sc.customer[id], l)
	}
	for sid := range sc.site {
		if c := siteCust[sid]; c != "" && sc.customer[c] == 0 {
			sc.customer[c] = 1
		}
	}
	return sc
}

// ---- Prüfungen am Principal (ohne Einschränkung: Rolle des Kontos) ----

func (p *Principal) full(admin bool) bool { return !admin || p.canEdit() }

func (p *Principal) canHost(id string, admin bool) bool {
	if p.Scope == nil {
		return p.full(admin)
	}
	return p.Scope.host[id] >= lvl(admin)
}

func (p *Principal) canMachine(id string, admin bool) bool {
	if p.Scope == nil {
		return p.full(admin)
	}
	return p.Scope.machine[id] >= lvl(admin)
}

// canSite: Standort selbst (UniFi, Agent, Einstellungen).
func (p *Principal) canSite(id string, admin bool) bool {
	if p.Scope == nil {
		return p.full(admin)
	}
	return p.Scope.siteDirect[id] >= lvl(admin)
}

func (p *Principal) seesSite(id string) bool {
	return p.Scope == nil || id == "cloud" || p.Scope.site[id] > 0
}

func (p *Principal) seesGroup(id string) bool { return p.Scope == nil || p.Scope.group[id] > 0 }

func (p *Principal) seesCustomer(id string) bool { return p.Scope == nil || p.Scope.customer[id] > 0 }

// canEvent: darf das Ereignis sehen (bzw. quittieren, admin=true)?
func (p *Principal) canEvent(e *Event, admin bool) bool {
	if p.Scope == nil {
		return p.full(admin) || !admin
	}
	switch {
	case strings.HasPrefix(e.HostID, "m:"):
		return p.canMachine(e.HostID[2:], admin)
	case e.HostID != "":
		return p.canHost(e.HostID, admin)
	default:
		return p.canSite(e.Site, admin)
	}
}

// canMute: Stummschalten/Pausieren für einen Bereich.
func (p *Principal) canMute(kind, id string) bool {
	if p.Scope == nil {
		return p.canEdit()
	}
	switch kind {
	case "host":
		return p.canHost(id, true)
	case "machine":
		return p.canMachine(id, true)
	case "site":
		return p.canSite(id, true)
	case "group":
		return p.Scope.groupRule[id] >= 2
	case "customer":
		return p.Scope.customerRul[id] >= 2
	}
	return false
}

// scopedPaths: Schnittstellen, die eingeschränkte Benutzer überhaupt erreichen (Rest: 403).
// Die Handler prüfen dort je Objekt.
var scopedPaths = map[string]bool{
	"/api/state": true, "/api/overview": true, "/api/series": true, "/api/stats": true,
	"/api/events": true, "/api/events/ack": true, "/api/hosts": true, "/api/mute": true,
	"/api/fritz": true, "/api/snmp": true, "/api/trace": true,
	"/api/machine": true, "/api/machine/inventory": true, "/api/actions": true, "/api/scripts": true,
	"/api/unifi": true, "/api/unifi/ignore": true, "/api/unifi/alert": true,
	"/api/me": true, "/api/me/notify": true, "/api/me/profile": true, "/api/me/org": true,
	"/api/totp": true, "/api/passkey": true, "/api/terms/accept": true,
	"/api/app/device": true, "/api/app/pushtest": true, "/api/webpush": true,
}

// scopedWrite: Änderungen, die eingeschränkte Benutzer (mit Admin-Recht am Objekt) machen dürfen.
var scopedWrite = map[string]bool{
	"/api/hosts": true, "/api/mute": true, "/api/events/ack": true, "/api/trace": true,
	"/api/machine": true, "/api/actions": true, "/api/unifi/ignore": true, "/api/unifi/alert": true, "/api/unifi/protect": true, "/api/me/org": true,
}

func scopedAllowed(r *http.Request) bool {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/passkey/") {
		return true
	}
	if !strings.HasPrefix(path, "/api/") {
		return true // Seiten, Login, Downloads
	}
	if !scopedPaths[path] {
		return false
	}
	if r.Method != http.MethodGet && (path == "/api/scripts" || path == "/api/unifi") {
		return false
	}
	return true
}

// ---- Kontowechsel für Benutzer mit mehreren Konten ----

// POST /api/me/org {org}
func (s *Server) handleMyOrg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	var req struct{ Org string }
	if !readJSON(w, r, &req) {
		return
	}
	p := principal(r)
	ok := p.Super && s.m.orgByID(req.Org) != nil
	if !ok && p.User != nil {
		if u := s.m.userByID(p.User.ID); u != nil {
			if _, _, member := u.membership(req.Org); member && accessError(u, s.m.orgByID(req.Org)) == nil {
				ok = true
			}
		}
	}
	if !ok {
		http.Error(w, "Kein Zugriff auf dieses Konto", 403)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "pw_org", Value: req.Org, Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]bool{"ok": true})
}

// applyMembership wählt für normale Benutzer das Konto (Cookie/Header) und setzt Rolle und Einschränkung.
func (s *Server) applyMembership(r *http.Request, p *Principal) {
	if p.Super || p.User == nil {
		return
	}
	u := s.m.userByID(p.User.ID)
	if u == nil {
		return
	}
	want := r.URL.Query().Get("org")
	if want == "" {
		want = r.Header.Get("X-Org")
	}
	if want == "" {
		if c, err := r.Cookie("pw_org"); err == nil {
			want = c.Value
		}
	}
	org := u.Org
	if want != "" && want != u.Org {
		if _, _, ok := u.membership(want); ok && accessError(u, s.m.orgByID(want)) == nil {
			org = want
		}
	}
	role, access, _ := u.membership(org)
	p.Org, p.Role = org, role
	if len(access) > 0 && role != "owner" {
		p.Scope = s.m.buildScope(org, access)
	}
}

// userOrgs: Konten eines Benutzers für die Auswahl oben.
func (s *Server) userOrgs(u *User) []map[string]string {
	var out []map[string]string
	for _, id := range u.orgIDs() {
		if o := s.m.orgByID(id); o != nil {
			role, _, _ := u.membership(id)
			out = append(out, map[string]string{"id": o.ID, "name": o.Name, "role": role})
		}
	}
	return out
}

// ---- Benutzer: Rollen und Zugriffe je Konto pflegen ----

// cleanAccess prüft die Regeln gegen die Objekte des Kontos.
func (m *Monitor) cleanAccessLocked(org string, in []AccessRule) []AccessRule {
	exists := func(kind, id string) bool {
		switch kind {
		case "customer":
			for _, c := range m.cfg.Customers {
				if c.ID == id && c.Org == org {
					return true
				}
			}
		case "site":
			for _, s := range m.cfg.Sites {
				if s.ID == id && s.Org == org {
					return true
				}
			}
		case "group":
			for _, g := range m.cfg.Groups {
				if g.ID == id && g.Org == org {
					return true
				}
			}
		case "host":
			for _, h := range m.cfg.Hosts {
				if h.ID == id && h.Org == org {
					return true
				}
			}
		case "machine":
			for _, mc := range m.cfg.Machines {
				if mc.ID == id && mc.Org == org {
					return true
				}
			}
		}
		return false
	}
	seen := map[string]int{}
	var out []AccessRule
	for _, r := range in {
		if !validAccessKind(r.Kind) || !exists(r.Kind, r.ID) {
			continue
		}
		k := r.Kind + ":" + r.ID
		if i, ok := seen[k]; ok {
			out[i].Admin = out[i].Admin || r.Admin
			continue
		}
		seen[k] = len(out)
		out = append(out, AccessRule{Kind: r.Kind, ID: r.ID, Admin: r.Admin})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// ---- Hosts in ein anderes Konto verschieben (Betreiber) ----

// POST /api/admin/move {from, to, group?, hosts?[]}
func (s *Server) handleAdminMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From, To, Group, Site string
		Hosts                 []string
	}
	if !readJSON(w, r, &req) {
		return
	}
	if s.m.orgByID(req.From) == nil || s.m.orgByID(req.To) == nil || req.From == req.To {
		http.Error(w, "Konto unbekannt", 400)
		return
	}
	if req.Site != "" {
		h, mc, err := s.m.moveSite(req.From, req.To, req.Site)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "hosts": h, "machines": mc})
		return
	}
	n, err := s.m.moveHosts(req.From, req.To, req.Group, req.Hosts)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "moved": n})
}

// moveHosts verschiebt Cloud-Hosts (nur Standort „cloud“) samt Gruppe und Ereignissen in ein anderes Konto.
// Hosts an Agent-Standorten bleiben, weil der Agent zum alten Konto gehört.
func (m *Monitor) moveHosts(from, to, group string, ids []string) (int, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	m.mu.Lock()
	gName := ""
	if group != "" {
		for _, g := range m.cfg.Groups {
			if g.ID == group && g.Org == from {
				gName = g.Name
			}
		}
		if gName == "" {
			m.mu.Unlock()
			return 0, errString("Gruppe unbekannt")
		}
	}
	// Zielgruppe gleichen Namens verwenden oder anlegen
	newGroup := ""
	if gName != "" {
		for _, g := range m.cfg.Groups {
			if g.Org == to && strings.EqualFold(g.Name, gName) {
				newGroup = g.ID
			}
		}
		if newGroup == "" {
			g := Group{ID: randHex(5), Org: to, Name: gName, Created: time.Now().UnixMilli()}
			m.cfg.Groups = append(m.cfg.Groups, g)
			newGroup = g.ID
		}
	}
	var moved []HostCfg
	movedID := map[string]bool{}
	for _, hc := range m.cfg.Hosts {
		if hc.Org != from || hc.Site != "cloud" {
			continue
		}
		if !(want[hc.ID] || (group != "" && hc.Group == group)) {
			continue
		}
		hc.Org, hc.Group, hc.Parent = to, newGroup, ""
		moved = append(moved, hc)
		movedID[hc.ID] = true
	}
	m.mu.Unlock()
	for _, hc := range moved {
		m.UpsertHost(hc)
	}
	m.evMu.Lock()
	var changed []Event
	for _, e := range m.events {
		if movedID[e.HostID] && e.Org == from {
			e.Org = to
			changed = append(changed, *e)
		}
	}
	evs := make([]*Event, len(m.events))
	copy(evs, m.events)
	m.evMu.Unlock()
	for _, e := range changed {
		m.store.AppendEvent(e) // Datenbank: Upsert
	}
	m.store.RewriteEvents(evs) // Dateien: neu schreiben
	// leere Quellgruppe entfernen
	if group != "" {
		m.mu.Lock()
		used := false
		for _, hc := range m.cfg.Hosts {
			if hc.Group == group {
				used = true
			}
		}
		for _, mc := range m.cfg.Machines {
			if mc.Group == group {
				used = true
			}
		}
		if !used {
			keep := m.cfg.Groups[:0]
			for _, g := range m.cfg.Groups {
				if g.ID != group {
					keep = append(keep, g)
				}
			}
			m.cfg.Groups = keep
		}
		m.saveConfigLocked()
		m.mu.Unlock()
	}
	return len(moved), nil
}

type errString string

func (e errString) Error() string { return string(e) }

// ---- Filter für eingeschränkte Benutzer ----

func filterGroups(p *Principal, in []Group) []Group {
	if p.Scope == nil {
		return in
	}
	out := []Group{}
	for _, g := range in {
		if p.seesGroup(g.ID) {
			out = append(out, g)
		}
	}
	return out
}

func filterCustomers(p *Principal, in []Customer) []Customer {
	if p.Scope == nil {
		return in
	}
	out := []Customer{}
	for _, c := range in {
		if p.seesCustomer(c.ID) {
			out = append(out, c)
		}
	}
	return out
}

func filterMachines(p *Principal, in []machineView) []machineView {
	if p.Scope == nil {
		return in
	}
	out := []machineView{}
	for _, mv := range in {
		if p.canMachine(mv.ID, false) {
			out = append(out, mv)
		}
	}
	return out
}

// activeMutesFor: aktive Stummschaltungen – eingeschränkte Benutzer sehen nur die, die ihre Objekte betreffen.
func (m *Monitor) activeMutesFor(p *Principal) []muteView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	all := m.activeMutesLocked(p.Org, time.Now())
	if p.Scope == nil {
		return all
	}
	rel := map[string]bool{}
	for _, w := range m.cfg.Maints {
		if w.Org != p.Org {
			continue
		}
		if len(w.Hosts) == 0 {
			rel[w.ID] = true
			continue
		}
		for _, h := range w.Hosts {
			if (strings.HasPrefix(h, "m:") && p.canMachine(h[2:], false)) || p.canHost(h, false) ||
				(strings.HasPrefix(h, "s:") && p.canSite(h[2:], false)) {
				rel[w.ID] = true
				break
			}
		}
	}
	out := []muteView{}
	for _, v := range all {
		if rel[v.ID] {
			out = append(out, v)
		}
	}
	return out
}

// scopedHostWriteOK: Host anlegen/ändern nur mit Admin-Recht am Host bzw. am Ziel (Standort oder Gruppe).
func (s *Server) scopedHostWriteOK(p *Principal, hc HostCfg) bool {
	if hc.ID != "" {
		if !p.canHost(hc.ID, true) {
			return false
		}
		if old := s.m.host(hc.ID); old != nil {
			old.mu.Lock()
			same := old.cfg.Site == hc.Site && old.cfg.Group == hc.Group
			old.mu.Unlock()
			if same {
				return true
			}
		}
	}
	// neuer Host oder verschoben: Ziel muss freigegeben sein
	if hc.Site != "" && hc.Site != "cloud" && p.canSite(hc.Site, true) {
		return true
	}
	return hc.Group != "" && p.Scope.groupRule[hc.Group] >= 2
}

// noticeVisible: darf ein Benutzer diese Meldung bekommen (Konto und ggf. Einschränkung)?
func (m *Monitor) noticeVisible(u *User, n notice) bool {
	if u == nil {
		return true
	}
	role, access, ok := u.membership(n.org)
	if !ok {
		return u.isSuper()
	}
	if len(access) == 0 || role == "owner" {
		return true
	}
	p := &Principal{Org: n.org, Role: role, Scope: m.buildScope(n.org, access)}
	return p.canEvent(&Event{HostID: n.hostID, Site: n.site}, false)
}

// moveSite verschiebt einen Standort (Agent) samt seiner Hosts, Rechner und Ereignisse in ein anderes Konto.
// Gruppen werden im Zielkonto über den Namen zugeordnet bzw. angelegt; die Kundenzuordnung entfällt.
func (m *Monitor) moveSite(from, to, siteID string) (int, int, error) {
	m.mu.Lock()
	idx := -1
	for i, sc := range m.cfg.Sites {
		if sc.ID == siteID && sc.Org == from {
			idx = i
		}
	}
	if idx < 0 || siteID == "cloud" {
		m.mu.Unlock()
		return 0, 0, errString("Standort unbekannt")
	}
	m.cfg.Sites[idx].Org = to
	m.cfg.Sites[idx].Customer = ""
	groupMap := map[string]string{}
	mapGroup := func(old string) string {
		if old == "" {
			return ""
		}
		if g, ok := groupMap[old]; ok {
			return g
		}
		name := ""
		for _, g := range m.cfg.Groups {
			if g.ID == old {
				name = g.Name
			}
		}
		res := ""
		if name != "" {
			for _, g := range m.cfg.Groups {
				if g.Org == to && strings.EqualFold(g.Name, name) {
					res = g.ID
				}
			}
			if res == "" {
				g := Group{ID: randHex(5), Org: to, Name: name, Created: time.Now().UnixMilli()}
				m.cfg.Groups = append(m.cfg.Groups, g)
				res = g.ID
			}
		}
		groupMap[old] = res
		return res
	}
	var hosts []HostCfg
	ids := map[string]bool{}
	for _, hc := range m.cfg.Hosts {
		if hc.Site == siteID && hc.Org == from {
			hc.Org, hc.Group, hc.Parent = to, mapGroup(hc.Group), ""
			hosts = append(hosts, hc)
			ids[hc.ID] = true
		}
	}
	nm := 0
	for i := range m.cfg.Machines {
		mc := &m.cfg.Machines[i]
		if mc.Site == siteID && mc.Org == from {
			mc.Org, mc.Group = to, mapGroup(mc.Group)
			ids["m:"+mc.ID] = true
			nm++
		}
	}
	m.cfg.Version++
	m.saveConfigLocked()
	m.mu.Unlock()
	for _, hc := range hosts {
		m.UpsertHost(hc)
	}
	m.evMu.Lock()
	var changed []Event
	for _, e := range m.events {
		if e.Org == from && (e.Site == siteID || ids[e.HostID]) {
			e.Org = to
			changed = append(changed, *e)
		}
	}
	evs := make([]*Event, len(m.events))
	copy(evs, m.events)
	m.evMu.Unlock()
	for _, e := range changed {
		m.store.AppendEvent(e)
	}
	m.store.RewriteEvents(evs)
	return len(hosts), nm, nil
}

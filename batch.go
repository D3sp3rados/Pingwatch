package main

// Sammelmeldung: Fallen innerhalb einer Minute mehrere Hosts/Geräte eines Kontos aus,
// gehen die ersten (Schwelle-1) wie bisher einzeln raus, alle weiteren werden gesammelt
// und als EINE Meldung je Empfänger verschickt (sobald 20 s nichts Neues kam, spätestens
// nach 90 s). Dasselbe für die Entwarnungen. Jeder Empfänger bekommt nur die Hosts,
// die er ohnehin bekommen würde; ist es für ihn nur einer, kommt die normale Einzelmeldung.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var batchQuietTest = 20 * time.Second

const (
	batchDefaultMin = 3
	batchWindow     = 60 * time.Second
	batchMax        = 90 * time.Second
)

type nbItem struct {
	n          notice
	at         time.Time
	early      bool // schon einzeln verschickt
	summarized bool // stand schon in einer Sammelmeldung
}

type nbBucket struct {
	recent  []*nbItem
	pending []*nbItem
	first   time.Time
	timer   *time.Timer
}

var nbState = struct {
	sync.Mutex
	b   map[string]*nbBucket // org|down / org|up
	reg map[string][]*nbItem // b:<id> → Mitglieder einer Sammelmeldung
	seq int
}{b: map[string]*nbBucket{}, reg: map[string][]*nbItem{}}

// batchSide: "down"/"up" für sammelbare Meldungen, sonst "".
func batchSide(n notice) string {
	switch n.kind {
	case "outage":
		return "down"
	case "recovered":
		return "up"
	case "devoff":
		if strings.HasPrefix(n.title, "🔋") {
			return ""
		}
		return "down"
	case "devon":
		return "up"
	}
	return ""
}

func (m *Monitor) batchMin(org string) int {
	o := m.orgByID(org)
	if o == nil || o.BatchMin == nil {
		return batchDefaultMin
	}
	return *o.BatchMin
}

// noticeLabel: Gerätename aus dem Meldungstitel.
func noticeLabel(n notice) string {
	t := n.title
	if i := strings.Index(t, " "); i > 0 && i <= 5 { // Emoji vorne
		t = t[i+1:]
	}
	if strings.HasPrefix(t, "UniFi") {
		if i := strings.Index(t, ": "); i > 0 {
			t = t[i+2:]
		}
	}
	for _, s := range []string{" antwortet nicht", " wieder erreichbar", " wieder online", " offline"} {
		t = strings.TrimSuffix(t, s)
	}
	return t
}

func sameDevice(a, b notice) bool {
	if a.hostID != "" || b.hostID != "" {
		return a.hostID == b.hostID
	}
	return a.site == b.site && noticeLabel(a) == noticeLabel(b)
}

// batchIn: true = Meldung wird gesammelt (oder verworfen), nicht sofort verschicken.
func (m *Monitor) batchIn(n notice) bool {
	side := batchSide(n)
	if side == "" || n.org == "" || strings.HasPrefix(n.hostID, "b:") {
		return false
	}
	min := m.batchMin(n.org)
	if min < 2 {
		return false
	}
	now := time.Now()
	nbState.Lock()
	defer nbState.Unlock()
	// Ausfall noch im Sammeltopf und schon wieder da → beides weglassen
	if side == "up" {
		if d := nbState.b[n.org+"|down"]; d != nil {
			for i, it := range d.pending {
				if sameDevice(it.n, n) {
					d.pending = append(d.pending[:i], d.pending[i+1:]...)
					return true
				}
			}
		}
	}
	key := n.org + "|" + side
	b := nbState.b[key]
	if b == nil {
		b = &nbBucket{}
		nbState.b[key] = b
	}
	keep := b.recent[:0]
	for _, it := range b.recent {
		if now.Sub(it.at) < batchWindow {
			keep = append(keep, it)
		}
	}
	b.recent = keep
	if len(b.pending) > 0 {
		b.pending = append(b.pending, &nbItem{n: n, at: now})
		if now.Sub(b.first) < batchMax-batchQuietTest {
			b.timer.Reset(batchQuietTest)
		}
		return true
	}
	if len(b.recent)+1 < min {
		b.recent = append(b.recent, &nbItem{n: n, at: now, early: true})
		return false
	}
	b.pending = []*nbItem{{n: n, at: now}}
	b.first = now
	b.timer = time.AfterFunc(batchQuietTest, func() { m.batchFlush(key, side) })
	return true
}

func (m *Monitor) batchFlush(key, side string) {
	nbState.Lock()
	b := nbState.b[key]
	if b == nil || len(b.pending) == 0 {
		nbState.Unlock()
		return
	}
	var cp []*nbItem // Kopie: early = schon einzeln gemeldet
	for _, it := range b.recent {
		if !it.summarized {
			c := *it
			c.early = true
			cp = append(cp, &c)
			it.summarized = true
		}
	}
	for _, it := range b.pending {
		c := *it
		c.early = false
		cp = append(cp, &c)
		it.early, it.summarized = true, true
		b.recent = append(b.recent, it)
	}
	b.pending = nil
	nbState.seq++
	id := "b:" + strconv.Itoa(nbState.seq)
	nbState.reg[id] = cp
	org := cp[0].n.org
	nbState.Unlock()
	time.AfterFunc(15*time.Minute, func() { nbState.Lock(); delete(nbState.reg, id); nbState.Unlock() })
	kind := "outage"
	if side == "up" {
		kind = "recovered"
	}
	m.notifyNow(notice{kind, id, "Sammelmeldung", "", org, ""})
}

// batchList: die Einzelmeldungen hinter einer Meldung (bei normalen Meldungen sie selbst).
func batchList(n notice) []notice {
	if !strings.HasPrefix(n.hostID, "b:") {
		return []notice{n}
	}
	nbState.Lock()
	defer nbState.Unlock()
	var out []notice
	for _, it := range nbState.reg[n.hostID] {
		out = append(out, it.n)
	}
	return out
}

func noticeKey(n notice) string { return n.kind + "|" + n.hostID + "|" + n.site + "|" + n.title }

// pickFor: was ein Empfänger bekommt. ok prüft eine Einzelmeldung.
// Ergebnis: Einzelmeldung, Sammelmeldung oder nichts.
func (m *Monitor) pickFor(n notice, ok func(notice) bool) (notice, bool) {
	if !strings.HasPrefix(n.hostID, "b:") {
		return n, ok(n)
	}
	nbState.Lock()
	all := append([]*nbItem(nil), nbState.reg[n.hostID]...)
	nbState.Unlock()
	var sel []*nbItem
	fresh := 0
	for _, it := range all {
		if ok(it.n) {
			sel = append(sel, it)
			if !it.early {
				fresh++
			}
		}
	}
	if fresh == 0 {
		return n, false
	}
	if len(sel) == 1 {
		return sel[0].n, true
	}
	return m.summarize(n, sel), true
}

func (m *Monitor) summarize(n notice, sel []*nbItem) notice {
	down := n.kind == "outage"
	// nach Kunde/Gruppe bzw. UniFi-Standort ordnen
	m.mu.RLock()
	gname := map[string]string{}
	for _, g := range m.cfg.Groups {
		gname[g.ID] = g.Name
	}
	where := func(x notice) string {
		if x.hostID != "" {
			if g := gname[m.hostGroupLocked(x.hostID)]; g != "" {
				return g
			}
			return "ohne Gruppe"
		}
		for _, s := range m.cfg.Sites {
			if s.ID == x.site {
				return "UniFi " + s.Name
			}
		}
		return "UniFi"
	}
	type grp struct {
		name  string
		names []string
	}
	gs := map[string]*grp{}
	var order []string
	early := 0
	first, last := sel[0].at, sel[0].at
	for _, it := range sel {
		w := where(it.n)
		if gs[w] == nil {
			gs[w] = &grp{name: w}
			order = append(order, w)
		}
		gs[w].names = append(gs[w].names, noticeLabel(it.n))
		if it.early {
			early++
		}
		if it.at.Before(first) {
			first = it.at
		}
		if it.at.After(last) {
			last = it.at
		}
	}
	m.mu.RUnlock()
	sort.SliceStable(order, func(i, j int) bool { return len(gs[order[i]].names) > len(gs[order[j]].names) })
	var lines []string
	for _, w := range order {
		g := gs[w]
		names := g.names
		more := ""
		if len(names) > 15 {
			more = fmt.Sprintf(" … +%d weitere", len(names)-15)
			names = names[:15]
		}
		lines = append(lines, fmt.Sprintf("%s (%d): %s%s", g.name, len(g.names), strings.Join(names, ", "), more))
	}
	span := first.Format("15:04")
	if last.Format("15:04") != span {
		span += "–" + last.Format("15:04")
	}
	var title, tail string
	if down {
		title = fmt.Sprintf("🔴 Größerer Ausfall: %d Geräte antworten nicht", len(sel))
		tail = "Ausgefallen " + span
	} else {
		title = fmt.Sprintf("🟢 %d Geräte wieder erreichbar", len(sel))
		tail = "Wieder da " + span
	}
	if early > 0 {
		tail += fmt.Sprintf(" · %d davon schon einzeln gemeldet", early)
	}
	return notice{n.kind, "", title, strings.Join(lines, "\n") + "\n" + tail, n.org, ""}
}

// ---------- API: /api/org/batch ----------

func (s *Server) handleOrgBatch(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]int{"min": s.m.batchMin(p.Org)})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET/POST", 405)
		return
	}
	if !p.canEdit() {
		http.Error(w, "keine Berechtigung", 403)
		return
	}
	var req struct{ Min int }
	if !readJSON(w, r, &req) {
		return
	}
	if req.Min < 0 || req.Min > 100 {
		http.Error(w, "Wert 0–100", 400)
		return
	}
	v := req.Min
	s.m.mu.Lock()
	for i := range s.m.cfg.Orgs {
		if s.m.cfg.Orgs[i].ID == p.Org {
			s.m.cfg.Orgs[i].BatchMin = &v
		}
	}
	s.m.saveConfigLocked()
	s.m.mu.Unlock()
	writeJSON(w, map[string]int{"min": v})
}

// batchEarly: Einzelmeldung x stand in der Sammelmeldung n nur zur Übersicht (schon einzeln verschickt).
func batchEarly(n, x notice) bool {
	nbState.Lock()
	defer nbState.Unlock()
	for _, it := range nbState.reg[n.hostID] {
		if noticeKey(it.n) == noticeKey(x) {
			return it.early
		}
	}
	return false
}

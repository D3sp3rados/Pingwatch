package main

import (
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Öffentliche Statusseite eines Kunden (Paket Pro): /status/<name>

type StatusPage struct {
	On    bool     `json:"on"`
	Slug  string   `json:"slug"`
	Title string   `json:"title"`
	Hosts []string `json:"hosts"` // angezeigte Hosts in dieser Reihenfolge
	Note  string   `json:"note,omitempty"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,40}$`)

type statusDay struct {
	Day   string  `json:"day"`
	Avail float64 `json:"avail"` // -1 = keine Daten
	Down  int64   `json:"down"`
}

type statusHost struct {
	Name   string      `json:"name"`
	State  string      `json:"state"` // ok, down, maint, unknown
	Uptime float64     `json:"uptime"`
	Days   []statusDay `json:"days"`
}

type statusIncident struct {
	Name  string `json:"name"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

type statusData struct {
	Title     string           `json:"title"`
	Note      string           `json:"note,omitempty"`
	Overall   string           `json:"overall"`
	Hosts     []statusHost     `json:"hosts"`
	Incidents []statusIncident `json:"incidents"`
	Updated   int64            `json:"updated"`
}

var statusCache = struct {
	sync.Mutex
	m map[string]struct {
		at   time.Time
		days map[string][]statusDay
	}
}{m: map[string]struct {
	at   time.Time
	days map[string][]statusDay
}{}}

const statusDays = 90

func (m *Monitor) statusFor(o *Org) statusData {
	sp := o.StatusPage
	d := statusData{Title: sp.Title, Note: sp.Note, Updated: time.Now().UnixMilli()}
	if d.Title == "" {
		d.Title = o.Name
	}
	now := time.Now()
	// Tagesverfügbarkeit (teuer) höchstens alle 10 Minuten neu berechnen
	statusCache.Lock()
	c, ok := statusCache.m[o.ID]
	statusCache.Unlock()
	if !ok || time.Since(c.at) > 10*time.Minute {
		c.at, c.days = now, map[string][]statusDay{}
		for _, id := range sp.Hosts {
			h := m.host(id)
			if h == nil {
				continue
			}
			ds := m.dayStatsAt(h, now, statusDays)
			days := make([]statusDay, 0, len(ds))
			for i := len(ds) - 1; i >= 0; i-- {
				days = append(days, statusDay{Day: ds[i].Day, Avail: dayAvail(ds[i]), Down: ds[i].Downtime})
			}
			c.days[id] = days
		}
		statusCache.Lock()
		statusCache.m[o.ID] = c
		statusCache.Unlock()
	}
	nDown, nMaint := 0, 0
	names := map[string]string{}
	for _, id := range sp.Hosts {
		h := m.host(id)
		if h == nil {
			continue
		}
		h.mu.Lock()
		cfg := h.cfg
		down := h.oOut != nil
		fresh := h.lastT > 0 && now.UnixMilli()-h.lastT < int64(cfg.IntervalMs)*5+60000
		h.mu.Unlock()
		if cfg.Org != o.ID {
			continue
		}
		names[id] = cfg.Name
		sh := statusHost{Name: cfg.Name, State: "ok", Days: c.days[id], Uptime: -1}
		switch {
		case m.inMaint(o.ID, id, now):
			sh.State = "maint"
			nMaint++
		case down:
			sh.State = "down"
			nDown++
		case !fresh || cfg.Paused:
			sh.State = "unknown"
		}
		var tot, dn int64
		for _, x := range sh.Days {
			if x.Avail >= 0 {
				tot += 86400000
				dn += x.Down
			}
		}
		if tot > 0 {
			sh.Uptime = 1 - float64(dn)/float64(tot)
		}
		d.Hosts = append(d.Hosts, sh)
	}
	switch {
	case nDown > 0 && nDown == len(d.Hosts):
		d.Overall = "down"
	case nDown > 0:
		d.Overall = "partial"
	case nMaint > 0:
		d.Overall = "maint"
	default:
		d.Overall = "ok"
	}
	for _, e := range m.queryEvents(o.ID, "", "", map[string]bool{"outage": true}, now.AddDate(0, 0, -14).UnixMilli(), 0, 500) {
		if e.Maint || e.Cause != "" || names[e.HostID] == "" {
			continue
		}
		end := e.End
		if end > 0 && end-e.Start < 60000 {
			continue // Kurzausfälle unter 1 Minute nicht öffentlich auflisten
		}
		d.Incidents = append(d.Incidents, statusIncident{Name: names[e.HostID], Start: e.Start, End: end})
		if len(d.Incidents) >= 30 {
			break
		}
	}
	return d
}

func (m *Monitor) orgBySlug(slug string) *Org {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.cfg.Orgs {
		o := m.cfg.Orgs[i]
		if o.StatusPage.On && o.StatusPage.Slug == slug && !o.Disabled {
			return &o
		}
	}
	return nil
}

// GET /status/<slug> (HTML) und /status/<slug>.json
func (s *Server) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/status/")
	asJSON := strings.HasSuffix(slug, ".json")
	slug = strings.TrimSuffix(slug, ".json")
	o := s.m.orgBySlug(slug)
	if o == nil || !planHas(o.Plan, "status") {
		http.Error(w, "Statusseite nicht gefunden", 404)
		return
	}
	d := s.m.statusFor(o)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Robots-Tag", "noindex")
	if asJSON {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		writeJSON(w, d)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	statusTmpl.Execute(w, map[string]any{"D": d, "Provider": providerInfo()})
}

// GET/POST /api/statuspage
func (s *Server) handleStatusConfig(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	o := s.m.orgByID(p.Org)
	if o == nil {
		http.Error(w, "Kunde unbekannt", 404)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"page": o.StatusPage, "allowed": planHas(o.Plan, "status")})
		return
	}
	if !planHas(o.Plan, "status") {
		http.Error(w, "Die öffentliche Statusseite gibt es im Paket Pro", 402)
		return
	}
	var sp StatusPage
	if !readJSON(w, r, &sp) {
		return
	}
	sp.Slug = strings.ToLower(strings.TrimSpace(sp.Slug))
	sp.Title = strings.TrimSpace(sp.Title)
	sp.Note = strings.TrimSpace(sp.Note)
	if len(sp.Note) > 500 {
		sp.Note = sp.Note[:500]
	}
	if !slugRe.MatchString(sp.Slug) {
		http.Error(w, "Adresse: 3–40 Zeichen, nur a–z, 0–9 und Bindestrich", 400)
		return
	}
	own := map[string]bool{}
	for _, h := range s.m.hostsOf(p.Org) {
		own[h.cfg.ID] = true
	}
	hs := []string{}
	for _, id := range sp.Hosts {
		if own[id] && len(hs) < 50 {
			hs = append(hs, id)
		}
	}
	sp.Hosts = hs
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.cfg.Orgs {
		if x.ID != p.Org && x.StatusPage.Slug == sp.Slug && sp.Slug != "" {
			http.Error(w, "Diese Adresse ist schon vergeben", 409)
			return
		}
	}
	for i := range m.cfg.Orgs {
		if m.cfg.Orgs[i].ID == p.Org {
			m.cfg.Orgs[i].StatusPage = sp
		}
	}
	m.saveConfigLocked()
	statusCache.Lock()
	delete(statusCache.m, p.Org)
	statusCache.Unlock()
	writeJSON(w, map[string]any{"ok": true, "url": baseURL(r) + "/status/" + sp.Slug})
}

func availColor(a float64) string {
	switch {
	case a < 0:
		return "nd"
	case a >= 0.999:
		return "g"
	case a >= 0.99:
		return "y"
	case a >= 0.95:
		return "o"
	}
	return "r"
}

var statusTmpl = template.Must(template.New("s").Funcs(template.FuncMap{
	"pct": func(v float64) string {
		if v < 0 {
			return "–"
		}
		return strings.Replace(fmt.Sprintf("%.2f %%", v*100), ".", ",", 1)
	},
	"col": availColor,
	"dayDE": func(s string) string {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return s
		}
		return t.Format("02.01.2006")
	},
	"ts": func(ms int64) string { return time.UnixMilli(ms).Format("02.01.2006 15:04") },
	"dur": func(a, b int64) string {
		if b == 0 {
			b = time.Now().UnixMilli()
		}
		return fmtDurShort(b - a)
	},
}).Parse(`<!doctype html><html lang="de"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="60"><title>{{.D.Title}} – Status</title><meta name="robots" content="noindex">
<style>
:root{--bg:#f6f7f9;--panel:#fff;--fg:#0f172a;--mut:#64748b;--line:#e2e8f0;--g:#16a34a;--y:#84cc16;--o:#f59e0b;--r:#dc2626;--nd:#cbd5e1;--b:#2563eb}
@media (prefers-color-scheme:dark){:root{--bg:#0b1120;--panel:#111827;--fg:#e5e7eb;--mut:#94a3b8;--line:#1f2937;--nd:#334155}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Arial,sans-serif}
.w{max-width:860px;margin:0 auto;padding:28px 16px 40px}h1{font-size:26px;margin:0 0 18px}
.ban{border-radius:12px;padding:16px 18px;color:#fff;font-weight:600;font-size:17px;margin-bottom:18px}
.ban.ok{background:var(--g)}.ban.partial{background:var(--o)}.ban.down{background:var(--r)}.ban.maint{background:var(--b)}
.note{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:12px 16px;margin-bottom:18px;white-space:pre-wrap}
.p{background:var(--panel);border:1px solid var(--line);border-radius:12px;overflow:hidden}
.h{padding:14px 16px;border-top:1px solid var(--line)}.h:first-child{border-top:0}
.hh{display:flex;align-items:center;gap:10px;margin-bottom:8px}.hh b{flex:1}.st{font-size:13px;font-weight:600}
.st.ok{color:var(--g)}.st.down{color:var(--r)}.st.maint{color:var(--b)}.st.unknown{color:var(--mut)}
.bars{display:flex;gap:2px;height:30px}.bars i{flex:1;border-radius:2px;background:var(--nd)}
.bars i.g{background:var(--g)}.bars i.y{background:var(--y)}.bars i.o{background:var(--o)}.bars i.r{background:var(--r)}
.ax{display:flex;justify-content:space-between;font-size:12px;color:var(--mut);margin-top:4px}
h2{font-size:17px;margin:28px 0 10px}.inc{padding:10px 16px;border-top:1px solid var(--line);display:flex;gap:12px;flex-wrap:wrap}.inc:first-child{border-top:0}
.inc span{color:var(--mut);font-size:14px}.f{margin-top:28px;font-size:12.5px;color:var(--mut);text-align:center}.f a{color:inherit}
@media (max-width:600px){.bars i:nth-child(-n+60){display:none}.ax .l90{display:none}.ax .l30{display:inline!important}}
</style></head><body><div class="w">
<h1>{{.D.Title}}</h1>
{{if eq .D.Overall "ok"}}<div class="ban ok">Alle Systeme betriebsbereit</div>{{else if eq .D.Overall "maint"}}<div class="ban maint">Wartungsarbeiten laufen</div>{{else if eq .D.Overall "down"}}<div class="ban down">Störung</div>{{else}}<div class="ban partial">Teilweise Störung</div>{{end}}
{{if .D.Note}}<div class="note">{{.D.Note}}</div>{{end}}
<div class="p">{{range .D.Hosts}}<div class="h"><div class="hh"><b>{{.Name}}</b><span class="st {{.State}}">{{if eq .State "ok"}}Betriebsbereit{{else if eq .State "down"}}Störung{{else if eq .State "maint"}}Wartung{{else}}Unbekannt{{end}}</span></div>
<div class="bars">{{range .Days}}<i class="{{col .Avail}}" title="{{dayDE .Day}}: {{pct .Avail}}"></i>{{end}}</div>
<div class="ax"><span class="l90">vor 90 Tagen</span><span class="l30" style="display:none">vor 30 Tagen</span><span>{{pct .Uptime}} verfügbar</span><span>heute</span></div></div>
{{else}}<div class="h">Keine Systeme ausgewählt.</div>{{end}}</div>
<h2>Vorfälle der letzten 14 Tage</h2>
<div class="p">{{range .D.Incidents}}<div class="inc"><b>{{.Name}}</b><span>{{ts .Start}} · {{if .End}}Dauer {{dur .Start .End}}{{else}}seit {{dur .Start .End}} – wird bearbeitet{{end}}</span></div>
{{else}}<div class="inc"><span>Keine Vorfälle.</span></div>{{end}}</div>
<div class="f">Stand {{ts .D.Updated}} · aktualisiert sich jede Minute · Überwachung mit PingWacht von <a href="{{.Provider.url}}" rel="noopener">{{.Provider.name}}</a></div>
</div></body></html>`))

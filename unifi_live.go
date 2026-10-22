package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// UniFi-Livestatus: Der Agent fragt die UniFi-Konsole am Standort jede Minute ab (Integration-API)
// und meldet alle Geräte. Der Server erkennt Geräte, die offline gehen, und alarmiert.

type UniFiCfg struct {
	On   bool   `json:"on"`
	URL  string `json:"url"`
	Key  string `json:"key,omitempty"`
	Site string `json:"site,omitempty"` // UniFi-Site (leer = alle)
	// Quelle: "" = lokal über den Agenten, "cloud" = zentral über den Cloud-Proxy von Ubiquiti
	Mode        string `json:"mode,omitempty"`
	Console     string `json:"console,omitempty"`     // Konsolen-ID (Site Manager)
	ConsoleName string `json:"consoleName,omitempty"` // Anzeige
	KeySet      bool   `json:"keySet,omitempty"`      // nur Ausgabe: Key am Standort hinterlegt
	// WAN-Anschlüsse, die nicht überwacht werden (z. B. vorbereitet, aber ungenutzt): "WAN3"
	WanIgnore []string `json:"wanIgnore,omitempty"`
}

// ignoresWAN: WAN-Anschluss in der Ignorier-Liste des Standorts?
func (u *UniFiCfg) ignoresWAN(name string) bool {
	if u == nil {
		return false
	}
	for _, x := range u.WanIgnore {
		if strings.EqualFold(strings.TrimSpace(x), name) {
			return true
		}
	}
	return false
}

// parseWANList: "WAN3, wan4" → ["WAN3","WAN4"]
func parseWANList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		f = strings.ToUpper(strings.TrimSpace(f))
		if f == "WAN1" {
			f = "WAN"
		}
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

type unifiSiteState struct {
	At      int64                 `json:"at"`
	Err     string                `json:"err,omitempty"`
	ErrN    int                   `json:"-"`
	Devices map[string]*unifiLive `json:"-"`
	open    map[string]*Event     // MAC → offenes Ereignis
	wanOpen map[string]*Event     // MAC|WAN → offenes Ereignis
	errEv   *Event                // Konsole nicht erreichbar
}

// unifiPoint: Zustand eines Geräts in einer Minute (für den 24-h-Verlauf).
type unifiPoint struct {
	T    int64
	Up   bool
	N    int8
	Lost int8
	Avg  float32
}

type unifiLive struct {
	unifiDevice
	Since  int64        `json:"since"` // Status seit
	At     int64        `json:"-"`     // letzte Meldung
	hist   []unifiPoint // 24 h, eine Minute je Punkt
	boots  []int64      // Neustarts (Zeitpunkte)
	wanLat map[string][]float32
}

var unifiLiveState = struct {
	sync.Mutex
	bySite map[string]*unifiSiteState
}{bySite: map[string]*unifiSiteState{}}

func unifiOffline(state string) bool {
	switch strings.ToUpper(state) {
	case "OFFLINE", "CONNECTION_INTERRUPTED", "ISOLATED", "DISCONNECTED":
		return true
	}
	return false
}

func devLabel(d unifiDevice) string {
	if d.Name != "" {
		return d.Name
	}
	return strings.TrimSpace(d.Model + " " + d.MAC)
}

// ingestUniFi verarbeitet eine Abfrage des Agenten.
func (m *Monitor) ingestUniFi(site SiteCfg, devs []unifiDevice, errText string) {
	now := time.Now()
	nowMs := now.UnixMilli()
	inMaint := m.inMaint(site.Org, "", now)
	unifiLiveState.Lock()
	st := unifiLiveState.bySite[site.ID]
	if st == nil {
		st = &unifiSiteState{Devices: map[string]*unifiLive{}, open: map[string]*Event{}, wanOpen: map[string]*Event{}}
		unifiLiveState.bySite[site.ID] = st
	}
	st.At = nowMs
	var starts, ends []Event
	var notes []notice
	if errText != "" {
		st.Err = errText
		st.ErrN++
		if st.ErrN == 3 && st.errEv == nil { // 3 Minuten in Folge keine Antwort
			e := m.newSiteEvent(site, "device", "UniFi-Konsole", nowMs, inMaint)
			st.errEv = e
			starts = append(starts, *e)
		}
		unifiLiveState.Unlock()
		m.unifiNotify(site, starts, nil, errText)
		return
	}
	st.Err, st.ErrN = "", 0
	if st.errEv != nil {
		ends = append(ends, m.closeSiteEvent(st.errEv, nowMs))
		st.errEv = nil
	}
	// ignorierte und nie verbundene WAN-Anschlüsse gar nicht erst verarbeiten (keine Meldung, keine Anzeige).
	// Auch serverseitig, damit ältere Agenten nicht nachgerüstet werden müssen. Ein Anschluss, der
	// schon einmal angezeigt wurde, bleibt (sonst würde ein echter Ausfall verschluckt).
	for i := range devs {
		k := devs[i].MAC
		if k == "" {
			k = devs[i].Name
		}
		known := map[string]bool{}
		if o := st.Devices[k]; o != nil {
			for _, w := range o.WAN {
				known[w.Name] = true
			}
		}
		ws := devs[i].WAN[:0:0]
		for _, w := range devs[i].WAN {
			if site.UniFi.ignoresWAN(w.Name) || (!known[w.Name] && !w.Up && w.IP == "" && w.Avail <= 0) {
				continue
			}
			ws = append(ws, w)
		}
		devs[i].WAN = ws
	}
	seen := map[string]bool{}
	first := len(st.Devices) == 0
	cut := nowMs - 24*3600*1000
	for _, d := range devs {
		key := d.MAC
		if key == "" {
			key = d.Name
		}
		seen[key] = true
		old := st.Devices[key]
		cur := old
		if old == nil {
			cur = &unifiLive{unifiDevice: d, Since: nowMs, wanLat: map[string][]float32{}}
			st.Devices[key] = cur
		} else {
			if old.State != d.State {
				cur.Since = nowMs
			}
			// Neustart: Laufzeit kleiner als erwartet
			if old.Uptime > 0 && d.Uptime > 0 && old.At > 0 {
				expected := old.Uptime + (nowMs-old.At)/1000
				if d.Uptime+120 < expected {
					bootAt := nowMs - d.Uptime*1000
					cur.boots = append(cur.boots, bootAt)
					e := m.newDevEvent(site, "reboot", devLabel(d), key, bootAt, inMaint)
					cp := m.closeSiteEvent(e, bootAt)
					if !inMaint {
						notes = append(notes, notice{"reboot", "", "🔄 UniFi: " + devLabel(d) + " neu gestartet",
							fmt.Sprintf("Neustart um %s (%s)", time.UnixMilli(bootAt).Format("15:04"), site.Name) + unifiVia(site), site.Org})
					}
					_ = cp
				}
			}
			// WAN-Leitungen
			oldW := map[string]unifiWAN{}
			for _, w := range old.WAN {
				oldW[w.Name] = w
			}
			for _, w := range d.WAN {
				wk := key + "|" + w.Name
				ow, had := oldW[w.Name]
				switch {
				case had && ow.Up && !w.Up && st.wanOpen[wk] == nil:
					e := m.newDevEvent(site, "wan", devLabel(d)+" · "+w.Name, key, nowMs, inMaint)
					st.wanOpen[wk] = e
					if !inMaint {
						notes = append(notes, notice{"wandown", "", "🔴 " + w.Name + " offline: " + devLabel(d), "Leitung " + w.Name + " ist getrennt (" + site.Name + ")" + unifiVia(site), site.Org})
					}
				case w.Up && st.wanOpen[wk] != nil:
					ce := m.closeSiteEvent(st.wanOpen[wk], nowMs)
					delete(st.wanOpen, wk)
					if ce.Notified {
						notes = append(notes, notice{"wanup", "", "🟢 " + w.Name + " wieder online: " + devLabel(d), "War " + fmtDurShort(ce.End-ce.Start) + " getrennt" + unifiVia(site), site.Org})
					}
				}
				if w.Latency > 0 {
					l := append(cur.wanLat[w.Name], float32(w.Latency))
					if len(l) > 1440 {
						l = l[len(l)-1440:]
					}
					cur.wanLat[w.Name] = l
				}
			}
			cur.unifiDevice = d
		}
		cur.At = nowMs
		// Verlauf
		pt := unifiPoint{T: nowMs, Up: !unifiOffline(d.State)}
		if d.Ping != nil {
			pt.N, pt.Lost, pt.Avg = int8(d.Ping.N), int8(d.Ping.Lost), float32(d.Ping.Avg)
		}
		cur.hist = append(cur.hist, pt)
		if i := sort.Search(len(cur.hist), func(i int) bool { return cur.hist[i].T >= cut }); i > 0 {
			cur.hist = append([]unifiPoint(nil), cur.hist[i:]...)
		}
		for len(cur.boots) > 0 && cur.boots[0] < cut {
			cur.boots = cur.boots[1:]
		}
		off := unifiOffline(d.State)
		e := st.open[key]
		switch {
		case off && e == nil && !first:
			ne := m.newDevEvent(site, "device", devLabel(d), key, nowMs, inMaint)
			st.open[key] = ne
			starts = append(starts, *ne)
		case off && e == nil && first:
			// beim ersten Abruf bereits offline: Ereignis ohne Alarm anlegen
			st.open[key] = m.newDevEvent(site, "device", devLabel(d), key, nowMs, true)
		case !off && e != nil:
			ends = append(ends, m.closeSiteEvent(e, nowMs))
			delete(st.open, key)
		}
	}
	for key := range st.Devices {
		if !seen[key] { // aus der Konsole entfernt
			delete(st.Devices, key)
			if e := st.open[key]; e != nil {
				m.closeSiteEvent(e, nowMs)
				delete(st.open, key)
			}
		}
	}
	unifiLiveState.Unlock()
	m.unifiNotify(site, starts, ends, "")
	for _, n := range notes {
		m.notify(n)
	}
}

// loadUniFiBoots: Neustarts der letzten 24 h nach einem Server-Neustart aus den Ereignissen übernehmen.
func (m *Monitor) unifiBootsFromEvents(site, dev string, since int64) []int64 {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	var out []int64
	for _, e := range m.events {
		if e.Type == "reboot" && e.Site == site && e.Dev == dev && e.Start >= since {
			out = append(out, e.Start)
		}
	}
	return out
}

func (m *Monitor) newDevEvent(site SiteCfg, typ, name, dev string, at int64, maint bool) *Event {
	e := m.newSiteEvent(site, typ, name, at, maint)
	m.evMu.Lock()
	e.Dev = dev
	m.evMu.Unlock()
	return e
}

func (m *Monitor) newSiteEvent(site SiteCfg, typ, name string, at int64, maint bool) *Event {
	m.evMu.Lock()
	m.nextEv++
	e := &Event{ID: m.nextEv, Site: site.ID, HostName: name, Org: site.Org, Type: typ, Start: at, Last: at, Maint: maint, Decided: true, Notified: !maint}
	m.events = append(m.events, e)
	m.evMu.Unlock()
	return e
}

func (m *Monitor) closeSiteEvent(e *Event, at int64) Event {
	m.evMu.Lock()
	e.End, e.Last = at, at
	cp := *e
	m.evMu.Unlock()
	m.store.AppendEvent(cp)
	return cp
}

// unifiVia: Herkunft der Prüfung im Meldungstext (nur bei der Cloud-Abfrage, Agent-Texte bleiben wie bisher).
func unifiVia(site SiteCfg) string {
	if site.UniFi.isCloud() {
		return "\nGeprüft von: UniFi-Cloud"
	}
	return ""
}

func (m *Monitor) unifiNotify(site SiteCfg, starts, ends []Event, errText string) {
	for _, e := range starts {
		if e.Maint {
			continue
		}
		body := "Gerät meldet sich nicht mehr bei der UniFi-Konsole (" + site.Name + ")"
		if errText != "" {
			body = "Der Agent erreicht die UniFi-Konsole nicht: " + errText
			if site.UniFi.isCloud() {
				body = "Die UniFi-Konsole ist über die UniFi-Cloud nicht erreichbar (Internet beim Kunden weg?): " + errText
			}
		}
		m.notify(notice{"devoff", "", "🔴 UniFi: " + e.HostName + " offline", body + unifiVia(site), site.Org})
	}
	for _, e := range ends {
		if !e.Notified {
			continue
		}
		m.notify(notice{"devon", "", "🟢 UniFi: " + e.HostName + " wieder online",
			fmt.Sprintf("War %s offline (%s)", fmtDurShort(e.End-e.Start), site.Name) + unifiVia(site), site.Org})
	}
}

// ---------- API ----------

// POST /api/agent/unifi {devices, error}
func (s *Server) handleAgentUniFi(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Devices []unifiDevice `json:"devices"`
		Error   string        `json:"error"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	if site.UniFi == nil || !site.UniFi.On || site.UniFi.isCloud() {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	s.m.ingestUniFi(site, req.Devices, req.Error)
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/unifi?site= → Geräteliste · POST /api/unifi {site, on, url, key, usite}
func (s *Server) handleUniFi(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		site := s.siteFor(r, r.URL.Query().Get("site"))
		if site == nil {
			http.Error(w, "Standort unbekannt", 404)
			return
		}
		writeJSON(w, s.m.unifiSiteView(site))
		return
	}
	var req struct {
		Site, URL, Key, USite string
		On                    bool
		Mode                  string // "" lokal (Agent), "cloud" (UniFi-Cloud)
		Console, ConsoleName  string
		ClearKey              bool   // eigenen Standort-Key entfernen (dann gilt der Key des Kunden)
		WanIgnore             string // „WAN3, WAN4“ – nicht überwachen
	}
	if !readJSON(w, r, &req) {
		return
	}
	site := s.siteFor(r, req.Site)
	if site == nil {
		http.Error(w, "Standort unbekannt", 404)
		return
	}
	cloud := req.Mode == unifiModeCloud
	if site.Kind == siteKindUniFi {
		cloud = true // Standort ohne Agent: nur Cloud möglich
	}
	u := strings.TrimRight(strings.TrimSpace(req.URL), "/")
	if u != "" && !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if req.On && !cloud && !strings.HasPrefix(u, "https://") {
		http.Error(w, "Adresse der UniFi-Konsole angeben, z. B. https://192.168.1.1", 400)
		return
	}
	if req.On && cloud && strings.TrimSpace(req.Console) == "" {
		http.Error(w, "Bitte eine Konsole auswählen („Konsolen laden“)", 400)
		return
	}
	m := s.m
	m.mu.Lock()
	for i := range m.cfg.Sites {
		sc := &m.cfg.Sites[i]
		if sc.ID != site.ID {
			continue
		}
		key := strings.TrimSpace(req.Key)
		// Key nur weiterverwenden, wenn die Quelle gleich bleibt (lokaler Konsolen-Key ≠ Cloud-Key)
		if key == "" && !req.ClearKey && sc.UniFi != nil && sc.UniFi.isCloud() == cloud {
			key = sc.UniFi.Key
		}
		if req.On && key == "" && !cloud {
			m.mu.Unlock()
			http.Error(w, "API-Schlüssel fehlt", 400)
			return
		}
		if req.On && key == "" && cloud && m.cfg.UniFiKeys[sc.Org] == "" {
			m.mu.Unlock()
			http.Error(w, "Kein UniFi-Cloud-Key – bitte oben unter „UniFi-Cloud (ohne Agent)“ eintragen oder hier einen Key angeben", 400)
			return
		}
		nc := &UniFiCfg{On: req.On, URL: u, Key: key, Site: strings.TrimSpace(req.USite), WanIgnore: parseWANList(req.WanIgnore)}
		if cloud {
			nc.Mode, nc.URL = unifiModeCloud, ""
			nc.Console, nc.ConsoleName = strings.TrimSpace(req.Console), strings.TrimSpace(req.ConsoleName)
		}
		if old := sc.UniFi; old != nil && (old.isCloud() != nc.isCloud() || old.Console != nc.Console) {
			unifiLiveState.Lock()
			delete(unifiLiveState.bySite, sc.ID) // andere Quelle: Livestatus neu aufbauen
			unifiLiveState.Unlock()
		}
		sc.UniFi = nc
	}
	m.cfg.Version++
	m.saveConfigLocked()
	m.mu.Unlock()
	if cloud && req.On {
		unifiCloudPoll.Lock()
		delete(unifiCloudPoll.next, site.ID)
		unifiCloudPoll.Unlock()
		go func() { // erste Abfrage sofort statt in bis zu einer Minute
			if sc, ok := m.siteByID(site.ID); ok {
				m.unifiCloudPollSite(sc)
			}
		}()
	}
	if !req.On {
		unifiLiveState.Lock()
		delete(unifiLiveState.bySite, site.ID)
		unifiLiveState.Unlock()
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type unifiSummary struct {
	On          bool        `json:"on"`
	Mode        string      `json:"mode,omitempty"` // "cloud" = über die UniFi-Cloud
	Console     string      `json:"console,omitempty"`
	ConsoleName string      `json:"consoleName,omitempty"`
	KeySet      bool        `json:"keySet,omitempty"` // eigener Key am Standort
	URL         string      `json:"url,omitempty"`
	Site        string      `json:"site,omitempty"`
	WanIgnore   []string    `json:"wanIgnore,omitempty"`
	At          int64       `json:"at,omitempty"`
	Err         string      `json:"err,omitempty"`
	Total       int         `json:"total"`
	Offline     int         `json:"offline"`
	Boots       int         `json:"boots"`   // Neustarts in 24 h (alle Geräte)
	WanDown     int         `json:"wanDown"` // getrennte WAN-Leitungen
	Devices     []unifiView `json:"devices,omitempty"`
}

type unifiView struct {
	Name    string    `json:"name"`
	Model   string    `json:"model"`
	MAC     string    `json:"mac"`
	IP      string    `json:"ip"`
	State   string    `json:"state"`
	Since   int64     `json:"since"`
	Cat     string    `json:"cat"`
	FW      string    `json:"fw,omitempty"`
	Uptime  int64     `json:"uptime,omitempty"`
	CPU     float64   `json:"cpu,omitempty"`
	Mem     float64   `json:"mem,omitempty"`
	Tx      float64   `json:"tx,omitempty"`
	Rx      float64   `json:"rx,omitempty"`
	PingAvg float64   `json:"pingAvg"` // 1 h, ms (-1 = keine Daten)
	PingMax float64   `json:"pingMax"`
	Loss    float64   `json:"loss"`  // 1 h, %
	Avail   float64   `json:"avail"` // 24 h, % (-1 = keine Daten)
	Boots   []int64   `json:"boots"` // Neustarts 24 h
	Bar     []int8    `json:"bar"`   // 48 × 30 min: 0 keine Daten, 1 ok, 2 Auffälligkeit, 3 offline
	WAN     []wanView `json:"wan,omitempty"`
}

type wanView struct {
	unifiWAN
	Lat24 float64 `json:"lat24,omitempty"` // Ø Latenz 24 h laut UDM
}

const barSlots = 48

func (m *Monitor) unifiSiteView(site *SiteCfg) unifiSummary {
	v := unifiSummary{}
	if site.UniFi == nil {
		return v
	}
	v.On, v.URL, v.Site = site.UniFi.On, site.UniFi.URL, site.UniFi.Site
	v.Mode, v.Console, v.ConsoleName, v.KeySet = site.UniFi.Mode, site.UniFi.Console, site.UniFi.ConsoleName, site.UniFi.Key != ""
	v.WanIgnore = site.UniFi.WanIgnore
	unifiLiveState.Lock()
	defer unifiLiveState.Unlock()
	st := unifiLiveState.bySite[site.ID]
	if st == nil {
		return v
	}
	v.At, v.Err = st.At, st.Err
	now := time.Now().UnixMilli()
	slot := int64(24*3600*1000) / barSlots
	start := now - 24*3600*1000
	for key, d := range st.Devices {
		dv := unifiView{Name: devLabel(d.unifiDevice), Model: d.Model, MAC: d.MAC, IP: d.IP, State: d.State, Since: d.Since,
			Cat: unifiCategory(d.unifiDevice), FW: d.Firmware, Uptime: d.Uptime, CPU: d.CPU, Mem: d.Mem, Tx: d.Tx, Rx: d.Rx,
			PingAvg: -1, Avail: -1, Bar: make([]int8, barSlots)}
		if d.Uptime > 0 && d.At > 0 {
			dv.Uptime = d.Uptime + (now-d.At)/1000
		}
		boots := d.boots
		if len(boots) == 0 {
			boots = m.unifiBootsFromEvents(site.ID, key, start)
		}
		dv.Boots = append([]int64{}, boots...)
		var n, lost, good, upMin, allMin int
		var sum, mx float64
		for _, p := range d.hist {
			allMin++
			if p.Up {
				upMin++
			}
			i := int((p.T - start) / slot)
			if i >= 0 && i < barSlots {
				lvl := int8(1)
				if !p.Up {
					lvl = 3
				} else if p.Lost > 0 {
					lvl = 2
				}
				if lvl > dv.Bar[i] {
					dv.Bar[i] = lvl
				}
			}
			if p.T >= now-3600*1000 && p.N > 0 {
				n += int(p.N)
				lost += int(p.Lost)
				if g := int(p.N - p.Lost); g > 0 {
					sum += float64(p.Avg) * float64(g)
					good += g
					if float64(p.Avg) > mx {
						mx = float64(p.Avg)
					}
				}
			}
		}
		for _, b := range boots {
			if i := int((b - start) / slot); i >= 0 && i < barSlots && dv.Bar[i] < 2 {
				dv.Bar[i] = 2
			}
		}
		if good > 0 {
			dv.PingAvg, dv.PingMax = sum/float64(good), mx
		}
		if n > 0 {
			dv.Loss = float64(lost) * 100 / float64(n)
		}
		if allMin > 0 {
			dv.Avail = float64(upMin) * 100 / float64(allMin)
		}
		for _, w := range d.WAN {
			wv := wanView{unifiWAN: w}
			if l := d.wanLat[w.Name]; len(l) > 0 {
				var s float64
				for _, x := range l {
					s += float64(x)
				}
				wv.Lat24 = s / float64(len(l))
			}
			if !w.Up {
				v.WanDown++
			}
			dv.WAN = append(dv.WAN, wv)
		}
		v.Total++
		if unifiOffline(d.State) {
			v.Offline++
		}
		v.Boots += len(dv.Boots)
		v.Devices = append(v.Devices, dv)
	}
	catOrder := map[string]int{"gateway": 0, "switch": 1, "ap": 2, "other": 3}
	sort.Slice(v.Devices, func(i, j int) bool {
		a, b := v.Devices[i], v.Devices[j]
		oi, oj := unifiOffline(a.State), unifiOffline(b.State)
		if oi != oj {
			return oi
		}
		if catOrder[a.Cat] != catOrder[b.Cat] {
			return catOrder[a.Cat] < catOrder[b.Cat]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return v
}

// ---------- Agent ----------

// unifiLoop fragt die Konsole jede Minute ab, solange sie in der Konfiguration steht.
func (a *Agent) unifiLoop() {
	for {
		a.mu.Lock()
		cfg := a.unifi
		a.mu.Unlock()
		if cfg != nil && cfg.On {
			devs, err := unifiFetch(cfg.URL, cfg.Key, cfg.Site, true)
			body := map[string]any{"devices": devs}
			if err != nil {
				body["error"] = err.Error()
			}
			b, _ := json.Marshal(body)
			if resp, e := a.req("POST", "/api/agent/unifi", bytes.NewReader(b), false); e == nil {
				resp.Body.Close()
			} else {
				log.Printf("UniFi-Status senden: %v", e)
			}
		}
		select {
		case <-a.stopAll:
			return
		case <-time.After(60 * time.Second):
		}
	}
}

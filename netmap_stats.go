package main

import (
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Statistik der Netzkarte: je Gerät (IP) stündlich Prüfungen, Ausfälle, Ø/Max-Ping,
// Ausfallzeiträume und – per SNMP – Traffic je Schnittstelle (Ø/Max Bit/s).
// Gespeichert als Tages-Blob „mapstat:<Karte>:<JJJJ-MM-TT>“, 90 Tage.

const mapStatDays = 90

// hour: [Prüfungen, nicht erreichbar, Summe Ping (Zehntel-ms), Max Ping]
type ipDay struct {
	H   [][4]int64         `json:"h"`             // 24 Stunden
	Out [][2]int64         `json:"out,omitempty"` // Ausfälle [Beginn, Ende] in ms, Ende 0 = läuft
	If  map[string]*ifHour `json:"if,omitempty"`  // Schnittstelle → Stunden
}

// ifHour: je Stunde [Messungen, Summe In, Summe Out, Max In, Max Out] (Bit/s)
type ifHour struct {
	H [][5]float64 `json:"h"`
}

type mapDay struct {
	Day string            `json:"day"`
	IPs map[string]*ipDay `json:"ips"`
}

var mapStats = struct {
	sync.Mutex
	days     map[string]*mapDay           // Karte|Tag
	dirty    map[string]bool              // Karte|Tag
	down     map[string]map[string]int64  // Karte → IP → Ausfall seit (ms)
	prev     map[string]map[string]ifPrev // Karte → IP|Schnittstelle → letzter Zählerstand
	snmp     map[string]time.Time         // Karte → letzter SNMP-Auftrag
	sjobs    map[string]string            // SNMP-Auftrag → Karte
	snmpLast map[string]int64             // Karte → letztes SNMP-Ergebnis
	snmpOK   map[string]int               // Karte → Geräte mit SNMP-Antwort
}{days: map[string]*mapDay{}, dirty: map[string]bool{}, down: map[string]map[string]int64{},
	prev: map[string]map[string]ifPrev{}, snmp: map[string]time.Time{}, sjobs: map[string]string{},
	snmpLast: map[string]int64{}, snmpOK: map[string]int{}}

type ifPrev struct {
	T       int64
	In, Out uint64
	HC      bool
}

func mapStatKey(mapID, day string) string { return "mapstat:" + mapID + ":" + day }

// mapDayLocked: Tagesdaten aus dem Speicher oder der Datenbank (mapStats gesperrt).
func (m *Monitor) mapDayLocked(mapID, day string) *mapDay {
	k := mapID + "|" + day
	if d := mapStats.days[k]; d != nil {
		return d
	}
	d := &mapDay{Day: day, IPs: map[string]*ipDay{}}
	if bs, ok := m.store.(blobStore); ok {
		if b, err := bs.LoadBlob(mapStatKey(mapID, day)); err == nil && len(b) > 0 {
			_ = json.Unmarshal(b, d)
			if d.IPs == nil {
				d.IPs = map[string]*ipDay{}
			}
		}
	}
	mapStats.days[k] = d
	// nur heute und gestern im Speicher halten
	for kk := range mapStats.days {
		if strings.HasPrefix(kk, mapID+"|") && kk < mapID+"|"+time.Now().AddDate(0, 0, -1).Format("2006-01-02") && !mapStats.dirty[kk] {
			delete(mapStats.days, kk)
		}
	}
	return d
}

func newIPDay() *ipDay { return &ipDay{H: make([][4]int64, 24)} }

// recordMapReach: Ergebnis einer Erreichbarkeitsrunde in die Statistik übernehmen.
func (m *Monitor) recordMapReach(mapID string, ips []string, rtt map[string]int32, at time.Time) {
	day, hr, ms := at.Format("2006-01-02"), at.Hour(), at.UnixMilli()
	mapStats.Lock()
	defer mapStats.Unlock()
	d := m.mapDayLocked(mapID, day)
	dn := mapStats.down[mapID]
	if dn == nil {
		dn = map[string]int64{}
		mapStats.down[mapID] = dn
	}
	for _, ip := range ips {
		x := d.IPs[ip]
		if x == nil {
			x = newIPDay()
			d.IPs[ip] = x
		}
		h := &x.H[hr]
		h[0]++
		v, up := rtt[ip]
		if !up {
			h[1]++
			if _, open := dn[ip]; !open {
				dn[ip] = ms
				x.Out = append(x.Out, [2]int64{ms, 0})
			}
			continue
		}
		h[2] += int64(v)
		if int64(v) > h[3] {
			h[3] = int64(v)
		}
		if since, open := dn[ip]; open {
			delete(dn, ip)
			// Ende im Eintrag des Beginn-Tages vermerken (auch über Mitternacht)
			sd := m.mapDayLocked(mapID, time.UnixMilli(since).Format("2006-01-02"))
			if sx := sd.IPs[ip]; sx != nil {
				for i := len(sx.Out) - 1; i >= 0; i-- {
					if sx.Out[i][0] == since && sx.Out[i][1] == 0 {
						sx.Out[i][1] = ms
						mapStats.dirty[mapID+"|"+sd.Day] = true
						break
					}
				}
			}
		}
	}
	mapStats.dirty[mapID+"|"+day] = true
}

// recordMapSNMP: Zählerstände → Bit/s je Schnittstelle (stündlich Ø und Max).
func (m *Monitor) recordMapSNMP(mapID string, res map[string][]ifSample, at time.Time) {
	day, hr, ms := at.Format("2006-01-02"), at.Hour(), at.UnixMilli()
	mapStats.Lock()
	defer mapStats.Unlock()
	d := m.mapDayLocked(mapID, day)
	pv := mapStats.prev[mapID]
	if pv == nil {
		pv = map[string]ifPrev{}
		mapStats.prev[mapID] = pv
	}
	for ip, ifs := range res {
		x := d.IPs[ip]
		if x == nil {
			x = newIPDay()
			d.IPs[ip] = x
		}
		for _, f := range ifs {
			name := f.Name
			if f.Alias != "" && f.Alias != f.Name {
				name += " (" + f.Alias + ")"
			}
			k := ip + "|" + name
			p, ok := pv[k]
			pv[k] = ifPrev{T: ms, In: f.In, Out: f.Out, HC: f.HC}
			if !ok || ms-p.T < 30_000 || ms-p.T > 20*60_000 {
				continue
			}
			din, ok1 := counterDelta(f.In, p.In, f.HC)
			dout, ok2 := counterDelta(f.Out, p.Out, f.HC)
			if !ok1 || !ok2 {
				continue
			}
			sec := float64(ms-p.T) / 1000
			bi, bo := float64(din)*8/sec, float64(dout)*8/sec
			if x.If == nil {
				x.If = map[string]*ifHour{}
			}
			ih := x.If[name]
			if ih == nil {
				if len(x.If) >= 16 {
					continue
				}
				ih = &ifHour{H: make([][5]float64, 24)}
				x.If[name] = ih
			}
			h := &ih.H[hr]
			h[0]++
			h[1] += bi
			h[2] += bo
			if bi > h[3] {
				h[3] = bi
			}
			if bo > h[4] {
				h[4] = bo
			}
		}
	}
	mapStats.dirty[mapID+"|"+day] = true
}

// saveMapStats: geänderte Tage speichern (alle 30 Minuten und beim Beenden).
func (m *Monitor) saveMapStats() {
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	mapStats.Lock()
	type job struct {
		key string
		b   []byte
	}
	var jobs []job
	for k := range mapStats.dirty {
		d := mapStats.days[k]
		if d == nil {
			continue
		}
		b, err := json.Marshal(d)
		if err != nil {
			continue
		}
		i := strings.LastIndex(k, "|")
		jobs = append(jobs, job{mapStatKey(k[:i], k[i+1:]), b})
	}
	mapStats.dirty = map[string]bool{}
	mapStats.Unlock()
	for _, j := range jobs {
		bs.SaveBlob(j.key, j.b)
	}
}

// pruneMapStats: Tage älter als 90 Tage löschen (einmal täglich).
func (m *Monitor) pruneMapStats() {
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	m.mu.RLock()
	var ids []string
	for _, x := range m.cfg.Maps {
		ids = append(ids, x.ID)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		for i := mapStatDays; i < mapStatDays+7; i++ {
			bs.DeleteBlob(mapStatKey(id, time.Now().AddDate(0, 0, -i).Format("2006-01-02")))
		}
	}
}

// ---- SNMP-Aufträge ----

func (m *Monitor) mapSNMPTick() {
	m.mu.RLock()
	maps := append([]NetMap(nil), m.cfg.Maps...)
	m.mu.RUnlock()
	for _, mp := range maps {
		if mp.PingSite == "" || strings.TrimSpace(mp.SnmpComm) == "" || mp.SnmpScope == "off" {
			continue
		}
		mapStats.Lock()
		busy := false
		for _, x := range mapStats.sjobs {
			if x == mp.ID {
				busy = true
			}
		}
		due := !busy && time.Since(mapStats.snmp[mp.ID]) >= mapPingEvery
		if due {
			mapStats.snmp[mp.ID] = time.Now()
		}
		mapStats.Unlock()
		if !due {
			continue
		}
		sc, ok := m.siteByID(mp.PingSite)
		if !ok || sc.Org != mp.Org {
			continue
		}
		on, st := m.siteOnline(sc.ID)
		if !on || (st != nil && newerVersion("1.6.5", st.version)) {
			continue
		}
		d, err := m.loadMap(mp.ID)
		if err != nil {
			continue
		}
		deg := map[string]int{}
		for _, l := range d.Links {
			deg[l.A]++
			deg[l.B]++
		}
		seen := map[string]bool{}
		var ips []string
		for _, n := range d.Nodes {
			if mp.SnmpScope != "all" && deg[n.ID] < 2 && !n.Snmp {
				continue // Standard: nur Sektoren/Masten (mehrere Verbindungen) und markierte Geräte
			}
			ip := net.ParseIP(strings.TrimSpace(n.Addr)).To4()
			if ip == nil || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			ips = append(ips, ip.String())
		}
		for i := 0; i < len(ips); i += 400 {
			end := min(i+400, len(ips))
			j := agentJob{ID: randHex(8), Kind: "snmpif", Params: map[string]string{"ips": strings.Join(ips[i:end], ","), "communities": mp.SnmpComm}}
			jobs.Lock()
			jobs.byID[j.ID] = &jobState{agentJob: j, Org: mp.Org, Site: sc.ID, Status: "queued", Created: time.Now().UnixMilli()}
			jobs.queue[sc.ID] = append(jobs.queue[sc.ID], j)
			jobs.Unlock()
			mapStats.Lock()
			mapStats.sjobs[j.ID] = mp.ID
			mapStats.Unlock()
		}
	}
}

func (m *Monitor) mapSNMPResult(jobID, errText string, result json.RawMessage) {
	mapStats.Lock()
	mid, ok := mapStats.sjobs[jobID]
	delete(mapStats.sjobs, jobID)
	mapStats.Unlock()
	if !ok || errText != "" {
		return
	}
	var res map[string][]ifSample
	if json.Unmarshal(result, &res) != nil {
		return
	}
	m.recordMapSNMP(mid, res, time.Now())
	mapStats.Lock()
	mapStats.snmpLast[mid] = time.Now().UnixMilli()
	mapStats.snmpOK[mid] = len(res)
	mapStats.Unlock()
}

// ---- Auswertung ----

type statHour struct {
	T   int64   `json:"t"`
	N   int64   `json:"n"`
	D   int64   `json:"d"`
	Avg float64 `json:"avg"` // ms
	Max float64 `json:"max"`
}

type statIf struct {
	Name   string       `json:"name"`
	Points [][5]float64 `json:"p"` // [t, Ø in, Ø out, max in, max out] (Bit/s)
}

// GET /api/maps/stats?id=&node=&days=   bzw. &zone=  bzw. &sector=
func (s *Server) handleMapStats(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	q := r.URL.Query()
	id := q.Get("id")
	m.mu.RLock()
	_, _, ok := m.mapByID(p.Org, id)
	m.mu.RUnlock()
	if !ok {
		http.Error(w, "Karte unbekannt", 404)
		return
	}
	days, _ := strconv.Atoi(q.Get("days"))
	if days < 1 || days > mapStatDays {
		days = 7
	}
	d, err := m.loadMap(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	now := time.Now()
	loadDays := func() []*mapDay {
		var out []*mapDay
		mapStats.Lock()
		for i := days - 1; i >= 0; i-- {
			out = append(out, m.mapDayLocked(id, now.AddDate(0, 0, -i).Format("2006-01-02")))
		}
		mapStats.Unlock()
		return out
	}
	if nodeID := q.Get("node"); nodeID != "" {
		var n *mapNode
		for i := range d.Nodes {
			if d.Nodes[i].ID == nodeID {
				n = &d.Nodes[i]
			}
		}
		if n == nil {
			http.Error(w, "Gerät unbekannt", 404)
			return
		}
		ip := strings.TrimSpace(n.Addr)
		var hours []statHour
		ifs := map[string]*statIf{}
		var outs [][2]int64
		var tn, td int64
		var sum, cnt float64
		mds := loadDays()
		mapStats.Lock()
		for _, md := range mds {
			ds, _ := time.ParseInLocation("2006-01-02", md.Day, time.Local)
			x := md.IPs[ip]
			if x == nil {
				continue
			}
			for h := 0; h < 24 && h < len(x.H); h++ {
				v := x.H[h]
				if v[0] == 0 {
					continue
				}
				t := ds.Add(time.Duration(h) * time.Hour).UnixMilli()
				sh := statHour{T: t, N: v[0], D: v[1], Max: float64(v[3]) / 10}
				if v[0]-v[1] > 0 {
					sh.Avg = float64(v[2]) / float64(v[0]-v[1]) / 10
					sum += float64(v[2]) / 10
					cnt += float64(v[0] - v[1])
				}
				tn += v[0]
				td += v[1]
				hours = append(hours, sh)
			}
			outs = append(outs, x.Out...)
			for name, ih := range x.If {
				si := ifs[name]
				if si == nil {
					si = &statIf{Name: name}
					ifs[name] = si
				}
				for h := 0; h < 24 && h < len(ih.H); h++ {
					v := ih.H[h]
					if v[0] == 0 {
						continue
					}
					t := float64(ds.Add(time.Duration(h) * time.Hour).UnixMilli())
					si.Points = append(si.Points, [5]float64{t, v[1] / v[0], v[2] / v[0], v[3], v[4]})
				}
			}
		}
		mapStats.Unlock()
		il := []statIf{}
		for _, si := range ifs {
			il = append(il, *si)
		}
		sort.Slice(il, func(i, j int) bool { return il[i].Name < il[j].Name })
		avail, avg := -1.0, 0.0
		if tn > 0 {
			avail = float64(tn-td) * 100 / float64(tn)
		}
		if cnt > 0 {
			avg = sum / cnt
		}
		sort.Slice(outs, func(i, j int) bool { return outs[i][0] > outs[j][0] })
		writeJSON(w, map[string]any{"node": n, "hours": hours, "outages": outs, "avail": avail, "avg": avg, "ifs": il})
		return
	}
	// Bereich: Ort oder Sektor (mit Kunden) – Verfügbarkeit je Tag und Geräte mit den meisten Ausfällen
	var mem []int
	if z := q.Get("zone"); z != "" {
		mem = ruleMembers(d, MapRule{Scope: "zone", Target: z})
	} else if sid := q.Get("sector"); sid != "" {
		mem = ruleMembers(d, MapRule{Scope: "sector", Target: sid, Customers: true})
	} else {
		mem = ruleMembers(d, MapRule{Scope: "all"})
	}
	ipNode := map[string]mapNode{}
	for _, i := range mem {
		if a := strings.TrimSpace(d.Nodes[i].Addr); a != "" {
			ipNode[a] = d.Nodes[i]
		}
	}
	type dayAv struct {
		Day   string  `json:"day"`
		Avail float64 `json:"avail"`
		Avg   float64 `json:"avg"`
	}
	type devAv struct {
		ID    string  `json:"id"`
		Name  string  `json:"name"`
		Addr  string  `json:"addr"`
		Avail float64 `json:"avail"`
		Outs  int     `json:"outs"`
	}
	var dl []dayAv
	per := map[string]*[3]int64{} // n, d, outs
	mds := loadDays()
	mapStats.Lock()
	for _, md := range mds {
		var n, dd int64
		var sum, cnt float64
		for ip := range ipNode {
			x := md.IPs[ip]
			if x == nil {
				continue
			}
			pp := per[ip]
			if pp == nil {
				pp = &[3]int64{}
				per[ip] = pp
			}
			for _, v := range x.H {
				n += v[0]
				dd += v[1]
				pp[0] += v[0]
				pp[1] += v[1]
				sum += float64(v[2]) / 10
				cnt += float64(v[0] - v[1])
			}
			pp[2] += int64(len(x.Out))
		}
		da := dayAv{Day: md.Day, Avail: -1}
		if n > 0 {
			da.Avail = float64(n-dd) * 100 / float64(n)
		}
		if cnt > 0 {
			da.Avg = sum / cnt
		}
		dl = append(dl, da)
	}
	mapStats.Unlock()
	var devs []devAv
	for ip, pp := range per {
		if pp[0] == 0 || pp[1] == 0 {
			continue
		}
		n := ipNode[ip]
		devs = append(devs, devAv{ID: n.ID, Name: n.Name, Addr: ip, Avail: float64(pp[0]-pp[1]) * 100 / float64(pp[0]), Outs: int(pp[2])})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Avail < devs[j].Avail })
	if len(devs) > 100 {
		devs = devs[:100]
	}
	writeJSON(w, map[string]any{"days": dl, "worst": devs, "devices": len(ipNode)})
}

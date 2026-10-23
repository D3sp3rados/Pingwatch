package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Inventarisierung der überwachten Rechner: Hardware, Betriebssystem und installierte
// Software. Der Agent liest sie beim Start und alle 24 Stunden aus (oder auf Knopfdruck über
// einen Auftrag „inventory“) – nur lesend, keine Lizenzschlüssel. Der Server speichert die
// letzte Inventur und ein Änderungsprotokoll (Software neu/entfernt/aktualisiert, Hardware).

type invDisk struct {
	Model string `json:"m"`
	Size  uint64 `json:"s"` // Bytes
}

type invNIC struct {
	Name string   `json:"n"`
	MAC  string   `json:"mac,omitempty"`
	IPs  []string `json:"ip,omitempty"`
}

type invHW struct {
	Vendor  string    `json:"vendor,omitempty"`
	Model   string    `json:"model,omitempty"`
	Serial  string    `json:"serial,omitempty"`
	BIOS    string    `json:"bios,omitempty"`
	CPU     string    `json:"cpu,omitempty"`
	Cores   int       `json:"cores,omitempty"`
	Threads int       `json:"threads,omitempty"`
	RAM     uint64    `json:"ram,omitempty"`
	Disks   []invDisk `json:"disks,omitempty"`
	NICs    []invNIC  `json:"nics,omitempty"`
}

type invOS struct {
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	Build      string `json:"build,omitempty"`
	Installed  int64  `json:"installed,omitempty"` // ms
	Boot       int64  `json:"boot,omitempty"`      // ms
	Domain     string `json:"domain,omitempty"`
	InDomain   bool   `json:"inDomain,omitempty"`
	User       string `json:"user,omitempty"`       // angemeldeter Benutzer (nur Name)
	Activation string `json:"activation,omitempty"` // Windows-Aktivierung (nur Status, kein Schlüssel)
}

type invSW struct {
	Name      string `json:"n"`
	Version   string `json:"v,omitempty"`
	Vendor    string `json:"p,omitempty"`
	Installed string `json:"d,omitempty"` // YYYY-MM-DD, wenn bekannt
}

type sysInventory struct {
	At  int64   `json:"at"`
	HW  invHW   `json:"hw"`
	OS  invOS   `json:"os"`
	SW  []invSW `json:"sw"`
	Err string  `json:"err,omitempty"` // Teilfehler beim Auslesen
}

// invChange: ein Eintrag im Änderungsprotokoll.
type invChange struct {
	T    int64  `json:"t"`
	Kind string `json:"k"` // neu, entfernt, aktualisiert, hardware
	What string `json:"w"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

type invRecord struct {
	Inv     *sysInventory `json:"inv"`
	Changes []invChange   `json:"changes,omitempty"` // neueste zuerst, höchstens 500
}

const maxInvSW = 5000

// normInventory begrenzt und säubert die Angaben des Agenten.
func normInventory(inv *sysInventory) {
	clip := func(s string, n int) string {
		s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
		if len(s) > n {
			s = s[:n]
		}
		return s
	}
	h := &inv.HW
	h.Vendor, h.Model, h.Serial, h.BIOS, h.CPU = clip(h.Vendor, 120), clip(h.Model, 120), clip(h.Serial, 80), clip(h.BIOS, 80), clip(h.CPU, 160)
	if len(h.Disks) > 64 {
		h.Disks = h.Disks[:64]
	}
	for i := range h.Disks {
		h.Disks[i].Model = clip(h.Disks[i].Model, 120)
	}
	if len(h.NICs) > 64 {
		h.NICs = h.NICs[:64]
	}
	for i := range h.NICs {
		h.NICs[i].Name, h.NICs[i].MAC = clip(h.NICs[i].Name, 120), clip(h.NICs[i].MAC, 40)
		if len(h.NICs[i].IPs) > 16 {
			h.NICs[i].IPs = h.NICs[i].IPs[:16]
		}
	}
	o := &inv.OS
	o.Name, o.Version, o.Build, o.Domain, o.User, o.Activation = clip(o.Name, 160), clip(o.Version, 60), clip(o.Build, 60), clip(o.Domain, 120), clip(o.User, 120), clip(o.Activation, 60)
	seen := map[string]bool{}
	out := inv.SW[:0]
	for _, s := range inv.SW {
		s.Name, s.Version, s.Vendor, s.Installed = clip(s.Name, 200), clip(s.Version, 80), clip(s.Vendor, 120), clip(s.Installed, 20)
		k := strings.ToLower(s.Name) + "|" + s.Version
		if s.Name == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
		if len(out) >= maxInvSW {
			break
		}
	}
	inv.SW = out
	sort.Slice(inv.SW, func(i, j int) bool { return strings.ToLower(inv.SW[i].Name) < strings.ToLower(inv.SW[j].Name) })
	inv.Err = clip(inv.Err, 500)
}

// diffInventory vergleicht zwei Inventuren.
func diffInventory(old, cur *sysInventory, t int64) []invChange {
	if old == nil || cur == nil {
		return nil
	}
	var out []invChange
	key := func(s invSW) string { return strings.ToLower(strings.TrimSpace(s.Name)) }
	oldM := map[string][]string{}
	for _, s := range old.SW {
		oldM[key(s)] = append(oldM[key(s)], s.Version)
	}
	curM := map[string][]string{}
	for _, s := range cur.SW {
		curM[key(s)] = append(curM[key(s)], s.Version)
	}
	name := map[string]string{}
	for _, s := range old.SW {
		name[key(s)] = s.Name
	}
	for _, s := range cur.SW {
		name[key(s)] = s.Name
	}
	var keys []string
	for k := range name {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	join := func(v []string) string { sort.Strings(v); return strings.Join(v, ", ") }
	for _, k := range keys {
		o, okO := oldM[k]
		c, okC := curM[k]
		switch {
		case !okO && okC:
			out = append(out, invChange{T: t, Kind: "neu", What: name[k], To: join(c)})
		case okO && !okC:
			out = append(out, invChange{T: t, Kind: "entfernt", What: name[k], From: join(o)})
		case join(o) != join(c):
			out = append(out, invChange{T: t, Kind: "aktualisiert", What: name[k], From: join(o), To: join(c)})
		}
	}
	hw := func(label, a, b string) {
		if a != b && (a != "" || b != "") {
			out = append(out, invChange{T: t, Kind: "hardware", What: label, From: a, To: b})
		}
	}
	hw("Hersteller/Modell", strings.TrimSpace(old.HW.Vendor+" "+old.HW.Model), strings.TrimSpace(cur.HW.Vendor+" "+cur.HW.Model))
	hw("Seriennummer", old.HW.Serial, cur.HW.Serial)
	hw("BIOS", old.HW.BIOS, cur.HW.BIOS)
	hw("Prozessor", old.HW.CPU, cur.HW.CPU)
	if old.HW.RAM > 0 && cur.HW.RAM > 0 && absDiff(old.HW.RAM, cur.HW.RAM) > 256<<20 {
		hw("Arbeitsspeicher", gb(old.HW.RAM), gb(cur.HW.RAM))
	}
	ds := func(d []invDisk) string {
		var p []string
		for _, x := range d {
			p = append(p, strings.TrimSpace(x.Model)+" "+gb(x.Size))
		}
		sort.Strings(p)
		return strings.Join(p, "; ")
	}
	hw("Datenträger", ds(old.HW.Disks), ds(cur.HW.Disks))
	osName := func(o invOS) string { return strings.TrimSpace(o.Name + " " + o.Build) }
	hw("Betriebssystem", osName(old.OS), osName(cur.OS))
	return out
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

var invCache = struct {
	sync.Mutex
	m map[string]*invRecord
}{m: map[string]*invRecord{}}

func invKey(machineID string) string { return "inv:" + machineID }

// loadInv liefert die gespeicherte Inventur eines Rechners (Zwischenspeicher, sonst Datenbank).
func (m *Monitor) loadInv(machineID string) *invRecord {
	invCache.Lock()
	defer invCache.Unlock()
	if r := invCache.m[machineID]; r != nil {
		return r
	}
	r := &invRecord{}
	if bs, ok := m.store.(blobStore); ok {
		if b, err := bs.LoadBlob(invKey(machineID)); err == nil && len(b) > 0 {
			json.Unmarshal(b, r)
		}
	}
	invCache.m[machineID] = r
	return r
}

func (m *Monitor) dropInv(machineID string) {
	invCache.Lock()
	delete(invCache.m, machineID)
	invCache.Unlock()
	if bs, ok := m.store.(blobStore); ok {
		bs.DeleteBlob(invKey(machineID))
		bs.DeleteBlob(sysKey(machineID))
	}
}

// ingestInventory speichert eine neue Inventur und protokolliert Änderungen als Ereignis (ohne Push).
func (m *Monitor) ingestInventory(mc MachineCfg, inv *sysInventory) []invChange {
	if inv == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	if inv.At <= 0 || inv.At > now+60000 {
		inv.At = now
	}
	normInventory(inv)
	rec := m.loadInv(mc.ID)
	invCache.Lock()
	ch := diffInventory(rec.Inv, inv, inv.At)
	rec.Inv = inv
	if len(ch) > 0 {
		rec.Changes = append(append([]invChange(nil), ch...), rec.Changes...)
		if len(rec.Changes) > 500 {
			rec.Changes = rec.Changes[:500]
		}
	}
	b, _ := json.Marshal(rec)
	invCache.Unlock()
	if bs, ok := m.store.(blobStore); ok {
		bs.SaveBlob(invKey(mc.ID), b)
	}
	if len(ch) > 0 {
		n := map[string]int{}
		for _, c := range ch {
			n[c.Kind]++
		}
		var parts []string
		for _, k := range []string{"neu", "entfernt", "aktualisiert"} {
			if n[k] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n[k], k))
			}
		}
		if n["hardware"] > 0 {
			parts = append(parts, fmt.Sprintf("%d Hardware-Änderung%s", n["hardware"], map[bool]string{true: "en"}[n["hardware"] > 1]))
		}
		label := "Inventar geändert: " + strings.Join(parts, ", ")
		if len(ch) == 1 {
			c := ch[0]
			label = "Inventar: " + c.What + " " + c.Kind
			if c.Kind == "aktualisiert" || c.Kind == "hardware" {
				label += " (" + c.From + " → " + c.To + ")"
			}
		}
		if len(label) > 180 {
			label = label[:180] + "…"
		}
		e := m.newMachineEvent(mc, label+" · "+mc.Name, inv.At, false)
		m.evMu.Lock()
		e.Type, e.Notified = "inv", false
		m.evMu.Unlock()
		m.closeSiteEvent(e, inv.At)
	}
	return ch
}

// ---------- API ----------

// POST /api/agent/inventory {machine, inv}
func (s *Server) handleAgentInventory(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Machine string        `json:"machine"`
		Inv     *sysInventory `json:"inv"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil || req.Inv == nil {
		http.Error(w, "ungültig", 400)
		return
	}
	if site.Sys == nil || !site.Sys.On {
		writeJSON(w, map[string]any{"ok": true, "off": true})
		return
	}
	mc, ok := s.m.machineByID(machineKey(site.ID, strings.TrimSpace(req.Machine)))
	if !ok {
		http.Error(w, "Rechner unbekannt – zuerst Messwerte senden", 404)
		return
	}
	ch := s.m.ingestInventory(mc, req.Inv)
	writeJSON(w, map[string]any{"ok": true, "changes": len(ch)})
}

// GET /api/machine/inventory?id=
func (s *Server) handleMachineInventory(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mc, ok := s.m.machineByID(r.URL.Query().Get("id"))
	if !ok || mc.Org != p.Org || !p.canMachine(mc.ID, false) {
		http.Error(w, "unbekannter Rechner", 404)
		return
	}
	rec := s.m.loadInv(mc.ID)
	invCache.Lock()
	out := invRecord{Inv: rec.Inv, Changes: append([]invChange(nil), rec.Changes...)}
	invCache.Unlock()
	writeJSON(w, out)
}

func fmtDateMs(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format("02.01.2006 15:04")
}

// GET /api/export/inventory.csv?id=  → Hardware und Software eines Rechners
// GET /api/export/inventory.csv      → Inventarliste aller Rechner (je Rechner eine Zeile)
// GET /api/export/inventory.csv?software=1 → Software aller Rechner
func (s *Server) exportInventory(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	q := r.URL.Query()
	m.mu.RLock()
	var list []MachineCfg
	for _, mc := range m.cfg.Machines {
		if mc.Org == p.Org && (q.Get("id") == "" || mc.ID == q.Get("id")) {
			list = append(list, mc)
		}
	}
	groups := map[string]string{}
	for _, g := range m.cfg.Groups {
		groups[g.ID] = g.Name
	}
	m.mu.RUnlock()
	if q.Get("id") != "" && len(list) == 0 {
		http.Error(w, "unbekannter Rechner", 404)
		return
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	stamp := time.Now().Format("2006-01-02")
	write := func(name string, rows [][]string) {
		csvHeader(w, name)
		cw := csv.NewWriter(w)
		cw.Comma = ';'
		cw.WriteAll(rows)
	}
	if q.Get("id") != "" || q.Get("software") == "1" {
		rows := [][]string{{"Rechner", "Kunde", "Standort", "Software", "Version", "Hersteller", "Installiert"}}
		name := "pingwacht-software-" + stamp + ".csv"
		for _, mc := range list {
			rec := m.loadInv(mc.ID)
			invCache.Lock()
			inv := rec.Inv
			invCache.Unlock()
			if inv == nil {
				continue
			}
			for _, sw := range inv.SW {
				rows = append(rows, []string{mc.Name, groups[mc.Group], m.siteNameOf(mc.Site), sw.Name, sw.Version, sw.Vendor, sw.Installed})
			}
			if q.Get("id") != "" {
				name = "inventar-" + csvSafeName(mc.Name) + "-" + stamp + ".csv"
				// Hardware/OS als zusätzliche Zeilen oben
				hw := [][]string{{"Rechner", "Kunde", "Standort", "Merkmal", "Wert", "", ""}}
				add := func(k, v string) {
					if v != "" {
						hw = append(hw, []string{mc.Name, groups[mc.Group], m.siteNameOf(mc.Site), k, v, "", ""})
					}
				}
				invHWRows(inv, add)
				rows = append(append(hw, []string{}), rows...)
			}
		}
		write(name, rows)
		return
	}
	rows := [][]string{{"Rechner", "Rechnername", "Kunde", "Standort", "Hersteller", "Modell", "Seriennummer", "BIOS", "Prozessor", "Kerne", "RAM (GB)", "Datenträger", "Betriebssystem", "Version", "Build", "Aktivierung", "Domäne/Arbeitsgruppe", "Benutzer", "Installiert am", "Letzter Neustart", "Software (Anzahl)", "Stand"}}
	for _, mc := range list {
		rec := m.loadInv(mc.ID)
		invCache.Lock()
		inv := rec.Inv
		invCache.Unlock()
		row := []string{mc.Name, mc.Host, groups[mc.Group], m.siteNameOf(mc.Site)}
		if inv == nil {
			rows = append(rows, append(row, make([]string, 18)...))
			continue
		}
		var disks []string
		for _, d := range inv.HW.Disks {
			disks = append(disks, strings.TrimSpace(d.Model+" "+gb(d.Size)))
		}
		ram := ""
		if inv.HW.RAM > 0 {
			ram = strconv.FormatFloat(float64(inv.HW.RAM)/(1<<30), 'f', 1, 64)
		}
		cores := ""
		if inv.HW.Cores > 0 {
			cores = strconv.Itoa(inv.HW.Cores)
		}
		rows = append(rows, append(row, inv.HW.Vendor, inv.HW.Model, inv.HW.Serial, inv.HW.BIOS, inv.HW.CPU, cores, ram, strings.Join(disks, " | "),
			inv.OS.Name, inv.OS.Version, inv.OS.Build, inv.OS.Activation, inv.OS.Domain, inv.OS.User, fmtDateMs(inv.OS.Installed), fmtDateMs(inv.OS.Boot),
			strconv.Itoa(len(inv.SW)), fmtDateMs(inv.At)))
	}
	write("pingwacht-inventar-"+stamp+".csv", rows)
}

func invHWRows(inv *sysInventory, add func(k, v string)) {
	add("Hersteller", inv.HW.Vendor)
	add("Modell", inv.HW.Model)
	add("Seriennummer", inv.HW.Serial)
	add("BIOS", inv.HW.BIOS)
	add("Prozessor", inv.HW.CPU)
	if inv.HW.Cores > 0 {
		add("Kerne/Threads", fmt.Sprintf("%d / %d", inv.HW.Cores, inv.HW.Threads))
	}
	if inv.HW.RAM > 0 {
		add("Arbeitsspeicher", gb(inv.HW.RAM))
	}
	for _, d := range inv.HW.Disks {
		add("Datenträger", strings.TrimSpace(d.Model+" "+gb(d.Size)))
	}
	for _, n := range inv.HW.NICs {
		add("Netzwerk", strings.TrimSpace(n.Name+" "+n.MAC+" "+strings.Join(n.IPs, ", ")))
	}
	add("Betriebssystem", strings.TrimSpace(inv.OS.Name+" "+inv.OS.Version))
	add("Build", inv.OS.Build)
	add("Aktivierung", inv.OS.Activation)
	add("Domäne/Arbeitsgruppe", inv.OS.Domain)
	add("Angemeldeter Benutzer", inv.OS.User)
	add("Installiert am", fmtDateMs(inv.OS.Installed))
	add("Letzter Neustart", fmtDateMs(inv.OS.Boot))
	add("Stand der Inventur", fmtDateMs(inv.At))
}

func csvSafeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// invStateFor: Inventur-Zeitpunkt je Rechner (für die Übersicht/Detailansicht).
func (m *Monitor) invAt(machineID string) int64 {
	rec := m.loadInv(machineID)
	invCache.Lock()
	defer invCache.Unlock()
	if rec.Inv == nil {
		return 0
	}
	return rec.Inv.At
}

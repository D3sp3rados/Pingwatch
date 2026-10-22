package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// UniFi Protect: Status von Kameras, Sensoren, Lichtern, Gongs, Viewern und NVR über die
// Protect-Integration-API (/proxy/protect/integration/v1) – lokal über den Agenten oder über
// den Cloud-Proxy. Nur lesend und nur Status, keine Bilder.

const protectAPI = "/proxy/protect/integration/v1"
const protectEvery = 5 * time.Minute

type protectDev struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"` // camera, sensor, light, chime, viewer, nvr
	Name   string   `json:"name"`
	Model  string   `json:"model,omitempty"`
	State  string   `json:"state,omitempty"` // CONNECTED, DISCONNECTED …
	Bat    int      `json:"bat"`             // Akku in %, -1 = keiner
	BatLow bool     `json:"batLow,omitempty"`
	Temp   *float64 `json:"temp,omitempty"`
	Hum    *float64 `json:"hum,omitempty"`
	Lux    *float64 `json:"lux,omitempty"`
	Open   *bool    `json:"open,omitempty"` // Tür/Fenster
	Motion bool     `json:"motion,omitempty"`
	Leak   bool     `json:"leak,omitempty"`
	Tamper bool     `json:"tamper,omitempty"`
	FW     string   `json:"fw,omitempty"`
}

func (d protectDev) online() bool {
	return d.State == "" || strings.EqualFold(d.State, "CONNECTED")
}

type protectSnap struct {
	At      int64        `json:"at"`
	Version string       `json:"version,omitempty"`
	Devs    []protectDev `json:"devs,omitempty"`
	Err     string       `json:"err,omitempty"`
}

func mStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s := anyStr(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func mMap(m map[string]any, k string) map[string]any {
	x, _ := m[k].(map[string]any)
	return x
}

func mNum(m map[string]any, path ...string) *float64 {
	cur := m
	for i, k := range path {
		if cur == nil {
			return nil
		}
		if i == len(path)-1 {
			if v, ok := cur[k].(float64); ok {
				return &v
			}
			return nil
		}
		cur = mMap(cur, k)
	}
	return nil
}

// parseProtectList: Antwort einer Protect-Liste (Array oder {data:[…]}) in Geräte umsetzen.
func parseProtectList(kind string, b []byte) ([]protectDev, error) {
	var arr []map[string]any
	if err := json.Unmarshal(b, &arr); err != nil {
		var obj map[string]any
		if json.Unmarshal(b, &obj) != nil {
			return nil, err
		}
		if d, ok := obj["data"].([]any); ok {
			for _, x := range d {
				if m, ok := x.(map[string]any); ok {
					arr = append(arr, m)
				}
			}
		} else if obj["id"] != nil { // einzelnes Objekt (z. B. NVR)
			arr = []map[string]any{obj}
		}
	}
	var out []protectDev
	for _, m := range arr {
		d := protectDev{ID: mStr(m, "id", "mac"), Kind: kind, Name: mStr(m, "name", "displayName"), Model: mStr(m, "modelKey", "type", "marketName"),
			State: strings.ToUpper(mStr(m, "state")), Bat: -1, FW: mStr(m, "firmwareVersion", "version")}
		if d.Name == "" {
			d.Name = d.Model
		}
		if bs := mMap(m, "batteryStatus"); bs != nil {
			if p, ok := bs["percentage"].(float64); ok {
				d.Bat = int(p)
			}
			d.BatLow, _ = bs["isLow"].(bool)
		}
		if st := mMap(m, "stats"); st != nil {
			d.Temp, d.Hum, d.Lux = mNum(st, "temperature", "value"), mNum(st, "humidity", "value"), mNum(st, "light", "value")
		}
		if v, ok := m["isOpened"].(bool); ok {
			d.Open = &v
		}
		d.Motion, _ = m["isMotionDetected"].(bool)
		d.Leak = m["leakDetectedAt"] != nil && m["leakDetectedAt"] != float64(0)
		d.Tamper = m["tamperingDetectedAt"] != nil && m["tamperingDetectedAt"] != float64(0)
		if d.ID == "" && d.Name == "" {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// protect fragt alle Protect-Geräte ab. nil, nil = Protect ist auf der Konsole nicht installiert.
func (u *unifiAPI) protect() (*protectSnap, error) {
	code, b, err := u.raw(protectAPI + "/meta/info")
	if err != nil {
		return nil, err
	}
	if code == 404 || code == 502 || code == 503 {
		return nil, nil
	}
	if code != 200 {
		return nil, fmt.Errorf("UniFi Protect: %v", u.httpErr(code))
	}
	snap := &protectSnap{At: time.Now().UnixMilli()}
	var info map[string]any
	if json.Unmarshal(b, &info) == nil {
		snap.Version = mStr(info, "applicationVersion", "version")
	}
	var errs []string
	for _, k := range [][2]string{{"nvr", "/nvrs"}, {"camera", "/cameras"}, {"sensor", "/sensors"}, {"light", "/lights"}, {"chime", "/chimes"}, {"viewer", "/viewers"}} {
		code, b, err := u.raw(protectAPI + k[1])
		if err != nil {
			return nil, err
		}
		if code == 404 {
			continue
		}
		if code != 200 {
			errs = append(errs, k[1][1:]+": HTTP "+fmt.Sprint(code))
			continue
		}
		l, err := parseProtectList(k[0], b)
		if err != nil {
			errs = append(errs, k[1][1:]+": "+shortErr(err))
			continue
		}
		snap.Devs = append(snap.Devs, l...)
	}
	snap.Err = strings.Join(errs, " · ")
	return snap, nil
}

// ---------- Server ----------

type protectSite struct {
	snap   *protectSnap
	open   map[string]*Event // offline seit
	batLow map[string]bool
}

var protectState = struct {
	sync.Mutex
	bySite map[string]*protectSite
}{bySite: map[string]*protectSite{}}

var protectKindName = map[string]string{"camera": "Kamera", "sensor": "Sensor", "light": "Licht", "chime": "Gong", "viewer": "Viewer", "nvr": "NVR"}

// ingestProtect: Stand merken, Offline/Wieder-online und schwachen Akku melden.
func (m *Monitor) ingestProtect(site SiteCfg, snap *protectSnap) {
	if snap == nil {
		return
	}
	now := time.Now()
	nowMs := now.UnixMilli()
	maint := m.inMaintSite(site.Org, site.ID, now)
	protectState.Lock()
	ps := protectState.bySite[site.ID]
	first := ps == nil
	if ps == nil {
		ps = &protectSite{open: map[string]*Event{}, batLow: map[string]bool{}}
		protectState.bySite[site.ID] = ps
	}
	ps.snap = snap
	var notes []notice
	seen := map[string]bool{}
	for _, d := range snap.Devs {
		key := d.Kind + ":" + d.ID
		seen[key] = true
		label := protectKindName[d.Kind] + " " + d.Name
		if !d.online() {
			if ps.open[key] == nil {
				ps.open[key] = m.newSiteEvent(site, "device", "Protect · "+label, nowMs, maint)
				if !maint && !first {
					notes = append(notes, notice{"devoff", "", "🔴 UniFi Protect: " + label + " offline",
						"Gerät meldet sich nicht bei UniFi Protect (" + site.Name + ")" + unifiVia(site), site.Org, site.ID})
				} else if first {
					// beim ersten Stand nach einem Neustart nicht erneut melden
					m.evMu.Lock()
					ps.open[key].Notified = false
					m.evMu.Unlock()
				}
			}
		} else if e := ps.open[key]; e != nil {
			ce := m.closeSiteEvent(e, nowMs)
			delete(ps.open, key)
			if ce.Notified {
				notes = append(notes, notice{"devon", "", "🟢 UniFi Protect: " + label + " wieder online",
					fmt.Sprintf("War %s offline (%s)", fmtDurShort(ce.End-ce.Start), site.Name) + unifiVia(site), site.Org, site.ID})
			}
		}
		low := d.BatLow || (d.Bat >= 0 && d.Bat <= 15)
		if low && !ps.batLow[key] && !first && !maint {
			notes = append(notes, notice{"devoff", "", "🔋 UniFi Protect: Akku schwach – " + label,
				fmt.Sprintf("Akku bei %d %% (%s)", max(d.Bat, 0), site.Name) + unifiVia(site), site.Org, site.ID})
		}
		ps.batLow[key] = low
	}
	// entfernte Geräte: offene Ereignisse still schließen
	for k, e := range ps.open {
		if !seen[k] {
			m.closeSiteEvent(e, nowMs)
			delete(ps.open, k)
		}
	}
	protectState.Unlock()
	for _, n := range notes {
		m.notify(n)
	}
}

type protectView struct {
	At      int64        `json:"at"`
	Version string       `json:"version,omitempty"`
	Err     string       `json:"err,omitempty"`
	Total   int          `json:"total"`
	Offline int          `json:"offline"`
	BatLow  int          `json:"batLow"`
	Devs    []protectDev `json:"devs"`
}

func protectSiteView(siteID string) *protectView {
	protectState.Lock()
	defer protectState.Unlock()
	ps := protectState.bySite[siteID]
	if ps == nil || ps.snap == nil {
		return nil
	}
	v := &protectView{At: ps.snap.At, Version: ps.snap.Version, Err: ps.snap.Err, Devs: append([]protectDev(nil), ps.snap.Devs...)}
	order := map[string]int{"nvr": 0, "camera": 1, "sensor": 2, "light": 3, "chime": 4, "viewer": 5}
	sort.Slice(v.Devs, func(i, j int) bool {
		a, b := v.Devs[i], v.Devs[j]
		if a.online() != b.online() {
			return !a.online()
		}
		if order[a.Kind] != order[b.Kind] {
			return order[a.Kind] < order[b.Kind]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for _, d := range v.Devs {
		v.Total++
		if !d.online() {
			v.Offline++
		}
		if d.BatLow || (d.Bat >= 0 && d.Bat <= 15) {
			v.BatLow++
		}
	}
	return v
}

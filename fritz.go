package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fritzbox-Auslesung per TR-064: Der Agent fragt die Box im lokalen Netz jede Minute ab
// (SOAP mit HTTP-Digest-Anmeldung) und meldet Internet-Status, Sync-Raten und Laufzeiten.
// Der Server erkennt getrennte Verbindungen, Neueinwahlen und Neustarts der Box.

// fritzStatus: Stand der Box zu einem Zeitpunkt.
type fritzStatus struct {
	Model      string `json:"model,omitempty"`
	FW         string `json:"fw,omitempty"`
	BoxUptime  int64  `json:"boxUptime,omitempty"`  // s
	Access     string `json:"access,omitempty"`     // DSL, Ethernet, X_AVM-DE_Fiber …
	Link       string `json:"link,omitempty"`       // PhysicalLinkStatus: Up, Down, Initializing, Unavailable
	Conn       string `json:"conn,omitempty"`       // ConnectionStatus: Connected, Disconnected … ("" = unbekannt)
	ConnUptime int64  `json:"connUptime,omitempty"` // s
	LastErr    string `json:"lastErr,omitempty"`
	ExtIP      string `json:"extIP,omitempty"`
	Down       int64  `json:"down,omitempty"` // bit/s (Sync bzw. Leitungsrate)
	Up         int64  `json:"up,omitempty"`   // bit/s
	DSL        bool   `json:"dsl,omitempty"`  // Raten stammen aus dem DSL-Sync
}

func (s *fritzStatus) connected() bool { return s != nil && s.Conn == "Connected" }

type fritzSample struct {
	T   int64        `json:"t"`
	S   *fritzStatus `json:"s,omitempty"`
	Err string       `json:"err,omitempty"`
	X   *fritzExtra  `json:"x,omitempty"` // erweiterte Daten (alle 5 Minuten)
}

// ---------- TR-064-Client ----------

type tr064Client struct {
	base       string // http://192.168.178.1:49000
	user, pass string
	hc         *http.Client
	// bodyOverride: SOAP-Text mit Eingabewerten (callArgs), sonst leer
	bodyOverride string
	realm        string
	nonce        string
	opaque       string
	qop          string
	nc           int
}

func fritzBase(addr string, port int) string {
	addr = strings.TrimSpace(addr)
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	addr = strings.TrimRight(addr, "/")
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	addr = strings.Trim(addr, "[]")
	if port == 0 {
		port = 49000
	}
	scheme := "http"
	if port == 49443 {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(addr, strconv.Itoa(port))
}

func newTR064(base, user, pass string) *tr064Client {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // Box hat ein selbstsigniertes Zertifikat
	return &tr064Client{base: strings.TrimRight(base, "/"), user: user, pass: pass, hc: &http.Client{Timeout: 8 * time.Second, Transport: tr}}
}

var errFritzAuth = errors.New("Anmeldung an der Fritzbox abgelehnt – Benutzer/Passwort prüfen")

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// parseDigest liest die Werte aus einem WWW-Authenticate-Kopf (Digest).
func parseDigest(h string) map[string]string {
	out := map[string]string{}
	h = strings.TrimSpace(h)
	if len(h) >= 6 && strings.EqualFold(h[:6], "digest") {
		h = h[6:]
	}
	for len(h) > 0 {
		h = strings.TrimLeft(h, " ,")
		eq := strings.IndexByte(h, '=')
		if eq < 0 {
			break
		}
		k := strings.ToLower(strings.TrimSpace(h[:eq]))
		h = h[eq+1:]
		var v string
		if strings.HasPrefix(h, `"`) {
			end := strings.IndexByte(h[1:], '"')
			if end < 0 {
				v, h = h[1:], ""
			} else {
				v, h = h[1:end+1], h[end+2:]
			}
		} else {
			end := strings.IndexByte(h, ',')
			if end < 0 {
				v, h = h, ""
			} else {
				v, h = h[:end], h[end:]
			}
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

func (c *tr064Client) authHeader(uri string) string {
	c.nc++
	ha1 := md5hex(c.user + ":" + c.realm + ":" + c.pass)
	ha2 := md5hex("POST:" + uri)
	nc := fmt.Sprintf("%08x", c.nc)
	b := make([]byte, 8)
	rand.Read(b)
	cnonce := hex.EncodeToString(b)
	var resp string
	qop := ""
	if strings.Contains(c.qop, "auth") {
		qop = "auth"
		resp = md5hex(ha1 + ":" + c.nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		resp = md5hex(ha1 + ":" + c.nonce + ":" + ha2)
	}
	h := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`, c.user, c.realm, c.nonce, uri, resp)
	if qop != "" {
		h += fmt.Sprintf(`, qop=%s, nc=%s, cnonce="%s"`, qop, nc, cnonce)
	}
	if c.opaque != "" {
		h += fmt.Sprintf(`, opaque="%s"`, c.opaque)
	}
	return h
}

const soapEnv = `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%s xmlns:u="%s"></u:%s></s:Body></s:Envelope>`

// call führt eine TR-064-Aktion aus und liefert die Ausgabewerte (z. B. NewModelName).
func (c *tr064Client) call(control, service, action string) (map[string]string, error) {
	body := fmt.Sprintf(soapEnv, action, service, action)
	if c.bodyOverride != "" {
		body = c.bodyOverride
	}
	var resp *http.Response
	for try := 0; try < 2; try++ {
		req, err := http.NewRequest("POST", c.base+control, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SoapAction", service+"#"+action)
		if c.nonce != "" {
			req.Header.Set("Authorization", c.authHeader(control))
		}
		resp, err = c.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Fritzbox nicht erreichbar: %v", err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			break
		}
		d := parseDigest(resp.Header.Get("WWW-Authenticate"))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if d["nonce"] == "" || (try == 1) || (c.nonce != "" && d["stale"] != "true" && d["nonce"] == c.nonce) {
			return nil, errFritzAuth
		}
		c.realm, c.nonce, c.opaque, c.qop, c.nc = d["realm"], d["nonce"], d["opaque"], d["qop"], 0
		resp = nil
	}
	if resp == nil {
		return nil, errFritzAuth
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	vals := parseSOAP(data)
	if resp.StatusCode != http.StatusOK {
		if vals["errorDescription"] != "" {
			return nil, fmt.Errorf("%s: %s (%s)", action, vals["errorDescription"], vals["errorCode"])
		}
		return nil, fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
	}
	return vals, nil
}

// parseSOAP sammelt alle Blatt-Elemente (lokaler Name → Text) aus einer SOAP-Antwort.
func parseSOAP(b []byte) map[string]string {
	out := map[string]string{}
	dec := xml.NewDecoder(bytes.NewReader(b))
	var name string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name = t.Name.Local
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if name == t.Name.Local {
				out[name] = strings.TrimSpace(text.String())
			}
			name = ""
		}
	}
	return out
}

const (
	svcDeviceInfo = "urn:dslforum-org:service:DeviceInfo:1"
	svcCommonIf   = "urn:dslforum-org:service:WANCommonInterfaceConfig:1"
	svcPPP        = "urn:dslforum-org:service:WANPPPConnection:1"
	svcIP         = "urn:dslforum-org:service:WANIPConnection:1"
	svcDSL        = "urn:dslforum-org:service:WANDSLInterfaceConfig:1"
)

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// fritzQuery liest den aktuellen Stand der Box.
func fritzQuery(c *tr064Client) (*fritzStatus, error) {
	info, err := c.call("/upnp/control/deviceinfo", svcDeviceInfo, "GetInfo")
	if err != nil {
		return nil, err
	}
	st := &fritzStatus{Model: info["NewModelName"], FW: info["NewSoftwareVersion"], BoxUptime: atoi64(info["NewUpTime"])}
	if l, err := c.call("/upnp/control/wancommonifconfig1", svcCommonIf, "GetCommonLinkProperties"); err == nil {
		st.Access, st.Link = l["NewWANAccessType"], l["NewPhysicalLinkStatus"]
		st.Down, st.Up = atoi64(l["NewLayer1DownstreamMaxBitRate"]), atoi64(l["NewLayer1UpstreamMaxBitRate"])
	}
	// Verbindung: PPP (DSL) oder IP (Kabel, Glasfaser, Mobilfunk) – die aktive nehmen
	type conn struct {
		ctrl, svc string
		v         map[string]string
	}
	var cands []conn
	for _, x := range []conn{{"/upnp/control/wanpppconn1", svcPPP, nil}, {"/upnp/control/wanipconnection1", svcIP, nil}} {
		if v, err := c.call(x.ctrl, x.svc, "GetStatusInfo"); err == nil {
			s := v["NewConnectionStatus"]
			if s != "" && s != "Unconfigured" {
				x.v = v
				cands = append(cands, x)
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].v["NewConnectionStatus"] == "Connected" && cands[j].v["NewConnectionStatus"] != "Connected"
	})
	if len(cands) > 0 {
		x := cands[0]
		st.Conn, st.ConnUptime = x.v["NewConnectionStatus"], atoi64(x.v["NewUptime"])
		if e := x.v["NewLastConnectionError"]; e != "" && e != "ERROR_NONE" {
			st.LastErr = e
		}
		if ip, err := c.call(x.ctrl, x.svc, "GetExternalIPAddress"); err == nil {
			st.ExtIP = ip["NewExternalIPAddress"]
		}
	}
	// DSL-Sync (bei Kabel/Glasfaser gibt es den Dienst nicht – Fehler ignorieren)
	if st.Access == "" || strings.EqualFold(st.Access, "DSL") {
		if d, err := c.call("/upnp/control/wandslifconfig1", svcDSL, "GetInfo"); err == nil {
			if dn, up := atoi64(d["NewDownstreamCurrRate"]), atoi64(d["NewUpstreamCurrRate"]); dn > 0 {
				st.Down, st.Up, st.DSL = dn*1000, up*1000, true
			}
		}
	}
	return st, nil
}

// ---------- Agent ----------

const fritzPollEvery = 60 * time.Second

// fritzLoop fragt die Box jede Minute ab. Ist der Server nicht erreichbar (z. B. weil das
// Internet weg ist), werden die Messungen gesammelt und später nachgereicht.
func (a *Agent) fritzLoop(ah *agentHost) {
	c := newTR064(fritzBase(ah.cfg.Addr, ah.cfg.FritzPort), ah.cfg.FritzUser, ah.cfg.FritzPass)
	var pend []fritzSample
	var lastExtra time.Time
	for {
		s := fritzSample{T: time.Now().UnixMilli()}
		st, err := fritzQuery(c)
		if err != nil {
			s.Err = err.Error()
		} else {
			s.S = st
			if time.Since(lastExtra) >= fritzExtraEvery {
				lastExtra = time.Now()
				s.X = fritzExtraQuery(c)
			}
		}
		select {
		case <-ah.stop:
			return
		default:
		}
		pend = append(pend, s)
		if len(pend) > 1440 {
			pend = pend[len(pend)-1440:]
		}
		b, _ := json.Marshal(map[string]any{"host": ah.cfg.ID, "samples": pend})
		if resp, e := a.req("POST", "/api/agent/fritz", bytes.NewReader(b), false); e == nil {
			if resp.StatusCode == http.StatusOK {
				pend = nil
			}
			resp.Body.Close()
		} else {
			log.Printf("Fritzbox %s senden: %v", ah.cfg.Name, e)
		}
		select {
		case <-ah.stop:
			return
		case <-time.After(fritzPollEvery):
		}
	}
}

// ---------- Server ----------

type fritzPoint struct {
	T    int64 `json:"t"`
	Down int32 `json:"d"` // kbit/s
	Up   int32 `json:"u"` // kbit/s
	C    int8  `json:"c"` // 1 verbunden, 0 getrennt, -1 unbekannt
}

type fritzHost struct {
	mu        sync.Mutex
	at        int64 // letzte Meldung
	err       string
	cur       *fritzStatus
	curT      int64
	connSince int64
	badSince  int64
	badN      int
	open      *Event
	hist      []fritzPoint
	drops     []int64 // Abbrüche/Neueinwahlen (Zeitpunkte, 24 h)
	boots     []int64
	extra     *fritzExtra
	known     map[string]bool // bekannte Geräte (MAC) für „neues Gerät“
	fwNoted   string          // Firmware-Version, zu der schon gemeldet wurde
	logHist   []string        // gesammeltes Ereignisprotokoll (neueste zuerst)
	logLoaded bool
}

var fritzData = struct {
	sync.Mutex
	m map[string]*fritzHost
}{m: map[string]*fritzHost{}}

func getFritzHost(id string) *fritzHost {
	fritzData.Lock()
	defer fritzData.Unlock()
	h := fritzData.m[id]
	if h == nil {
		h = &fritzHost{}
		fritzData.m[id] = h
	}
	return h
}

func trimTimes(ts []int64, cut int64) []int64 {
	i := 0
	for i < len(ts) && ts[i] < cut {
		i++
	}
	return ts[i:]
}

// ingestFritz verarbeitet die Messungen des Agenten (zeitlich sortiert, auch nachgereichte).
func (m *Monitor) ingestFritz(hc HostCfg, samples []fritzSample) {
	now := time.Now().UnixMilli()
	sort.Slice(samples, func(i, j int) bool { return samples[i].T < samples[j].T })
	fh := getFritzHost(hc.ID)
	var notes []notice
	name := hc.Name
	hhmm := func(t int64) string { return time.UnixMilli(t).Format("15:04") }
	fh.mu.Lock()
	for _, s := range samples {
		if s.T <= fh.at || s.T > now+60000 || s.T < now-24*3600*1000 {
			continue
		}
		fh.at = s.T
		if s.X != nil {
			notes = append(notes, m.fritzExtraIngest(hc, fh, s.X, s.T >= now-15*60000)...)
		}
		if s.Err != "" || s.S == nil {
			fh.err = s.Err
			if fh.err == "" {
				fh.err = "keine Daten"
			}
			continue
		}
		fh.err = ""
		cur, prev, prevT := s.S, fh.cur, fh.curT
		cur.fixRates()
		maint := m.inMaint(hc.Org, hc.ID, time.UnixMilli(s.T))
		recent := s.T >= now-5*60000
		if prev != nil && prevT > 0 {
			dt := (s.T - prevT) / 1000
			// Neustart der Box: Laufzeit kleiner als erwartet
			if prev.BoxUptime > 0 && cur.BoxUptime > 0 && cur.BoxUptime+120 < prev.BoxUptime+dt {
				at := s.T - cur.BoxUptime*1000
				fh.boots = append(fh.boots, at)
				m.closeSiteEvent(m.newHostEvent(hc, "reboot", name+" (Fritzbox)", at, maint), at)
				if !maint && recent {
					notes = append(notes, notice{"reboot", hc.ID, "🔄 Fritzbox neu gestartet: " + name,
						m.withInfo(hc.ID, hc.Site, "Neustart um "+hhmm(at)+" (Stromausfall, Update oder manuell?)"), hc.Org, ""})
				}
			}
			// Neueinwahl: Verbindungsdauer springt zurück
			if prev.connected() && cur.connected() && fh.open == nil && prev.ConnUptime > 0 && cur.ConnUptime+120 < prev.ConnUptime+dt {
				at := s.T - cur.ConnUptime*1000
				fh.drops = append(fh.drops, at)
				if at-prevT > 3*60000 {
					// längere Lücke ohne Messungen: Ausfall vom letzten „verbunden“ bis zur Neueinwahl
					e := m.newHostEvent(hc, "wan", name+" · Internet", prevT, maint)
					ce := m.closeSiteEvent(e, at)
					if !maint && recent {
						notes = append(notes, notice{"wanup", hc.ID, "🟢 Internet wieder verbunden: " + name,
							m.withInfo(hc.ID, hc.Site, fmt.Sprintf("War ca. %s getrennt (%s – %s)", fmtDurShort(ce.End-ce.Start), hhmm(ce.Start), hhmm(ce.End))), hc.Org, ""})
					}
				} else {
					m.closeSiteEvent(m.newHostEvent(hc, "wan", name+" · Neueinwahl", at, maint), at) // kurz – nur als Ereignis, keine Meldung
				}
			}
		}
		switch {
		case cur.Conn == "": // Box ohne eigene Internetverbindung (z. B. IP-Client) – nichts bewerten
			fh.badN = 0
		case cur.connected():
			fh.badN = 0
			if fh.open != nil {
				end := s.T - cur.ConnUptime*1000
				if cur.ConnUptime <= 0 || end < fh.open.Start {
					end = s.T
				}
				ce := m.closeSiteEvent(fh.open, end)
				fh.open = nil
				if !ce.Maint && recent {
					title := "🟢 Internet wieder verbunden: " + name
					notes = append(notes, notice{"wanup", hc.ID, title,
						m.withInfo(hc.ID, hc.Site, fmt.Sprintf("War %s getrennt (%s – %s)", fmtDurShort(ce.End-ce.Start), hhmm(ce.Start), hhmm(ce.End))), hc.Org, ""})
				}
			}
			fh.connSince = s.T - cur.ConnUptime*1000
		default:
			if fh.badN == 0 {
				fh.badSince = s.T
			}
			fh.badN++
			if fh.open == nil && fh.badN >= 2 {
				e := m.newHostEvent(hc, "wan", name+" · Internet", fh.badSince, maint)
				fh.open = e
				fh.drops = append(fh.drops, fh.badSince)
				if !maint && recent {
					why := "Fritzbox meldet: " + cur.Conn
					if cur.LastErr != "" {
						why += " (" + cur.LastErr + ")"
					}
					if cur.Link != "" && cur.Link != "Up" {
						why += ", Leitung: " + cur.Link
					}
					notes = append(notes, notice{"wandown", hc.ID, "🔴 Internet getrennt: " + name,
						m.withInfo(hc.ID, hc.Site, "Seit "+hhmm(fh.badSince)+" keine Internetverbindung. "+why), hc.Org, ""})
				} else {
					m.evMu.Lock()
					e.Notified = false
					m.evMu.Unlock()
				}
			}
		}
		fh.cur, fh.curT = cur, s.T
		p := fritzPoint{T: s.T, Down: int32(cur.Down / 1000), Up: int32(cur.Up / 1000), C: -1}
		if cur.Conn != "" {
			p.C = 0
			if cur.connected() {
				p.C = 1
			}
		}
		fh.hist = append(fh.hist, p)
	}
	cut := now - 24*3600*1000
	if i := sort.Search(len(fh.hist), func(i int) bool { return fh.hist[i].T >= cut }); i > 0 {
		fh.hist = append([]fritzPoint(nil), fh.hist[i:]...)
	}
	fh.drops = trimTimes(fh.drops, cut)
	fh.boots = trimTimes(fh.boots, cut)
	fh.mu.Unlock()
	for _, n := range notes {
		m.notify(n)
	}
}

// fritzDropsFromEvents: Abbrüche der letzten 24 h nach einem Server-Neustart aus den Ereignissen.
func (m *Monitor) fritzDropsFromEvents(hostID string, since int64) []int64 {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	var out []int64
	for _, e := range m.events {
		if e.Type == "wan" && e.HostID == hostID && e.Start >= since {
			out = append(out, e.Start)
		}
	}
	return out
}

type fritzView struct {
	On        bool         `json:"on"`
	At        int64        `json:"at,omitempty"`
	Err       string       `json:"err,omitempty"`
	Status    *fritzStatus `json:"status,omitempty"`
	ConnSince int64        `json:"connSince,omitempty"`
	Drops24   int          `json:"drops24"`
	Boots24   int          `json:"boots24"`
	MinDown   int32        `json:"minDown,omitempty"` // kbit/s, 24 h
	MaxDown   int32        `json:"maxDown,omitempty"`
	Avail     float64      `json:"avail"` // % verbunden in 24 h (-1 = keine Daten)
	Hist      []fritzPoint `json:"hist,omitempty"`
	Extra     *fritzExtra  `json:"extra,omitempty"`
}

func (m *Monitor) fritzHostView(hc HostCfg) fritzView {
	v := fritzView{On: hc.Fritz, Avail: -1}
	if !hc.Fritz {
		return v
	}
	now := time.Now().UnixMilli()
	cut := now - 24*3600*1000
	fh := getFritzHost(hc.ID)
	fh.mu.Lock()
	v.At, v.Err, v.Status, v.ConnSince, v.Extra = fh.at, fh.err, fh.cur, fh.connSince, fh.extra.view(hc.FritzFullNums)
	if v.Extra != nil && len(fh.logHist) > 0 {
		c := *v.Extra
		c.Log = fh.logHist
		v.Extra = &c
	}
	drops := trimTimes(fh.drops, cut)
	v.Boots24 = len(trimTimes(fh.boots, cut))
	var known, up int
	for _, p := range fh.hist {
		if p.T < cut {
			continue
		}
		if p.C >= 0 {
			known++
			if p.C == 1 {
				up++
			}
		}
		if p.Down > 0 {
			if v.MinDown == 0 || p.Down < v.MinDown {
				v.MinDown = p.Down
			}
			if p.Down > v.MaxDown {
				v.MaxDown = p.Down
			}
		}
	}
	// höchstens 288 Punkte (5-Minuten-Raster) für den Verlauf
	step := len(fh.hist)/288 + 1
	for i := 0; i < len(fh.hist); i += step {
		v.Hist = append(v.Hist, fh.hist[i])
	}
	hasHist := len(fh.hist) > 0
	fh.mu.Unlock()
	if len(drops) == 0 && !hasHist {
		drops = m.fritzDropsFromEvents(hc.ID, cut)
	}
	v.Drops24 = len(drops)
	if known > 0 {
		v.Avail = float64(up) * 100 / float64(known)
	}
	return v
}

// POST /api/agent/fritz {host, samples}
func (s *Server) handleAgentFritz(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		Host    string        `json:"host"`
		Samples []fritzSample `json:"samples"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	h := s.m.host(req.Host)
	if h == nil {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg.Site == site.ID && cfg.Fritz && !cfg.Paused {
		s.m.ingestFritz(cfg, req.Samples)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/fritz?host= → Stand und Verlauf der Fritzbox
func (s *Server) handleFritz(w http.ResponseWriter, r *http.Request) {
	h := s.hostFor(r, r.URL.Query().Get("host"))
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	writeJSON(w, s.m.fritzHostView(cfg))
}

// publicHost entfernt geheime Felder, bevor eine Host-Konfiguration an den Browser geht.
func publicHost(hc HostCfg) HostCfg {
	hc.FritzPass, hc.MtPass = "", ""
	return hc
}

// fixRates: Ohne DSL (Glasfaser, Kabel, LTE) meldet die Fritzbox keine echte Sync-Rate, sondern einen
// Platzhalter bzw. den Wert aus den Interneteinstellungen (oft 1 Mbit/s) – solche Werte verwerfen.
func (st *fritzStatus) fixRates() {
	if st != nil && !st.DSL && st.Down <= 1_000_000 && st.Up <= 1_000_000 {
		st.Down, st.Up = 0, 0
	}
}

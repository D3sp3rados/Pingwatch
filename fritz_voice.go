package main

import (
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Sprachqualität der Internettelefonie (Fritzbox „Telefonie → Eigene Rufnummern → Sprachübertragung“).
// TR-064 liefert diese Werte nicht. Der Agent meldet sich deshalb wie ein Browser an der
// Benutzeroberfläche an (login_sid.lua, PBKDF2) und liest data.lua?page=sipQual – jede Minute,
// mit dem Benutzer aus den TR-064-Einstellungen. Zusätzlich können die Push-Service-Mails
// („FRITZ!Box-Info“ mit Detailinformationen) per IMAP eingelesen werden (fritz_voicemail.go).

// fritzVoice: ein Telefonat mit den Werten für Sende- (Tx) und Empfangsrichtung (Rx).
type fritzVoice struct {
	T       int64   `json:"t"`               // Beginn (ms)
	Start   string  `json:"start"`           // wie von der Box angezeigt, z. B. 11.11.2026 13:35
	Sec     int     `json:"sec"`             // Dauer in Sekunden (aus der Mail nur minutengenau)
	Own     string  `json:"own,omitempty"`   // eigene Rufnummer
	Other   string  `json:"other,omitempty"` // Gegenstelle
	Remote  string  `json:"remote,omitempty"`
	CodecTx string  `json:"codecTx,omitempty"`
	CodecRx string  `json:"codecRx,omitempty"`
	PktTx   int     `json:"pktTx"`
	PktRx   int     `json:"pktRx"`
	LossTx  float64 `json:"lossTx"` // % (-1 = keine Angabe)
	LossRx  float64 `json:"lossRx"`
	Delay   float64 `json:"delay"` // ms (-1 = keine Angabe)
	JitTx   float64 `json:"jitTx"`
	JitRx   float64 `json:"jitRx"`
	Burst   string  `json:"burst,omitempty"`
	Active  bool    `json:"active,omitempty"`
	Src     string  `json:"src,omitempty"` // live, mail
}

// key erkennt dasselbe Gespräch aus Live-Abfrage und Mail (Dauer ist dort unterschiedlich genau).
func (v fritzVoice) key() string {
	return v.Start + "|" + numDigits(v.Own) + "|" + numDigits(v.Other) + "|" + strconv.Itoa(v.PktTx)
}

func (v fritzVoice) maxLoss() float64 {
	if v.LossTx > v.LossRx {
		return v.LossTx
	}
	return v.LossRx
}

// fritzVoiceSnap: Ergebnis einer Abfrage (nur neue beendete Gespräche plus die laufenden).
type fritzVoiceSnap struct {
	At     int64        `json:"at"`
	Calls  []fritzVoice `json:"calls,omitempty"`
	Active []fritzVoice `json:"active,omitempty"`
	Err    string       `json:"err,omitempty"`
}

// numDigits: nur Ziffern; +49/0049 → 0 (damit +4977… und 077… gleich sind).
func numDigits(s string) string {
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	plus := strings.HasPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	switch {
	case plus && strings.HasPrefix(d, "49"):
		d = "0" + d[2:]
	case strings.HasPrefix(d, "0049"):
		d = "0" + d[4:]
	}
	return d
}

var berlin = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Berlin"); err == nil {
		return l
	}
	return time.Local
}()

// parseFritzTime: „11.11.2026 13:35“ (Uhrzeit der Box = deutsche Zeit).
func parseFritzTime(s string) int64 {
	s = strings.Join(strings.Fields(s), " ")
	for _, l := range []string{"2.1.2006 15:04:05", "2.1.2006 15:04", "2.1.06 15:04"} {
		if t, err := time.ParseInLocation(l, s, berlin); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// parseDurSec: „0:01:06“ (h:mm:ss) oder „0:03“ (h:mm, Mail).
func parseDurSec(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i] // „0:03 (4679 ms)“
	}
	p := strings.Split(s, ":")
	n := make([]int, len(p))
	for i, x := range p {
		v, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		n[i] = v
	}
	switch len(n) {
	case 3:
		return n[0]*3600 + n[1]*60 + n[2]
	case 2:
		return n[0]*3600 + n[1]*60
	}
	return 0
}

// parseNumUnit: „0.2 %“, „9.5 ms“, „28 %“ → Zahl; „-“ oder leer → -1.
func parseNumUnit(s string) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return -1
	}
	f, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return -1
	}
	return f
}

func parseLeadInt(s string) int {
	f := parseNumUnit(s)
	if f < 0 {
		return 0
	}
	return int(f)
}

// ---------- Seite sipQual (HTML) auswerten ----------

// cellParts: Zelleninhalt an <br> getrennt, ohne Tags, Entities aufgelöst.
func cellParts(cell string) []string {
	low := strings.ToLower(cell)
	var parts []string
	for {
		i := strings.Index(low, "<br")
		if i < 0 {
			parts = append(parts, cell)
			break
		}
		parts = append(parts, cell[:i])
		j := strings.IndexByte(low[i:], '>')
		if j < 0 {
			break
		}
		cell, low = cell[i+j+1:], low[i+j+1:]
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = html.UnescapeString(stripTags(p))
		p = strings.TrimSpace(strings.ReplaceAll(p, " ", " "))
		out = append(out, p)
	}
	return out
}

// tableRows: Zeilen (Zelleninhalte als HTML) der Tabelle mit der angegebenen id.
func tableRows(doc, id string) [][]string {
	low := strings.ToLower(doc)
	i := strings.Index(low, `id="`+strings.ToLower(id)+`"`)
	if i < 0 {
		return nil
	}
	end := strings.Index(low[i:], "</table")
	if end < 0 {
		end = len(low) - i
	}
	body, lb := doc[i:i+end], low[i:i+end]
	var rows [][]string
	for {
		r := strings.Index(lb, "<tr")
		if r < 0 {
			break
		}
		re := strings.Index(lb[r+3:], "<tr")
		var row, rowL string
		if re < 0 {
			row, rowL = body[r:], lb[r:]
		} else {
			row, rowL = body[r:r+3+re], lb[r:r+3+re]
		}
		var cells []string
		for {
			c := strings.Index(rowL, "<td")
			if c < 0 {
				break
			}
			gt := strings.IndexByte(rowL[c:], '>')
			if gt < 0 {
				break
			}
			start := c + gt + 1
			ce := strings.Index(rowL[start:], "</td")
			if ce < 0 {
				ce = len(rowL) - start
			}
			cells = append(cells, row[start:start+ce])
			row, rowL = row[start+ce:], rowL[start+ce:]
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
		if re < 0 {
			break
		}
		body, lb = body[r+3+re:], lb[r+3+re:]
	}
	return rows
}

func part(p []string, i int) string {
	if i < len(p) {
		return p[i]
	}
	return ""
}

// ownFromHeader: „Eigene Rufnummer: 077472109822 (Verfügbarkeit 100%)“ → Rufnummer.
func ownFromHeader(s string) (string, bool) {
	k := strings.Index(s, "Rufnummer:")
	if k < 0 {
		return "", false
	}
	s = strings.TrimSpace(s[k+len("Rufnummer:"):])
	if i := strings.IndexAny(s, " (\t"); i > 0 {
		s = s[:i]
	}
	return s, true
}

// parseSipQual liest laufende und beendete Gespräche aus der Antwort von data.lua?page=sipQual.
func parseSipQual(doc string) (active, done []fritzVoice) {
	read := func(id string, act bool) []fritzVoice {
		var out []fritzVoice
		own := ""
		for _, cells := range tableRows(doc, id) {
			if len(cells) < 8 {
				if o, ok := ownFromHeader(strings.Join(cellParts(cells[0]), " ")); ok {
					own = o
				}
				continue
			}
			c := make([][]string, len(cells))
			for i := range cells {
				c[i] = cellParts(cells[i])
			}
			when := part(c[0], 0)
			t := parseFritzTime(when)
			if t == 0 {
				continue // Kopfzeile oder unbekanntes Format
			}
			v := fritzVoice{T: t, Start: strings.Join(strings.Fields(when), " "), Sec: parseDurSec(part(c[0], 1)), Own: own,
				Other: part(c[1], 0), Remote: part(c[1], 1), CodecTx: part(c[2], 0), CodecRx: part(c[2], 1),
				PktTx: parseLeadInt(part(c[3], 0)), PktRx: parseLeadInt(part(c[3], 1)),
				LossTx: parseNumUnit(part(c[4], 0)), LossRx: parseNumUnit(part(c[4], 1)),
				Delay: parseNumUnit(part(c[5], 0)), JitTx: parseNumUnit(part(c[6], 0)), JitRx: parseNumUnit(part(c[6], 1)),
				Burst: strings.TrimSpace(part(c[7], 1)), Active: act, Src: "live"}
			if v.CodecRx == "" {
				v.CodecRx = v.CodecTx
			}
			if v.Burst == "-" {
				v.Burst = ""
			}
			out = append(out, v)
		}
		return out
	}
	return read("uiListOfActivCalls", true), read("uiListOfAllCalls", false)
}

// ---------- Anmeldung an der Benutzeroberfläche ----------

type fritzWeb struct {
	base, user, pass string
	hc               *http.Client
	sid              string
}

// fritzWebBase: Adresse der Benutzeroberfläche zum TR-064-Port (49000 → http, 49443 → https,
// sonst der Fernzugriffs-Port).
func fritzWebBase(addr string, port int) string {
	addr = strings.TrimSpace(addr)
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	addr = strings.TrimRight(addr, "/")
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	addr = strings.Trim(addr, "[]")
	host := addr
	if strings.Contains(addr, ":") {
		host = "[" + addr + "]"
	}
	switch port {
	case 0, 49000:
		return "http://" + host
	case 49443, 443:
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(addr, strconv.Itoa(port))
}

func newFritzWeb(base, user, pass string) *fritzWeb {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // selbstsigniertes Zertifikat der Box
	return &fritzWeb{base: base, user: user, pass: pass, hc: &http.Client{Timeout: 15 * time.Second, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type fritzSessionInfo struct {
	SID       string `xml:"SID"`
	Challenge string `xml:"Challenge"`
	BlockTime int    `xml:"BlockTime"`
}

// fritzLoginResponse berechnet die Antwort auf die Challenge (PBKDF2 ab FRITZ!OS 7.24, sonst MD5).
func fritzLoginResponse(challenge, pass string) (string, error) {
	if strings.HasPrefix(challenge, "2$") {
		p := strings.Split(challenge, "$")
		if len(p) != 5 {
			return "", errors.New("unbekanntes Challenge-Format")
		}
		it1, _ := strconv.Atoi(p[1])
		it2, _ := strconv.Atoi(p[3])
		s1, err1 := hex.DecodeString(p[2])
		s2, err2 := hex.DecodeString(p[4])
		if it1 <= 0 || it2 <= 0 || err1 != nil || err2 != nil {
			return "", errors.New("unbekanntes Challenge-Format")
		}
		h1, err := pbkdf2.Key(sha256.New, pass, s1, it1, 32)
		if err != nil {
			return "", err
		}
		h2, err := pbkdf2.Key(sha256.New, string(h1), s2, it2, 32)
		if err != nil {
			return "", err
		}
		return p[4] + "$" + hex.EncodeToString(h2), nil
	}
	// MD5 (ältere Boxen): UTF-16LE von „challenge-passwort“, Zeichen > 255 als „.“
	var b []byte
	for _, r := range utf16.Encode([]rune(challenge + "-" + pass)) {
		if r > 255 {
			r = '.'
		}
		b = append(b, byte(r), byte(r>>8))
	}
	h := md5.Sum(b)
	return challenge + "-" + hex.EncodeToString(h[:]), nil
}

func (f *fritzWeb) sessionInfo(resp *http.Response, err error) (*fritzSessionInfo, error) {
	if err != nil {
		return nil, fmt.Errorf("Fritzbox nicht erreichbar: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Anmeldung: HTTP %d", resp.StatusCode)
	}
	var si fritzSessionInfo
	if err := xml.Unmarshal(b, &si); err != nil {
		return nil, errors.New("Anmeldung: unerwartete Antwort der Box")
	}
	return &si, nil
}

func (f *fritzWeb) login() error {
	f.sid = ""
	si, err := f.sessionInfo(f.hc.Get(f.base + "/login_sid.lua?version=2"))
	if err != nil {
		return err
	}
	if si.BlockTime > 0 {
		return fmt.Errorf("Anmeldung an der Box für %d s gesperrt (zu viele Fehlversuche)", si.BlockTime)
	}
	resp, err := fritzLoginResponse(si.Challenge, f.pass)
	if err != nil {
		return err
	}
	form := url.Values{"username": {f.user}, "response": {resp}}
	si, err = f.sessionInfo(f.hc.PostForm(f.base+"/login_sid.lua?version=2", form))
	if err != nil {
		return err
	}
	if si.SID == "" || strings.Trim(si.SID, "0") == "" {
		return errors.New("Anmeldung an der Benutzeroberfläche abgelehnt – Benutzer/Passwort prüfen")
	}
	f.sid = si.SID
	return nil
}

// sipQual holt die Seite „Sprachübertragung“ (bei abgelaufener Sitzung einmal neu anmelden).
func (f *fritzWeb) sipQual() (string, error) {
	for try := 0; try < 2; try++ {
		if f.sid == "" {
			if err := f.login(); err != nil {
				return "", err
			}
		}
		resp, err := f.hc.PostForm(f.base+"/data.lua", url.Values{"xhr": {"1"}, "sid": {f.sid}, "page": {"sipQual"}, "lang": {"de"}})
		if err != nil {
			return "", fmt.Errorf("Fritzbox nicht erreichbar: %v", err)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		doc := string(b)
		if resp.StatusCode == http.StatusOK && (strings.Contains(doc, "uiListOfAllCalls") || strings.Contains(doc, "uiListOfActivCalls")) {
			return doc, nil
		}
		f.sid = "" // Sitzung abgelaufen bzw. Anmeldeseite – neu anmelden
		if resp.StatusCode == http.StatusOK && try == 1 {
			return "", errors.New("Seite „Sprachübertragung“ nicht verfügbar (Benutzer braucht das Recht „FRITZ!Box Einstellungen“)")
		}
	}
	return "", errors.New("Seite „Sprachübertragung“ nicht abrufbar")
}

// ---------- Agent ----------

// fritzVoiceTracker merkt sich, welche beendeten Gespräche schon an den Server gingen.
type fritzVoiceTracker struct {
	web  *fritzWeb
	sent map[string]bool
}

func (t *fritzVoiceTracker) poll() *fritzVoiceSnap {
	s := &fritzVoiceSnap{At: time.Now().UnixMilli()}
	doc, err := t.web.sipQual()
	if err != nil {
		s.Err = err.Error()
		return s
	}
	act, done := parseSipQual(doc)
	s.Active = act
	if t.sent == nil {
		t.sent = map[string]bool{}
	}
	cur := map[string]bool{}
	for _, v := range done {
		k := v.key()
		cur[k] = true
		if !t.sent[k] {
			s.Calls = append(s.Calls, v)
		}
	}
	t.sent = cur // die Box zeigt höchstens die letzten 100 – ältere fallen heraus
	return s
}

// ---------- Server ----------

const (
	fritzVoiceKeep   = 1500             // gespeicherte Gespräche je Box
	fritzVoiceMinSec = 30               // Meldung erst ab dieser Gesprächsdauer
	fritzVoiceDefMax = 3.0              // Standard-Grenzwert Verlust in %
	fritzVoiceRecent = 3 * 3600 * 1000  // nur frische Gespräche melden (ms)
	fritzVoiceLiveTO = 3 * 60 * 1000    // „läuft“ verfällt ohne neue Meldung
	fritzVoiceShow   = 300              // an den Browser
	fritzVoiceSince  = 24 * 3600 * 1000 // Auswertung „24 h“
)

func fritzVoiceKey(hostID string) string { return "fritzvoice:" + hostID }

func voiceMax(hc HostCfg) float64 {
	if hc.FritzVoiceMax > 0 {
		return hc.FritzVoiceMax
	}
	return fritzVoiceDefMax
}

// fritzVoiceLoad (unter fh.mu) lädt nach einem Server-Neustart das gesicherte Protokoll.
func (m *Monitor) fritzVoiceLoad(hostID string, fh *fritzHost) {
	if fh.voiceLoaded {
		return
	}
	fh.voiceLoaded = true
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	b, err := bs.LoadBlob(fritzVoiceKey(hostID))
	if err != nil || len(b) == 0 {
		return
	}
	var l []fritzVoice
	if json.Unmarshal(b, &l) == nil {
		have := map[string]bool{}
		for _, v := range fh.voice {
			have[v.key()] = true
		}
		for _, v := range l {
			if !have[v.key()] {
				fh.voice = append(fh.voice, v)
			}
		}
		sortVoice(fh.voice)
	}
}

func (m *Monitor) fritzVoiceSave(hostID string, fh *fritzHost) {
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	if b, err := json.Marshal(fh.voice); err == nil {
		bs.SaveBlob(fritzVoiceKey(hostID), b)
	}
}

func sortVoice(l []fritzVoice) {
	sort.SliceStable(l, func(i, j int) bool { return l[i].T > l[j].T })
}

// mergeVoice (unter fh.mu) übernimmt neue Gespräche; Werte aus der Live-Abfrage haben Vorrang
// (sekundengenaue Dauer). Liefert die neu hinzugekommenen.
func mergeVoice(fh *fritzHost, in []fritzVoice) []fritzVoice {
	idx := make(map[string]int, len(fh.voice))
	for i, v := range fh.voice {
		idx[v.key()] = i
	}
	var added []fritzVoice
	for _, v := range in {
		if v.T == 0 {
			continue
		}
		v.Active = false
		if i, ok := idx[v.key()]; ok {
			if fh.voice[i].Src == "mail" && v.Src == "live" {
				fh.voice[i] = v
			}
			continue
		}
		idx[v.key()] = len(fh.voice)
		fh.voice = append(fh.voice, v)
		added = append(added, v)
	}
	if len(added) > 0 {
		sortVoice(fh.voice)
		if len(fh.voice) > fritzVoiceKeep {
			fh.voice = fh.voice[:fritzVoiceKeep]
		}
	}
	return added
}

func fmtLoss(f float64) string {
	if f < 0 {
		return "–"
	}
	return strings.Replace(strconv.FormatFloat(f, 'f', -1, 64), ".", ",", 1) + " %"
}

func fmtMs(f float64) string {
	if f < 0 {
		return "–"
	}
	return strings.Replace(strconv.FormatFloat(f, 'f', -1, 64), ".", ",", 1) + " ms"
}

// voiceBad: Gespräch über dem Grenzwert (und lang genug für eine Meldung).
func voiceBad(v fritzVoice, max float64) bool {
	return v.Sec >= fritzVoiceMinSec && v.maxLoss() >= max
}

// fritzVoiceIngest (unter fh.mu): Abfrage des Agenten übernehmen, schlechte Gespräche melden.
func (m *Monitor) fritzVoiceIngest(hc HostCfg, fh *fritzHost, s *fritzVoiceSnap) []notice {
	m.fritzVoiceLoad(hc.ID, fh)
	if s.At >= fh.voiceAt {
		fh.voiceAt, fh.voiceErr, fh.voiceAct = s.At, s.Err, s.Active
	}
	added := mergeVoice(fh, s.Calls)
	if len(added) == 0 {
		return nil
	}
	m.fritzVoiceSave(hc.ID, fh)
	return m.voiceNotes(hc, added)
}

func (m *Monitor) voiceNotes(hc HostCfg, added []fritzVoice) []notice {
	now := time.Now().UnixMilli()
	max := voiceMax(hc)
	var lines []string
	for _, v := range added {
		if !voiceBad(v, max) || v.T < now-fritzVoiceRecent || m.inMaint(hc.Org, hc.ID, time.UnixMilli(v.T)) {
			continue
		}
		other := v.Other
		if !hc.FritzFullNums {
			other = maskNumber(numDigits(other))
		}
		codec := v.CodecTx
		if v.CodecRx != "" && v.CodecRx != v.CodecTx {
			codec += " / " + v.CodecRx
		}
		lines = append(lines, fmt.Sprintf("%s · %s · %s · %s\nVerlust senden %s, empfangen %s · Jitter %s / %s",
			time.UnixMilli(v.T).In(berlin).Format("02.01. 15:04"), fmtDurShort(int64(v.Sec)*1000), other, codec,
			fmtLoss(v.LossTx), fmtLoss(v.LossRx), fmtMs(v.JitTx), fmtMs(v.JitRx)))
	}
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > 5 {
		lines = append(lines[:5], fmt.Sprintf("… und %d weitere", len(lines)-5))
	}
	title := "📞 Schlechte Sprachqualität: " + hc.Name
	body := fmt.Sprintf("Paketverlust ab %s bei Telefonaten:\n%s", fmtLoss(max), strings.Join(lines, "\n"))
	return []notice{{"fritzvoice", hc.ID, title, m.withInfo(hc.ID, hc.Site, body), hc.Org, ""}}
}

type fritzVoiceView struct {
	On      bool         `json:"on"`
	At      int64        `json:"at,omitempty"`
	Err     string       `json:"err,omitempty"`
	Max     float64      `json:"max"`
	MinSec  int          `json:"minSec"`
	Active  []fritzVoice `json:"active,omitempty"`
	Calls   []fritzVoice `json:"calls,omitempty"`
	Total   int          `json:"total"`
	Calls24 int          `json:"calls24"`
	Bad24   int          `json:"bad24"`
}

func maskVoice(v fritzVoice) fritzVoice {
	v.Other, v.Own = maskNumber(numDigits(v.Other)), maskNumber(numDigits(v.Own))
	return v
}

// fritzVoiceView (ohne fh.mu) für den Browser.
func (m *Monitor) voiceView(hc HostCfg) *fritzVoiceView {
	fh := getFritzHost(hc.ID)
	fh.mu.Lock()
	defer fh.mu.Unlock()
	m.fritzVoiceLoad(hc.ID, fh)
	if !hc.FritzVoice && len(fh.voice) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	v := &fritzVoiceView{On: hc.FritzVoice, At: fh.voiceAt, Err: fh.voiceErr, Max: voiceMax(hc), MinSec: fritzVoiceMinSec, Total: len(fh.voice)}
	if fh.voiceAt > now-fritzVoiceLiveTO {
		v.Active = append(v.Active, fh.voiceAct...)
	}
	for i, c := range fh.voice {
		if c.T >= now-fritzVoiceSince {
			v.Calls24++
			if voiceBad(c, v.Max) {
				v.Bad24++
			}
		}
		if i < fritzVoiceShow {
			v.Calls = append(v.Calls, c)
		}
	}
	if !hc.FritzFullNums {
		for i := range v.Active {
			v.Active[i] = maskVoice(v.Active[i])
		}
		for i := range v.Calls {
			v.Calls[i] = maskVoice(v.Calls[i])
		}
	}
	return v
}

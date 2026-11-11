package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Push-Service-Mails der Fritzboxen einlesen: „FRITZ!Box-Info“ enthält bei aktivierten
// Detailinformationen (Telefonie → Eigene Rufnummern → Sprachübertragung) die Werte aller
// Telefonate seit der letzten Mail. PingWacht holt die Mails per IMAP (Umgebungsvariablen
// VOICE_IMAP_HOST, VOICE_IMAP_USER, VOICE_IMAP_PASS, optional VOICE_IMAP_PORT=993 und
// VOICE_IMAP_FOLDER=INBOX) und ordnet sie über die eigenen Rufnummern der Box zu.
// Mails ohne passende Box landen unter „nicht zugeordnet“ (nur für den Betreiber sichtbar).

// ---------- Mailtext auswerten ----------

type voiceMailData struct {
	MyFritz string                  // z. B. 48tx0wku9lpe4z5o.myfritz.net
	Model   string                  // Produktname
	ByOwn   map[string][]fritzVoice // eigene Rufnummer → Telefonate
	Order   []string                // Reihenfolge der Rufnummern
}

func isCallStart(f []string) bool {
	return len(f) >= 1 && parseFritzTime(f[0]) > 0
}

func fieldIdx(f []string, mark string) int {
	for i, x := range f {
		if x == mark {
			return i
		}
	}
	return -1
}

// parseVoiceMail liest die Tabelle „Internettelefonie Sprachübertragung“ aus dem (Text-)Inhalt.
// Je Telefonat: Zeile „Datum ⇥ Gegenstelle“, Zeile „>| ⇥ Codec ⇥ Pakete ⇥ Verlust ⇥ Verzögerung ⇥ Jitter ⇥ Burst“
// (Senden) und Zeile „Dauer ⇥ IP ⇥ <| ⇥ …“ (Empfangen).
func parseVoiceMail(text string) *voiceMailData {
	d := &voiceMailData{ByOwn: map[string][]fritzVoice{}}
	own := ""
	var cur *fritzVoice
	flush := func() {
		if cur != nil && cur.T > 0 {
			if _, ok := d.ByOwn[own]; !ok {
				d.Order = append(d.Order, own)
			}
			d.ByOwn[own] = append(d.ByOwn[own], *cur)
		}
		cur = nil
	}
	in := false
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		line := strings.ReplaceAll(raw, " ", " ")
		trim := strings.TrimSpace(line)
		if d.MyFritz == "" {
			if i := strings.Index(trim, ".myfritz.net"); i > 0 {
				j := strings.LastIndexAny(trim[:i], "/ \t")
				d.MyFritz = strings.ToLower(trim[j+1:i] + ".myfritz.net")
			}
		}
		if strings.HasPrefix(trim, "Produktname:") {
			d.Model = strings.TrimSpace(strings.TrimPrefix(trim, "Produktname:"))
		}
		if strings.Contains(trim, "Sprachübertragung") && strings.Contains(trim, "Internettelefonie") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if trim == "Ereignisse" || strings.HasPrefix(trim, "Einstellungsübersicht") || strings.HasPrefix(trim, "(*)") {
			flush()
			if trim != "" && !strings.HasPrefix(trim, "(*)") {
				in = false
			}
			continue
		}
		if o, ok := ownFromHeader(trim); ok && strings.HasPrefix(trim, "Eigene Rufnummer") {
			flush()
			own = o
			continue
		}
		f := strings.Split(line, "\t")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		switch {
		case isCallStart(f):
			flush()
			v := fritzVoice{T: parseFritzTime(f[0]), Start: strings.Join(strings.Fields(f[0]), " "), Own: own, Src: "mail",
				LossTx: -1, LossRx: -1, Delay: -1, JitTx: -1, JitRx: -1}
			if len(f) > 1 {
				v.Other = f[1]
				if i := strings.IndexByte(v.Other, '@'); i > 0 {
					v.Other = v.Other[:i]
				}
			}
			cur = &v
		case cur != nil && fieldIdx(f, ">|") >= 0:
			i := fieldIdx(f, ">|")
			g := func(k int) string { return part(f, i+k) }
			cur.CodecTx, cur.PktTx, cur.LossTx = g(1), parseLeadInt(g(2)), parseNumUnit(g(3))
			cur.Delay, cur.JitTx = parseNumUnit(g(4)), parseNumUnit(g(5))
		case cur != nil && fieldIdx(f, "<|") >= 0:
			i := fieldIdx(f, "<|")
			g := func(k int) string { return part(f, i+k) }
			if i >= 1 {
				cur.Sec = parseDurSec(f[0])
			}
			if i >= 2 {
				cur.Remote = f[1]
			}
			cur.CodecRx, cur.PktRx, cur.LossRx = g(1), parseLeadInt(g(2)), parseNumUnit(g(3))
			if cur.Delay < 0 {
				cur.Delay = parseNumUnit(g(4))
			}
			cur.JitRx = parseNumUnit(g(5))
			if b := strings.TrimSpace(g(6)); b != "-" {
				cur.Burst = b
			}
			flush()
		}
	}
	flush()
	return d
}

// htmlToText: Tabellen der Mail als Text (Zellen mit Tab, Zeilen mit Zeilenumbruch, Bilder als alt-Text).
func htmlToText(h string) string {
	var b strings.Builder
	low := strings.ToLower(h)
	i := 0
	for i < len(h) {
		if h[i] != '<' {
			j := strings.IndexByte(h[i:], '<')
			if j < 0 {
				j = len(h) - i
			}
			t := strings.Join(strings.FieldsFunc(h[i:i+j], func(r rune) bool { return r == '\n' || r == '\r' }), " ")
			b.WriteString(t)
			i += j
			continue
		}
		j := strings.IndexByte(h[i:], '>')
		if j < 0 {
			break
		}
		tag := low[i+1 : i+j]
		name := strings.TrimPrefix(strings.Fields(tag + " ")[0], "/")
		closing := strings.HasPrefix(tag, "/")
		switch {
		case name == "br" || name == "tr" && closing || name == "p" && closing || name == "div" && closing || len(name) == 2 && name[0] == 'h' && closing:
			b.WriteString("\n")
		case (name == "td" || name == "th") && closing:
			b.WriteString("\t")
		case name == "img":
			if k := strings.Index(tag, `alt="`); k >= 0 {
				orig := h[i+1+k+5 : i+j]
				if e := strings.IndexByte(orig, '"'); e >= 0 {
					b.WriteString(orig[:e])
				}
			}
		case name == "style" && !closing || name == "script" && !closing:
			if e := strings.Index(low[i:], "</"+name); e > 0 {
				i += e
				continue
			}
		}
		i += j + 1
	}
	return html.UnescapeString(b.String())
}

// mailText: lesbarer Inhalt einer Mail (text/plain bevorzugt, sonst text/html umgewandelt).
func mailText(raw []byte) (subject, text string, err error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	dec := new(mime.WordDecoder)
	dec.CharsetReader = charsetReader
	subject, _ = dec.DecodeHeader(msg.Header.Get("Subject"))
	var plain, htm string
	var walk func(ct, cte string, body io.Reader, depth int)
	walk = func(ct, cte string, body io.Reader, depth int) {
		mt, params, _ := mime.ParseMediaType(ct)
		if mt == "" {
			mt = "text/plain"
		}
		if strings.HasPrefix(mt, "multipart/") && depth < 5 {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err != nil {
					return
				}
				walk(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1)
			}
		}
		if !strings.HasPrefix(mt, "text/") {
			return
		}
		var r io.Reader = body
		switch strings.ToLower(strings.TrimSpace(cte)) {
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, &stripWS{r: r})
		}
		b, _ := io.ReadAll(io.LimitReader(r, 8<<20))
		s := mailCharset(b, params["charset"])
		if mt == "text/html" && htm == "" {
			htm = s
		} else if mt == "text/plain" && plain == "" {
			plain = s
		}
	}
	walk(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0)
	switch {
	case strings.Contains(plain, ">|") || htm == "":
		text = plain
	default:
		text = htmlToText(htm)
	}
	return subject, text, nil
}

// stripWS entfernt Zeilenumbrüche aus Base64-Inhalten.
type stripWS struct{ r io.Reader }

func (s *stripWS) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	k := 0
	for _, c := range p[:n] {
		if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
			p[k] = c
			k++
		}
	}
	return k, err
}

func mailCharset(b []byte, cs string) string {
	switch strings.ToLower(strings.TrimSpace(cs)) {
	case "iso-8859-1", "latin1", "iso-8859-15", "windows-1252", "us-ascii":
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r)
	}
	return string(b)
}

func charsetReader(cs string, in io.Reader) (io.Reader, error) {
	b, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(mailCharset(b, cs)), nil
}

// ---------- IMAP (nur das Nötigste: anmelden, Ordner wählen, suchen, abholen) ----------

type imapConn struct {
	c   net.Conn
	r   *bufio.Reader
	tag int
}

func imapQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// cmd schickt einen Befehl und liefert die Antwortzeilen samt Literalen (Literal direkt an die Zeile gehängt).
func (ic *imapConn) cmd(format string, a ...any) ([]string, [][]byte, error) {
	ic.tag++
	t := "a" + strconv.Itoa(ic.tag)
	ic.c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := fmt.Fprintf(ic.c, "%s %s\r\n", t, fmt.Sprintf(format, a...)); err != nil {
		return nil, nil, err
	}
	var lines []string
	var lits [][]byte
	for {
		l, err := ic.r.ReadString('\n')
		if err != nil {
			return lines, lits, err
		}
		l = strings.TrimRight(l, "\r\n")
		if strings.HasSuffix(l, "}") {
			if k := strings.LastIndexByte(l, '{'); k >= 0 {
				if n, err := strconv.Atoi(l[k+1 : len(l)-1]); err == nil && n >= 0 && n < 32<<20 {
					buf := make([]byte, n)
					if _, err := io.ReadFull(ic.r, buf); err != nil {
						return lines, lits, err
					}
					lits = append(lits, buf)
				}
			}
		}
		lines = append(lines, l)
		if strings.HasPrefix(l, t+" ") {
			rest := strings.TrimPrefix(l, t+" ")
			if !strings.HasPrefix(rest, "OK") {
				return lines, lits, errors.New("IMAP: " + rest)
			}
			return lines, lits, nil
		}
	}
}

func imapDial(host string, port int, user, pass string) (*imapConn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{ServerName: host})
	if err != nil {
		return nil, err
	}
	ic := &imapConn{c: c, r: bufio.NewReader(c)}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := ic.r.ReadString('\n'); err != nil {
		c.Close()
		return nil, err
	}
	if _, _, err := ic.cmd("LOGIN %s %s", imapQuote(user), imapQuote(pass)); err != nil {
		c.Close()
		return nil, errors.New("IMAP-Anmeldung fehlgeschlagen")
	}
	return ic, nil
}

func (ic *imapConn) close() {
	ic.cmd("LOGOUT")
	ic.c.Close()
}

// ---------- Abruf, Zuordnung, nicht zugeordnete Mails ----------

type voiceUnassigned struct {
	Number  string       `json:"number"`
	MyFritz string       `json:"myfritz,omitempty"`
	Model   string       `json:"model,omitempty"`
	Last    int64        `json:"last"` // letzte Mail
	Count   int          `json:"count"`
	Calls   []fritzVoice `json:"calls,omitempty"`
}

type voiceMailState struct {
	Validity   uint32             `json:"validity"`
	Done       []uint32           `json:"done"`
	Unassigned []*voiceUnassigned `json:"unassigned,omitempty"`
	LastRun    int64              `json:"lastRun,omitempty"`
	LastErr    string             `json:"lastErr,omitempty"`
	Imported   int                `json:"imported,omitempty"`
}

var voiceMail = struct {
	sync.Mutex
	st     *voiceMailState
	loaded bool
}{}

const voiceMailKey = "voicemail:state"

func (m *Monitor) voiceMailLoadLocked() *voiceMailState {
	if voiceMail.loaded {
		return voiceMail.st
	}
	voiceMail.loaded = true
	voiceMail.st = &voiceMailState{}
	if bs, ok := m.store.(blobStore); ok {
		if b, err := bs.LoadBlob(voiceMailKey); err == nil && len(b) > 0 {
			json.Unmarshal(b, voiceMail.st)
		}
	}
	return voiceMail.st
}

func (m *Monitor) voiceMailSaveLocked() {
	if bs, ok := m.store.(blobStore); ok {
		if b, err := json.Marshal(voiceMail.st); err == nil {
			bs.SaveBlob(voiceMailKey, b)
		}
	}
}

func voiceMailEnv() (host string, port int, user, pass, folder string) {
	host, user, pass = os.Getenv("VOICE_IMAP_HOST"), os.Getenv("VOICE_IMAP_USER"), os.Getenv("VOICE_IMAP_PASS")
	port, _ = strconv.Atoi(envOr("VOICE_IMAP_PORT", "993"))
	if port <= 0 {
		port = 993
	}
	folder = envOr("VOICE_IMAP_FOLDER", "INBOX")
	return
}

// voiceMailLoop holt alle 10 Minuten neue Mails (nur wenn VOICE_IMAP_* gesetzt ist).
func (m *Monitor) voiceMailLoop() {
	if h, _, u, p, _ := voiceMailEnv(); h == "" || u == "" || p == "" {
		return
	}
	time.Sleep(45 * time.Second)
	for {
		if err := m.voiceMailRun(); err != nil {
			log.Printf("Sprachqualität-Mails: %v", err)
		}
		time.Sleep(10 * time.Minute)
	}
}

func (m *Monitor) voiceMailRun() error {
	host, port, user, pass, folder := voiceMailEnv()
	ic, err := imapDial(host, port, user, pass)
	if err == nil {
		defer ic.close()
		err = m.voiceMailFetch(ic, folder)
	}
	voiceMail.Lock()
	st := m.voiceMailLoadLocked()
	st.LastRun = time.Now().UnixMilli()
	st.LastErr = ""
	if err != nil {
		st.LastErr = err.Error()
	}
	m.voiceMailSaveLocked()
	voiceMail.Unlock()
	return err
}

func (m *Monitor) voiceMailFetch(ic *imapConn, folder string) error {
	lines, _, err := ic.cmd("SELECT %s", imapQuote(folder))
	if err != nil {
		return err
	}
	var validity uint32
	for _, l := range lines {
		if k := strings.Index(l, "[UIDVALIDITY "); k >= 0 {
			v := l[k+13:]
			if e := strings.IndexByte(v, ']'); e > 0 {
				n, _ := strconv.ParseUint(strings.TrimSpace(v[:e]), 10, 32)
				validity = uint32(n)
			}
		}
	}
	since := time.Now().AddDate(0, 0, -14).Format("02-Jan-2006")
	lines, _, err = ic.cmd("UID SEARCH SINCE %s", since)
	if err != nil {
		return err
	}
	var uids []uint32
	for _, l := range lines {
		if strings.HasPrefix(l, "* SEARCH") {
			for _, f := range strings.Fields(l)[2:] {
				if n, err := strconv.ParseUint(f, 10, 32); err == nil {
					uids = append(uids, uint32(n))
				}
			}
		}
	}
	voiceMail.Lock()
	st := m.voiceMailLoadLocked()
	if st.Validity != validity {
		st.Validity, st.Done = validity, nil
	}
	done := map[uint32]bool{}
	for _, u := range st.Done {
		done[u] = true
	}
	voiceMail.Unlock()
	for _, u := range uids {
		if done[u] {
			continue
		}
		_, lits, err := ic.cmd("UID FETCH %d (BODY.PEEK[])", u)
		if err != nil {
			return err
		}
		for _, raw := range lits {
			if len(raw) > 200 {
				m.voiceMailImport(raw)
			}
		}
		voiceMail.Lock()
		st.Done = append(st.Done, u)
		if len(st.Done) > 5000 {
			st.Done = st.Done[len(st.Done)-5000:]
		}
		m.voiceMailSaveLocked()
		voiceMail.Unlock()
	}
	return nil
}

// voiceNumberIndex: Rufnummer → Host (Rufnummern aus TR-064 und von Hand eingetragene).
func (m *Monitor) voiceNumberIndex() map[string]HostCfg {
	idx := map[string]HostCfg{}
	for _, h := range m.hostList() {
		h.mu.Lock()
		hc := h.cfg
		h.mu.Unlock()
		if !hc.Fritz {
			continue
		}
		for _, n := range strings.FieldsFunc(hc.FritzVoiceNums, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
			if d := numDigits(n); len(d) >= 5 {
				idx[d] = hc
			}
		}
		fh := getFritzHost(hc.ID)
		fh.mu.Lock()
		if fh.extra != nil {
			for _, v := range fh.extra.VoIP {
				if d := numDigits(v.Number); len(d) >= 5 {
					if _, ok := idx[d]; !ok {
						idx[d] = hc
					}
				}
			}
		}
		fh.mu.Unlock()
	}
	return idx
}

// voiceMailImport ordnet eine Mail zu und übernimmt die Telefonate; liefert die Anzahl neuer Einträge.
func (m *Monitor) voiceMailImport(raw []byte) int {
	_, text, err := mailText(raw)
	if err != nil || !strings.Contains(text, "Sprachübertragung") {
		return 0
	}
	d := parseVoiceMail(text)
	if len(d.Order) == 0 {
		return 0
	}
	idx := m.voiceNumberIndex()
	n := 0
	for _, own := range d.Order {
		calls := d.ByOwn[own]
		if hc, ok := idx[numDigits(own)]; ok {
			n += m.voiceAddCalls(hc, calls)
			continue
		}
		m.voiceAddUnassigned(own, d, calls)
	}
	voiceMail.Lock()
	m.voiceMailLoadLocked().Imported += n
	voiceMail.Unlock()
	return n
}

func (m *Monitor) voiceAddCalls(hc HostCfg, calls []fritzVoice) int {
	fh := getFritzHost(hc.ID)
	fh.mu.Lock()
	m.fritzVoiceLoad(hc.ID, fh)
	added := mergeVoice(fh, calls)
	if len(added) > 0 {
		m.fritzVoiceSave(hc.ID, fh)
	}
	fh.mu.Unlock()
	for _, n := range m.voiceNotes(hc, added) {
		m.notify(n)
	}
	return len(added)
}

func (m *Monitor) voiceAddUnassigned(own string, d *voiceMailData, calls []fritzVoice) {
	voiceMail.Lock()
	defer voiceMail.Unlock()
	st := m.voiceMailLoadLocked()
	key := numDigits(own)
	var u *voiceUnassigned
	for _, x := range st.Unassigned {
		if numDigits(x.Number) == key {
			u = x
		}
	}
	if u == nil {
		u = &voiceUnassigned{Number: own}
		st.Unassigned = append(st.Unassigned, u)
	}
	u.Last, u.Count = time.Now().UnixMilli(), u.Count+1
	if d.MyFritz != "" {
		u.MyFritz = d.MyFritz
	}
	if d.Model != "" {
		u.Model = d.Model
	}
	have := map[string]bool{}
	for _, c := range u.Calls {
		have[c.key()] = true
	}
	for _, c := range calls {
		if !have[c.key()] {
			u.Calls = append(u.Calls, c)
		}
	}
	sortVoice(u.Calls)
	if len(u.Calls) > 300 {
		u.Calls = u.Calls[:300]
	}
	if len(st.Unassigned) > 200 {
		sort.Slice(st.Unassigned, func(i, j int) bool { return st.Unassigned[i].Last > st.Unassigned[j].Last })
		st.Unassigned = st.Unassigned[:200]
	}
	m.voiceMailSaveLocked()
}

type voiceMailInfo struct {
	On         bool              `json:"on"`
	LastRun    int64             `json:"lastRun,omitempty"`
	LastErr    string            `json:"lastErr,omitempty"`
	Imported   int               `json:"imported"`
	Unassigned []voiceUnassigned `json:"unassigned,omitempty"`
}

// voiceMailView: Stand des Mail-Abrufs und nicht zugeordnete Rufnummern (ohne Telefonate).
func (m *Monitor) voiceMailView() voiceMailInfo {
	h, _, u, p, _ := voiceMailEnv()
	voiceMail.Lock()
	defer voiceMail.Unlock()
	st := m.voiceMailLoadLocked()
	v := voiceMailInfo{On: h != "" && u != "" && p != "", LastRun: st.LastRun, LastErr: st.LastErr, Imported: st.Imported}
	for _, x := range st.Unassigned {
		c := *x
		c.Calls = nil
		c.Count = len(x.Calls)
		v.Unassigned = append(v.Unassigned, c)
	}
	sort.Slice(v.Unassigned, func(i, j int) bool { return v.Unassigned[i].Last > v.Unassigned[j].Last })
	return v
}

// voiceAssign: Rufnummer einer Box zuordnen und die gesammelten Telefonate übernehmen.
func (m *Monitor) voiceAssign(hc HostCfg, number string) (int, error) {
	key := numDigits(number)
	if len(key) < 5 {
		return 0, errors.New("ungültige Rufnummer")
	}
	nums := hc.FritzVoiceNums
	if !strings.Contains(","+strings.ReplaceAll(nums, " ", "")+",", ","+number+",") {
		if nums != "" {
			nums += ", "
		}
		nums += number
	}
	hc.FritzVoiceNums = nums
	saved := m.UpsertHost(hc)
	voiceMail.Lock()
	st := m.voiceMailLoadLocked()
	var calls []fritzVoice
	var keep []*voiceUnassigned
	for _, x := range st.Unassigned {
		if numDigits(x.Number) == key {
			calls = append(calls, x.Calls...)
			continue
		}
		keep = append(keep, x)
	}
	st.Unassigned = keep
	m.voiceMailSaveLocked()
	voiceMail.Unlock()
	for i := range calls {
		calls[i].Own = number
	}
	return m.voiceAddCalls(saved, calls), nil
}

// GET /api/fritz/voicemail – Stand des Mail-Abrufs (nur Betreiber)
// POST /api/fritz/voicemail {host, number} – Rufnummer einer Box zuordnen (nur Betreiber)
func (s *Server) handleVoiceMail(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p == nil || !p.Super {
		http.Error(w, "nur für den Betreiber", 403)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, s.m.voiceMailView())
		return
	}
	var req struct {
		Host   string `json:"host"`
		Number string `json:"number"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	h := s.m.host(req.Host)
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	h.mu.Lock()
	hc := h.cfg
	h.mu.Unlock()
	if !hc.Fritz {
		http.Error(w, "Host ist keine Fritzbox", 400)
		return
	}
	n, err := s.m.voiceAssign(hc, strings.TrimSpace(req.Number))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.m.audit(hc.Org, whoOf(p), "geändert", "Host", hc.Name, "Rufnummer "+maskNumber(req.Number)+" für Sprachqualität-Mails zugeordnet")
	writeJSON(w, map[string]any{"ok": true, "imported": n})
}

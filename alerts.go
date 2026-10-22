package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Channel ist ein Alarmweg einer Organisation: E-Mail, Microsoft Teams, Telegram oder Webhook.
type Channel struct {
	ID       string   `json:"id"`
	Org      string   `json:"org"`
	Type     string   `json:"type"` // email, teams, telegram, webhook
	Name     string   `json:"name"`
	Target   string   `json:"target"`           // E-Mail-Adressen (Komma), Webhook-URL oder Telegram-Chat-ID
	Secret   string   `json:"secret,omitempty"` // Telegram-Bot-Token (optional) bzw. Webhook-Signaturschlüssel
	Outage   bool     `json:"outage"`
	Loss     bool     `json:"loss"`
	Spike    bool     `json:"spike"`
	Offline  bool     `json:"offline"`
	Report   bool     `json:"report"`           // Monatsbericht (nur E-Mail)
	Hosts    []string `json:"hosts"`            // leer = alle
	Groups   []string `json:"groups,omitempty"` // Kunden (Gruppen) – zusätzlich zu Hosts; leer = alle
	Disabled bool     `json:"disabled,omitempty"`
	Escalate int      `json:"escalate,omitempty"` // Minuten: nur Ausfälle, die so lange offen und nicht quittiert sind
	Created  int64    `json:"created"`
}

func (c Channel) wants(n notice) bool { return c.wantsIn(n, "") }

// wantsIn prüft zusätzlich die Kunden-Auswahl (group = Gruppe des Hosts der Meldung).
func (c Channel) wantsIn(n notice, group string) bool {
	if c.Disabled {
		return false
	}
	if len(c.Groups) > 0 && n.hostID != "" {
		for _, g := range c.Groups {
			if g == group && group != "" {
				d := Device{Outage: c.Outage, Loss: c.Loss, Spike: c.Spike, Offline: c.Offline}
				return d.wants(n)
			}
		}
		if len(c.Hosts) == 0 {
			return false // Host gehört zu keinem gewählten Kunden
		}
	}
	d := Device{Outage: c.Outage, Loss: c.Loss, Spike: c.Spike, Offline: c.Offline, Hosts: c.Hosts}
	return d.wants(n)
}

type chanStatus struct {
	LastOK  int64  `json:"lastOK"`
	LastErr string `json:"lastErr"`
	ErrAt   int64  `json:"errAt"`
}

var alertState = struct {
	sync.Mutex
	status   map[string]chanStatus
	throttle map[string]time.Time
}{status: map[string]chanStatus{}, throttle: map[string]time.Time{}}

func setChanStatus(id string, err error) {
	alertState.Lock()
	st := alertState.status[id]
	if err == nil {
		st.LastOK = time.Now().UnixMilli()
		st.LastErr = ""
	} else {
		st.LastErr = err.Error()
		st.ErrAt = time.Now().UnixMilli()
	}
	alertState.status[id] = st
	alertState.Unlock()
}

// alertClient: kurze Zeitüberschreitung, damit ein hängender Dienst nichts blockiert.
var alertClient = &http.Client{Timeout: 15 * time.Second}

// telegramAPI kann in Tests überschrieben werden.
var telegramAPI = "https://api.telegram.org"

// dispatchAlerts verteilt eine Meldung an alle passenden Kanäle der Organisation.
func (m *Monitor) dispatchAlerts(n notice) {
	if n.org == "" {
		return
	}
	o := m.orgByID(n.org)
	if o == nil || o.Disabled || !planHas(o.Plan, "alerts") {
		return
	}
	m.mu.RLock()
	var chans []Channel
	group := m.hostGroupLocked(n.hostID)
	for _, c := range m.cfg.Channels {
		if c.Org == n.org && c.wantsIn(n, group) {
			chans = append(chans, c)
		}
	}
	m.mu.RUnlock()
	at := time.Now()
	for _, c := range chans {
		if c.Escalate > 0 {
			m.escalate(c, n, o.Name)
			continue
		}
		if n.kind == "loss" || n.kind == "spike" {
			k := c.ID + "|" + n.hostID + "|" + n.kind
			alertState.Lock()
			if time.Since(alertState.throttle[k]) < 10*time.Minute {
				alertState.Unlock()
				continue
			}
			alertState.throttle[k] = at
			alertState.Unlock()
		}
		go func(c Channel) {
			var err error
			for try := 0; try < 3; try++ {
				if err = sendAlert(c, n, at, o.Name); err == nil {
					break
				}
				time.Sleep(time.Duration(5*(try+1)) * time.Second)
			}
			setChanStatus(c.ID, err)
			if err != nil {
				log.Printf("Alarm %s (%s): %v", c.Name, c.Type, err)
			}
		}(c)
	}
}

// escalate: Eskalationswege bekommen nur Host-Ausfälle, die nach c.Escalate Minuten
// noch offen und nicht quittiert sind – und dann auch die Entwarnung.
var escalated sync.Map // Kanal|Host → true

var escalateUnit = time.Minute

func (m *Monitor) escalate(c Channel, n notice, orgName string) {
	key := c.ID + "|" + n.hostID
	switch n.kind {
	case "outage":
		if n.hostID == "" {
			return
		}
		time.AfterFunc(time.Duration(c.Escalate)*escalateUnit, func() {
			if !m.outageOpenUnacked(n.hostID) {
				return
			}
			nn := n
			nn.title = "⏫ " + n.title
			nn.body = fmt.Sprintf("%s – seit %d Minuten nicht quittiert", n.body, c.Escalate)
			err := sendAlert(c, nn, time.Now(), orgName)
			setChanStatus(c.ID, err)
			if err == nil {
				escalated.Store(key, true)
			}
		})
	case "recovered":
		if _, ok := escalated.LoadAndDelete(key); ok {
			setChanStatus(c.ID, sendAlert(c, n, time.Now(), orgName))
		}
	}
}

func (m *Monitor) outageOpenUnacked(hostID string) bool {
	h := m.host(hostID)
	if h == nil {
		return false
	}
	h.mu.Lock()
	e := h.oOut
	h.mu.Unlock()
	if e == nil {
		return false
	}
	m.evMu.Lock()
	defer m.evMu.Unlock()
	return e.End == 0 && e.AckAt == 0 && !e.quiet()
}

func planHas(plan, feature string) bool {
	for _, f := range planByID(plan).Features {
		if f == feature {
			return true
		}
	}
	return false
}

func kindLabel(k string) string {
	return map[string]string{"outage": "Ausfall", "recovered": "Wieder erreichbar", "loss": "Paketverlust", "spike": "Latenzspitze",
		"offline": "Standort offline", "online": "Standort online", "test": "Test"}[k]
}

func kindColor(k string) string {
	switch k {
	case "outage", "offline":
		return "#dc2626"
	case "loss":
		return "#ea580c"
	case "spike":
		return "#ca8a04"
	case "recovered", "online":
		return "#16a34a"
	}
	return "#2563eb"
}

func sendAlert(c Channel, n notice, at time.Time, orgName string) error {
	switch c.Type {
	case "email":
		subj := "[PingWatch] " + n.title
		txt := n.title + "\n" + n.body + "\n\n" + at.Format("02.01.2006 15:04:05") + " · " + orgName + "\n" + dashboardURL() + "\n"
		htm := fmt.Sprintf(`<div style="font-family:-apple-system,Segoe UI,Arial,sans-serif;max-width:560px">
<div style="border-left:4px solid %s;padding:10px 14px;background:#f8fafc">
<div style="font-size:17px;font-weight:600;color:#0f172a">%s</div>
<div style="font-size:14px;color:#334155;margin-top:4px">%s</div></div>
<p style="font-size:12px;color:#64748b">%s · %s%s</p></div>`,
			kindColor(n.kind), html.EscapeString(n.title), strings.ReplaceAll(html.EscapeString(n.body), "\n", "<br>"), at.Format("02.01.2006 15:04:05"),
			html.EscapeString(orgName), linkHTML(dashboardURL()))
		return sendMail(splitAddrs(c.Target), subj, txt, htm, nil)
	case "teams":
		card := map[string]any{
			"type": "message",
			"attachments": []any{map[string]any{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content": map[string]any{
					"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"type":    "AdaptiveCard", "version": "1.4",
					"body": []any{
						map[string]any{"type": "TextBlock", "text": n.title, "weight": "Bolder", "size": "Medium", "wrap": true,
							"color": map[string]string{"outage": "Attention", "offline": "Attention", "loss": "Warning", "spike": "Warning", "recovered": "Good", "online": "Good"}[n.kind]},
						map[string]any{"type": "TextBlock", "text": strings.ReplaceAll(n.body, "\n", "\n\n"), "wrap": true},
						map[string]any{"type": "TextBlock", "text": at.Format("02.01.2006 15:04:05") + " · " + orgName, "isSubtle": true, "size": "Small", "wrap": true},
					},
					"actions": teamsActions(),
				},
			}},
		}
		return postJSON(c.Target, card, nil)
	case "telegram":
		tok := c.Secret
		if tok == "" {
			tok = os.Getenv("TELEGRAM_BOT_TOKEN")
		}
		if tok == "" {
			return errors.New("kein Telegram-Bot eingerichtet")
		}
		text := "<b>" + html.EscapeString(n.title) + "</b>\n" + html.EscapeString(n.body) + "\n<i>" + at.Format("02.01. 15:04:05") + " · " + html.EscapeString(orgName) + "</i>"
		return postJSON(telegramAPI+"/bot"+tok+"/sendMessage", map[string]any{"chat_id": c.Target, "text": text, "parse_mode": "HTML",
			"disable_web_page_preview": true}, nil)
	case "webhook":
		payload := map[string]any{"event": n.kind, "label": kindLabel(n.kind), "title": n.title, "message": n.body,
			"host": n.hostID, "org": n.org, "orgName": orgName, "time": at.UTC().Format(time.RFC3339), "timestamp": at.UnixMilli()}
		b, _ := json.Marshal(payload)
		hdr := map[string]string{}
		if c.Secret != "" {
			mac := hmac.New(sha256.New, []byte(c.Secret))
			mac.Write(b)
			hdr["X-PingWatch-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		return postRaw(c.Target, b, hdr)
	}
	return fmt.Errorf("unbekannter Kanaltyp %q", c.Type)
}

func teamsActions() []any {
	if u := dashboardURL(); u != "" {
		return []any{map[string]any{"type": "Action.OpenUrl", "title": "PingWatch öffnen", "url": u}}
	}
	return []any{}
}

var lastBase sync.Map

// dashboardURL: PUBLIC_URL oder die zuletzt im Browser benutzte Adresse.
func dashboardURL() string {
	if u := os.Getenv("PUBLIC_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	if v, ok := lastBase.Load("u"); ok {
		return v.(string)
	}
	return ""
}

func linkHTML(u string) string {
	if u == "" {
		return ""
	}
	return ` · <a href="` + html.EscapeString(u) + `">PingWatch öffnen</a>`
}

func postJSON(url string, v any, hdr map[string]string) error {
	b, _ := json.Marshal(v)
	return postRaw(url, b, hdr)
}

func postRaw(url string, body []byte, hdr map[string]string) error {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return errors.New("URL muss mit https:// beginnen")
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "PingWatch/"+version)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := alertClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func splitAddrs(s string) []string {
	var out []string
	for _, a := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// ---------- E-Mail über den eigenen Mailserver ----------

type attachment struct {
	Name, Type string
	Data       []byte
}

func smtpConfigured() bool { return os.Getenv("SMTP_HOST") != "" }

// sendMailFn kann in Tests ersetzt werden.
var sendMailFn = sendMailSMTP

func sendMail(to []string, subject, text, htmlBody string, att *attachment) error {
	if len(to) == 0 {
		return errors.New("keine Empfänger")
	}
	for _, a := range to {
		if !validEmail(a) {
			return fmt.Errorf("ungültige Adresse %q", a)
		}
	}
	return sendMailFn(to, buildMail(to, subject, text, htmlBody, att))
}

func mailFrom() string {
	return envOr("SMTP_FROM", "PingWatch <pingwatch@"+envOr("SMTP_DOMAIN", "rst.de")+">")
}

func addrOnly(s string) string {
	if i := strings.LastIndex(s, "<"); i >= 0 {
		return strings.Trim(s[i:], "<> ")
	}
	return strings.TrimSpace(s)
}

func buildMail(to []string, subject, text, htmlBody string, att *attachment) []byte {
	var b bytes.Buffer
	from := mailFrom()
	if i := strings.Index(from, "<"); i > 0 {
		from = mime.QEncoding.Encode("utf-8", strings.TrimSpace(from[:i])) + " " + from[i:]
	}
	bnd := "pw" + randHex(12)
	alt := "pa" + randHex(12)
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@pingwatch>\r\nMIME-Version: 1.0\r\nAuto-Submitted: auto-generated\r\n",
		from, strings.Join(to, ", "), mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z), randHex(16))
	writeAlt := func() {
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", alt)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n", alt, wrap76(base64.StdEncoding.EncodeToString([]byte(text))))
		if htmlBody != "" {
			fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n", alt, wrap76(base64.StdEncoding.EncodeToString([]byte(htmlBody))))
		}
		fmt.Fprintf(&b, "--%s--\r\n", alt)
	}
	if att == nil {
		writeAlt()
		return b.Bytes()
	}
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n--%s\r\n", bnd, bnd)
	writeAlt()
	fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; name=\"%s\"\r\nContent-Disposition: attachment; filename=\"%s\"\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n--%s--\r\n",
		bnd, att.Type, att.Name, att.Name, wrap76(base64.StdEncoding.EncodeToString(att.Data)), bnd)
	return b.Bytes()
}

func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

// looseAuth: AUTH PLAIN auch ohne TLS (nur wenn SMTP_TLS=none ausdrücklich gesetzt ist).
type looseAuth struct{ user, pass string }

func (a looseAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}
func (a looseAuth) Next([]byte, bool) ([]byte, error) { return nil, nil }

// sendMailSMTP: SMTP_HOST, SMTP_PORT (587), SMTP_TLS (starttls | tls | none), SMTP_USER, SMTP_PASSWORD,
// SMTP_FROM, SMTP_INSECURE=1 (Zertifikat nicht prüfen, z. B. interner Server).
func sendMailSMTP(to []string, msg []byte) error {
	host := os.Getenv("SMTP_HOST")
	if host == "" {
		return errors.New("Mailserver nicht eingerichtet (SMTP_HOST)")
	}
	port := envOr("SMTP_PORT", "587")
	mode := strings.ToLower(os.Getenv("SMTP_TLS"))
	if mode == "" {
		mode = "starttls"
		if port == "465" {
			mode = "tls"
		}
	}
	tcfg := &tls.Config{ServerName: host, InsecureSkipVerify: os.Getenv("SMTP_INSECURE") == "1"}
	addr := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if mode == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tcfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("Mailserver %s nicht erreichbar: %v", addr, err)
	}
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello(envOr("SMTP_HELO", "pingwatch")); err != nil {
		return err
	}
	if mode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("Mailserver bietet kein STARTTLS an (SMTP_TLS=none setzen, falls gewollt)")
		}
		if err := c.StartTLS(tcfg); err != nil {
			return fmt.Errorf("STARTTLS: %v", err)
		}
	}
	if u := os.Getenv("SMTP_USER"); u != "" {
		var auth smtp.Auth = smtp.PlainAuth("", u, os.Getenv("SMTP_PASSWORD"), host)
		if mode == "none" {
			auth = looseAuth{u, os.Getenv("SMTP_PASSWORD")}
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("Anmeldung am Mailserver: %v", err)
		}
	}
	if err := c.Mail(addrOnly(mailFrom())); err != nil {
		return err
	}
	for _, a := range to {
		if err := c.Rcpt(a); err != nil {
			return fmt.Errorf("Empfänger %s: %v", a, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// ---------- API ----------

type channelView struct {
	Channel
	HasSecret bool `json:"hasSecret"`
	chanStatus
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	o := m.orgByID(p.Org)
	if o == nil {
		http.Error(w, "Kunde unbekannt", 404)
		return
	}
	switch r.Method {
	case http.MethodGet:
		out := []channelView{}
		m.mu.RLock()
		for _, c := range m.cfg.Channels {
			if c.Org == p.Org {
				v := channelView{Channel: c, HasSecret: c.Secret != ""}
				v.Secret = ""
				alertState.Lock()
				v.chanStatus = alertState.status[c.ID]
				alertState.Unlock()
				out = append(out, v)
			}
		}
		m.mu.RUnlock()
		writeJSON(w, map[string]any{"channels": out, "allowed": planHas(o.Plan, "alerts"), "report": planHas(o.Plan, "report"),
			"smtp": smtpConfigured(), "telegramBot": os.Getenv("TELEGRAM_BOT_TOKEN") != "", "telegramName": os.Getenv("TELEGRAM_BOT_NAME")})
	case http.MethodPost:
		if !planHas(o.Plan, "alerts") {
			http.Error(w, "Alarme per E-Mail, Teams, Telegram und Webhook gibt es ab dem Paket Basis", 402)
			return
		}
		var c Channel
		if !readJSON(w, r, &c) {
			return
		}
		c.Name = strings.TrimSpace(c.Name)
		c.Target = strings.TrimSpace(c.Target)
		if err := validateChannel(&c); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if c.Report && !planHas(o.Plan, "report") {
			c.Report = false
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if c.ID == "" {
			n := 0
			for _, x := range m.cfg.Channels {
				if x.Org == p.Org {
					n++
				}
			}
			if n >= 20 {
				http.Error(w, "Höchstens 20 Alarmwege", 400)
				return
			}
			c.ID, c.Org, c.Created = randHex(6), p.Org, time.Now().UnixMilli()
			m.cfg.Channels = append(m.cfg.Channels, c)
		} else {
			found := false
			for i := range m.cfg.Channels {
				x := &m.cfg.Channels[i]
				if x.ID == c.ID && x.Org == p.Org {
					if c.Secret == "" {
						c.Secret = x.Secret
					} else if c.Secret == "-" {
						c.Secret = ""
					}
					c.Org, c.Created = x.Org, x.Created
					*x = c
					found = true
				}
			}
			if !found {
				http.Error(w, "Alarmweg unbekannt", 404)
				return
			}
		}
		m.saveConfigLocked()
		writeJSON(w, map[string]any{"ok": true, "id": c.ID})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		out := m.cfg.Channels[:0]
		for _, c := range m.cfg.Channels {
			if !(c.ID == id && c.Org == p.Org) {
				out = append(out, c)
			}
		}
		m.cfg.Channels = out
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

func validateChannel(c *Channel) error {
	switch c.Type {
	case "email":
		as := splitAddrs(c.Target)
		if len(as) == 0 || len(as) > 10 {
			return errors.New("Bitte 1 bis 10 E-Mail-Adressen angeben")
		}
		for _, a := range as {
			if !validEmail(strings.ToLower(a)) {
				return fmt.Errorf("Ungültige E-Mail-Adresse: %s", a)
			}
		}
		c.Target = strings.ToLower(strings.Join(as, ", "))
	case "teams", "webhook":
		if !strings.HasPrefix(c.Target, "https://") {
			return errors.New("Die Webhook-Adresse muss mit https:// beginnen")
		}
	case "telegram":
		if _, err := strconv.ParseInt(c.Target, 10, 64); err != nil && !strings.HasPrefix(c.Target, "@") {
			return errors.New("Telegram: Chat-ID (Zahl) oder @Kanalname angeben")
		}
		if c.Secret != "" && c.Secret != "-" && !strings.Contains(c.Secret, ":") {
			return errors.New("Telegram-Bot-Token hat das Format 123456:ABC…")
		}
		if c.Secret == "" && c.ID == "" && os.Getenv("TELEGRAM_BOT_TOKEN") == "" {
			return errors.New("Bitte das Token Ihres Telegram-Bots angeben")
		}
	default:
		return errors.New("Unbekannter Typ")
	}
	if c.Name == "" {
		c.Name = map[string]string{"email": "E-Mail", "teams": "Teams", "telegram": "Telegram", "webhook": "Webhook"}[c.Type]
	}
	if !c.Outage && !c.Loss && !c.Spike && !c.Offline && !c.Report {
		return errors.New("Bitte mindestens eine Meldungsart auswählen")
	}
	if c.Report && c.Type != "email" {
		c.Report = false
	}
	if c.Escalate < 0 || c.Escalate > 1440 {
		return errors.New("Eskalation: 0 bis 1440 Minuten")
	}
	if c.Escalate > 0 {
		c.Loss, c.Spike = false, false // Eskalation nur für Ausfälle
		c.Outage = true
	}
	return nil
}

func (s *Server) channelOf(r *http.Request, id string) *Channel {
	p := principal(r)
	s.m.mu.RLock()
	defer s.m.mu.RUnlock()
	for _, c := range s.m.cfg.Channels {
		if c.ID == id && c.Org == p.Org {
			cc := c
			return &cc
		}
	}
	return nil
}

func (s *Server) handleAlertTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	c := s.channelOf(r, req.ID)
	if c == nil {
		http.Error(w, "Alarmweg unbekannt", 404)
		return
	}
	o := s.m.orgByID(c.Org)
	err := sendAlert(*c, notice{"test", "", "✅ PingWatch-Test", "Dieser Alarmweg funktioniert. Ab jetzt kommen hier die Meldungen an.", c.Org}, time.Now(), o.Name)
	setChanStatus(c.ID, err)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type tgMsg struct {
	Chat struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"chat"`
}

// handleTelegramChats liest über getUpdates die Chats, die dem Bot zuletzt geschrieben haben.
func (s *Server) handleTelegramChats(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tok := strings.TrimSpace(req.Token)
	if tok == "" && req.ID != "" {
		if c := s.channelOf(r, req.ID); c != nil {
			tok = c.Secret
		}
	}
	if tok == "" {
		tok = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	if tok == "" {
		http.Error(w, "Kein Bot-Token", 400)
		return
	}
	resp, err := alertClient.Get(telegramAPI + "/bot" + tok + "/getUpdates?limit=50")
	if err != nil {
		http.Error(w, "Telegram nicht erreichbar", 502)
		return
	}
	defer resp.Body.Close()
	var res struct {
		OK     bool `json:"ok"`
		Result []struct {
			Message      *tgMsg `json:"message"`
			ChannelPost  *tgMsg `json:"channel_post"`
			MyChatMember *tgMsg `json:"my_chat_member"`
		} `json:"result"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if !res.OK {
		http.Error(w, "Telegram: "+res.Description, 400)
		return
	}
	type chat struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	seen := map[int64]bool{}
	out := []chat{}
	for _, u := range res.Result {
		for _, msg := range []*tgMsg{u.Message, u.ChannelPost, u.MyChatMember} {
			if msg == nil || seen[msg.Chat.ID] {
				continue
			}
			seen[msg.Chat.ID] = true
			name := msg.Chat.Title
			if name == "" {
				name = strings.TrimSpace(msg.Chat.FirstName + " " + msg.Chat.LastName)
			}
			out = append(out, chat{strconv.FormatInt(msg.Chat.ID, 10), name, msg.Chat.Type})
		}
	}
	writeJSON(w, out)
}

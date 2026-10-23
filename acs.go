package main

// TR-069 / CWMP: schlanker Auto-Configuration-Server (ACS) für FRITZ!Boxen – Prototyp.
//
// Ablauf einer Sitzung (immer von der Box aus):
//  1. Box → POST Inform (Seriennummer, Ereignisse, einige Parameter)   ACS → InformResponse
//  2. Box → POST leer                                                   ACS → nächster Auftrag (RPC) oder 204 = Ende
//  3. Box → POST Antwort auf den Auftrag                                ACS → nächster Auftrag oder 204
//
// Eingeschaltet nur mit ACS_ENABLE=1. Endpunkt: /cwmp (HTTPS über die rstcloud).
// Optional ACS_CPE_USER / ACS_CPE_PASS: Boxen müssen sich mit diesen Zugangsdaten (HTTP Basic) anmelden.
// Bedienung nur für den Betreiber (Einstellungen → TR-069).

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func acsEnabled() bool { return os.Getenv("ACS_ENABLE") == "1" }

// ---------- Datenmodell ----------

type AcsParam struct {
	Name  string `json:"n"`
	Value string `json:"v"`
	Type  string `json:"t,omitempty"` // xsd:string, xsd:unsignedInt, xsd:boolean …
}

type AcsTask struct {
	ID      string     `json:"id"`
	Kind    string     `json:"kind"` // get, names, set, reboot, factoryreset, download
	Names   []string   `json:"names,omitempty"`
	Values  []AcsParam `json:"values,omitempty"`
	URL     string     `json:"url,omitempty"`      // download: Datei-URL
	File    string     `json:"fileType,omitempty"` // download: "1 Firmware Upgrade Image" …
	Created int64      `json:"created"`
	Done    int64      `json:"done,omitempty"`
	State   string     `json:"state"` // queued, sent, done, fault
	Result  string     `json:"result,omitempty"`
	By      string     `json:"by,omitempty"`
	Secret  bool       `json:"secret,omitempty"` // Werte nicht anzeigen (Passwörter)
}

// AcsProfile: Soll-Einstellungen einer Box (werden beim ersten Kontakt und nach jedem Reset gesetzt).
type AcsProfile struct {
	PPPUser  string     `json:"pppUser,omitempty"`
	PPPPass  string     `json:"pppPass,omitempty"` // nie an den Browser
	Extra    []AcsParam `json:"extra,omitempty"`
	Interval int        `json:"interval,omitempty"` // PeriodicInformInterval in s (Standard 3600)
	Voip     []AcsVoip  `json:"voip,omitempty"`
}

type AcsDevice struct {
	ID       string            `json:"id"` // OUI-ProductClass-Serial
	OUI      string            `json:"oui"`
	Class    string            `json:"class"`
	Serial   string            `json:"serial"`
	Maker    string            `json:"maker,omitempty"`
	Model    string            `json:"model,omitempty"`
	SW       string            `json:"sw,omitempty"`
	HW       string            `json:"hw,omitempty"`
	ProvCode string            `json:"provCode,omitempty"`
	Root     string            `json:"root,omitempty"` // "InternetGatewayDevice." oder "Device."
	IP       string            `json:"ip,omitempty"`   // Absender der letzten Sitzung
	ExtIP    string            `json:"extIP,omitempty"`
	First    int64             `json:"first"`
	Last     int64             `json:"last"`
	Events   []string          `json:"events,omitempty"` // Ereignisse der letzten Sitzung
	Params   map[string]string `json:"params,omitempty"` // zuletzt gelesene Werte
	Tasks    []AcsTask         `json:"tasks,omitempty"`  // neueste zuletzt, höchstens 100
	Profile  *AcsProfile       `json:"profile,omitempty"`
	Applied  int64             `json:"applied,omitempty"` // Profil zuletzt gesetzt
	Name     string            `json:"name,omitempty"`    // Anzeigename (Kunde)
	Note     string            `json:"note,omitempty"`
	Log      []string          `json:"log,omitempty"` // Sitzungsprotokoll, neueste zuletzt, höchstens 200
}

type acsSession struct {
	dev     string
	ip      string
	ns      string // CWMP-Namensraum der Box
	last    time.Time
	current string // ID des gesendeten Auftrags
}

var acsState = struct {
	sync.Mutex
	sess      map[string]*acsSession // Cookie → Sitzung
	lastSave  time.Time
	dirty     bool
	idCounter int64
}{sess: map[string]*acsSession{}}

// ---------- SOAP ----------

type soapIn struct {
	ID       string
	Method   string // erstes Element im Body, z. B. Inform, GetParameterValuesResponse, Fault
	NS       string
	Body     []byte // innerer Inhalt des Body
	FaultMsg string
}

func parseCWMP(b []byte) (*soapIn, error) {
	out := &soapIn{}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	depth, inHeader, inBody := 0, false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case t.Name.Local == "Header" && depth == 2:
				inHeader = true
			case t.Name.Local == "Body" && depth == 2:
				inBody = true
			case inHeader && t.Name.Local == "ID":
				var s string
				dec.DecodeElement(&s, &t)
				out.ID = strings.TrimSpace(s)
				depth--
			case inBody && depth == 3 && out.Method == "":
				out.Method = t.Name.Local
				out.NS = t.Name.Space
				var raw struct {
					Inner []byte `xml:",innerxml"`
				}
				dec.DecodeElement(&raw, &t)
				out.Body = raw.Inner
				depth--
			}
		case xml.EndElement:
			if t.Name.Local == "Header" {
				inHeader = false
			}
			if t.Name.Local == "Body" {
				inBody = false
			}
			depth--
		}
	}
	if out.Method == "Fault" {
		// CWMP-Fehler steckt in detail/cwmp:Fault (FaultCode, FaultString)
		out.FaultMsg = lastOf(xmlTexts(out.Body, "FaultString")) + " (" + lastOf(xmlTexts(out.Body, "FaultCode")) + ")"
		if strings.HasPrefix(out.FaultMsg, " (") {
			out.FaultMsg = xmlText(out.Body, "faultstring")
		}
	}
	return out, nil
}

func lastOf(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[len(l)-1]
}

// xmlTexts: Text aller Elemente mit diesem lokalen Namen.
func xmlTexts(b []byte, name string) []string {
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(append(append([]byte("<x>"), b...), "</x>"...)))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		if s, ok := tok.(xml.StartElement); ok && s.Name.Local == name {
			var v string
			if dec.DecodeElement(&v, &s) == nil {
				out = append(out, strings.TrimSpace(v))
			}
		}
	}
}

func xmlText(b []byte, name string) string {
	if l := xmlTexts(b, name); len(l) > 0 {
		return l[0]
	}
	return ""
}

// paramList liest ParameterValueStruct- bzw. ParameterInfoStruct-Listen.
func paramList(b []byte) []AcsParam {
	var out []AcsParam
	dec := xml.NewDecoder(bytes.NewReader(append(append([]byte("<x>"), b...), "</x>"...)))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		s, ok := tok.(xml.StartElement)
		if !ok || (s.Name.Local != "ParameterValueStruct" && s.Name.Local != "ParameterInfoStruct") {
			continue
		}
		var v struct {
			Name     string `xml:"Name"`
			Writable string `xml:"Writable"`
			Value    struct {
				Type string `xml:"type,attr"`
				Text string `xml:",chardata"`
			} `xml:"Value"`
		}
		if dec.DecodeElement(&v, &s) == nil {
			p := AcsParam{Name: strings.TrimSpace(v.Name), Value: v.Value.Text, Type: v.Value.Type}
			if s.Name.Local == "ParameterInfoStruct" {
				p.Value, p.Type = "", "writable="+v.Writable
			}
			out = append(out, p)
		}
	}
}

const soapHead = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soapenc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="%s">
<soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">%s</cwmp:ID></soapenv:Header>
<soapenv:Body>%s</soapenv:Body>
</soapenv:Envelope>`

func xe(s string) string { return html.EscapeString(s) }

func soapOut(ns, id, body string) []byte {
	if ns == "" {
		ns = "urn:dslforum-org:cwmp-1-0"
	}
	return []byte(fmt.Sprintf(soapHead, ns, xe(id), body))
}

// rpcBody: SOAP-Body für einen Auftrag.
func rpcBody(t AcsTask) string {
	switch t.Kind {
	case "get":
		var b strings.Builder
		for _, n := range t.Names {
			b.WriteString("<string>" + xe(n) + "</string>")
		}
		return fmt.Sprintf(`<cwmp:GetParameterValues><ParameterNames soapenc:arrayType="xsd:string[%d]">%s</ParameterNames></cwmp:GetParameterValues>`, len(t.Names), b.String())
	case "names":
		p := ""
		if len(t.Names) > 0 {
			p = t.Names[0]
		}
		return `<cwmp:GetParameterNames><ParameterPath>` + xe(p) + `</ParameterPath><NextLevel>true</NextLevel></cwmp:GetParameterNames>`
	case "set":
		var b strings.Builder
		for _, v := range t.Values {
			typ := v.Type
			if typ == "" {
				typ = "xsd:string"
			}
			b.WriteString(`<ParameterValueStruct><Name>` + xe(v.Name) + `</Name><Value xsi:type="` + xe(typ) + `">` + xe(v.Value) + `</Value></ParameterValueStruct>`)
		}
		return fmt.Sprintf(`<cwmp:SetParameterValues><ParameterList soapenc:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList><ParameterKey>%s</ParameterKey></cwmp:SetParameterValues>`, len(t.Values), b.String(), xe(t.ID))
	case "reboot":
		return `<cwmp:Reboot><CommandKey>` + xe(t.ID) + `</CommandKey></cwmp:Reboot>`
	case "factoryreset":
		return `<cwmp:FactoryReset/>`
	case "download":
		ft := t.File
		if ft == "" {
			ft = "1 Firmware Upgrade Image"
		}
		return `<cwmp:Download><CommandKey>` + xe(t.ID) + `</CommandKey><FileType>` + xe(ft) + `</FileType><URL>` + xe(t.URL) + `</URL><Username></Username><Password></Password><FileSize>0</FileSize><TargetFileName></TargetFileName><DelaySeconds>0</DelaySeconds><SuccessURL></SuccessURL><FailureURL></FailureURL></cwmp:Download>`
	}
	return ""
}

// ---------- Hilfen für Geräte ----------

func (m *Monitor) acsDevice(id string) *AcsDevice {
	for i := range m.cfg.Acs {
		if m.cfg.Acs[i].ID == id {
			return &m.cfg.Acs[i]
		}
	}
	return nil
}

func (d *AcsDevice) logf(format string, a ...any) {
	d.Log = append(d.Log, time.Now().Format("02.01. 15:04:05")+" "+fmt.Sprintf(format, a...))
	if len(d.Log) > 200 {
		d.Log = d.Log[len(d.Log)-200:]
	}
}

func acsNewID() string {
	acsState.Lock()
	acsState.idCounter++
	n := acsState.idCounter
	acsState.Unlock()
	return fmt.Sprintf("t%d%03d", time.Now().Unix(), n%1000)
}

// root: Datenmodell der Box – TR-098 ("InternetGatewayDevice.") oder TR-181 ("Device.").
func (d *AcsDevice) root() string {
	if d.Root != "" {
		return d.Root
	}
	return "InternetGatewayDevice."
}

// wanPPPPrefix: Pfad der PPP-Verbindung aus den bekannten Parametern (Glasfaser-Boxen nutzen teils andere Indizes).
func (d *AcsDevice) wanPPPPrefix() string {
	best := ""
	for k := range d.Params {
		isPPP := (strings.Contains(k, ".WANPPPConnection.") || strings.HasPrefix(k, "Device.PPP.Interface.")) && strings.HasSuffix(k, ".Username")
		if isPPP {
			p := k[:len(k)-len("Username")]
			if best == "" || p < best {
				best = p
			}
		}
	}
	if best == "" {
		if d.root() == "Device." {
			best = "Device.PPP.Interface.1."
		} else {
			best = "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.1."
		}
	}
	return best
}

// AcsVoip: eine Rufnummer (SIP-Konto) für die Box (TR-104, VoiceProfile 1).
type AcsVoip struct {
	Number    string `json:"number"`
	User      string `json:"user,omitempty"`
	Pass      string `json:"pass,omitempty"` // nie an den Browser
	Registrar string `json:"registrar,omitempty"`
	Proxy     string `json:"proxy,omitempty"`
	Domain    string `json:"domain,omitempty"`
}

// AcsCodeProfile: Profil für alle Boxen mit diesem Provisioning-Code (kommt z. B. vom RADIUS über
// TR069PROVC=… in der PPP-Anmeldung). Code "*" = Grundeinstellungen für alle Boxen (nur weitere Werte).
type AcsCodeProfile struct {
	Code    string     `json:"code"`
	Name    string     `json:"name,omitempty"`
	Profile AcsProfile `json:"profile"`
}

// profileValues: alle Soll-Werte eines Profils für diese Box.
func (d *AcsDevice) profileValues(p *AcsProfile, global []AcsParam) []AcsParam {
	r := d.root()
	var vals []AcsParam
	if p != nil && p.PPPUser != "" {
		pre := d.wanPPPPrefix()
		vals = append(vals, AcsParam{Name: pre + "Username", Value: p.PPPUser}, AcsParam{Name: pre + "Password", Value: p.PPPPass})
	}
	iv := 3600
	if p != nil && p.Interval > 0 {
		iv = p.Interval
	}
	vals = append(vals, AcsParam{Name: r + "ManagementServer.PeriodicInformEnable", Value: "1", Type: "xsd:boolean"},
		AcsParam{Name: r + "ManagementServer.PeriodicInformInterval", Value: fmt.Sprint(iv), Type: "xsd:unsignedInt"})
	if p != nil && len(p.Voip) > 0 && r == "InternetGatewayDevice." {
		vp := r + "Services.VoiceService.1.VoiceProfile.1."
		v0 := p.Voip[0]
		if v0.Registrar != "" {
			vals = append(vals, AcsParam{Name: vp + "SIP.RegistrarServer", Value: v0.Registrar})
		}
		if v0.Proxy != "" {
			vals = append(vals, AcsParam{Name: vp + "SIP.ProxyServer", Value: v0.Proxy})
		}
		if v0.Domain != "" {
			vals = append(vals, AcsParam{Name: vp + "SIP.UserAgentDomain", Value: v0.Domain})
		}
		for i, v := range p.Voip {
			lp := fmt.Sprintf("%sLine.%d.", vp, i+1)
			vals = append(vals, AcsParam{Name: lp + "Enable", Value: "Enabled"}, AcsParam{Name: lp + "DirectoryNumber", Value: v.Number},
				AcsParam{Name: lp + "SIP.AuthUserName", Value: v.User}, AcsParam{Name: lp + "SIP.AuthPassword", Value: v.Pass},
				AcsParam{Name: lp + "SIP.URI", Value: v.Number})
		}
	}
	vals = append(vals, global...)
	if p != nil {
		vals = append(vals, p.Extra...)
	}
	return vals
}

// profileTasks: Aufträge, die das Profil auf die Box bringen.
func (d *AcsDevice) profileTasks(p *AcsProfile, global []AcsParam) []AcsTask {
	if p == nil && len(global) == 0 {
		return nil
	}
	vals := d.profileValues(p, global)
	secret := p != nil && (p.PPPPass != "" || len(p.Voip) > 0)
	return []AcsTask{{ID: acsNewID(), Kind: "set", Values: vals, Created: time.Now().UnixMilli(), State: "queued", By: "Profil", Secret: secret}}
}

// effectiveProfile: eigenes Profil der Box, sonst das Profil zu ihrem Provisioning-Code; dazu die Grundeinstellungen ("*").
func (m *Monitor) effectiveProfile(d *AcsDevice) (*AcsProfile, []AcsParam, string) {
	var global []AcsParam
	var p *AcsProfile
	src := ""
	for i := range m.cfg.AcsProfiles {
		cp := &m.cfg.AcsProfiles[i]
		if cp.Code == "*" {
			global = cp.Profile.Extra
		} else if d.ProvCode != "" && strings.EqualFold(cp.Code, d.ProvCode) {
			p, src = &cp.Profile, "Code "+cp.Code
		}
	}
	if d.Profile != nil {
		p, src = d.Profile, "eigenes Profil"
	}
	return p, global, src
}

// standardabfrage nach dem Inform: Grunddaten der Box.
func (d *AcsDevice) basicParams() []string {
	if d.root() == "Device." {
		return []string{"Device.DeviceInfo.", "Device.ManagementServer.", "Device.PPP.Interface."}
	}
	return []string{"InternetGatewayDevice.DeviceInfo.", "InternetGatewayDevice.ManagementServer.", "InternetGatewayDevice.WANDevice."}
}

// ---------- HTTP ----------

func (s *Server) handleCWMP(w http.ResponseWriter, r *http.Request) {
	if !acsEnabled() {
		http.NotFound(w, r)
		return
	}
	if u := os.Getenv("ACS_CPE_USER"); u != "" {
		bu, bp, ok := r.BasicAuth()
		if !ok || bu != u || bp != os.Getenv("ACS_CPE_PASS") {
			w.Header().Set("WWW-Authenticate", `Basic realm="PingWacht ACS"`)
			http.Error(w, "unauthorized", 401)
			return
		}
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	m := s.m
	// Sitzung über Cookie
	var sess *acsSession
	var sid string
	if c, err := r.Cookie("acs_sid"); err == nil {
		sid = c.Value
	}
	acsState.Lock()
	for k, v := range acsState.sess {
		if time.Since(v.last) > 10*time.Minute {
			delete(acsState.sess, k)
		}
	}
	if sid != "" {
		sess = acsState.sess[sid]
	}
	ip := clientIP(r)
	if sess == nil {
		// Box ohne Cookie-Unterstützung: jüngste Sitzung derselben Adresse
		for _, v := range acsState.sess {
			if v.ip == ip && time.Since(v.last) < 2*time.Minute && (sess == nil || v.last.After(sess.last)) {
				sess = v
			}
		}
	}
	acsState.Unlock()

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	if len(bytes.TrimSpace(body)) == 0 {
		if sess == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.acsNext(w, sess)
		return
	}
	in, err := parseCWMP(body)
	if err != nil || in.Method == "" {
		http.Error(w, "ungültige SOAP-Nachricht", 400)
		return
	}
	if in.Method == "Inform" {
		sid = randHex(16)
		sess = &acsSession{ns: in.NS, last: time.Now(), ip: ip}
		dev := m.acsInform(in, ip)
		sess.dev = dev
		acsState.Lock()
		acsState.sess[sid] = sess
		acsState.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "acs_sid", Value: sid, Path: "/cwmp", HttpOnly: true})
		w.Write(soapOut(in.NS, in.ID, `<cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>`))
		return
	}
	if sess == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sess.last = time.Now()
	switch in.Method {
	case "TransferComplete":
		m.acsResult(sess, "", "TransferComplete", in)
		w.Write(soapOut(sess.ns, in.ID, `<cwmp:TransferCompleteResponse/>`))
		return
	case "GetRPCMethods":
		w.Write(soapOut(sess.ns, in.ID, `<cwmp:GetRPCMethodsResponse><MethodList soapenc:arrayType="xsd:string[3]"><string>Inform</string><string>GetRPCMethods</string><string>TransferComplete</string></MethodList></cwmp:GetRPCMethodsResponse>`))
		return
	}
	// Antwort auf unseren Auftrag
	m.acsResult(sess, sess.current, in.Method, in)
	s.acsNext(w, sess)
}

// acsNext schickt den nächsten Auftrag oder beendet die Sitzung.
func (s *Server) acsNext(w http.ResponseWriter, sess *acsSession) {
	m := s.m
	m.mu.Lock()
	d := m.acsDevice(sess.dev)
	var task *AcsTask
	if d != nil {
		for i := range d.Tasks {
			if d.Tasks[i].State == "queued" {
				task = &d.Tasks[i]
				break
			}
		}
	}
	var out []byte
	if task != nil {
		task.State = "sent"
		sess.current = task.ID
		out = soapOut(sess.ns, task.ID, rpcBody(*task))
		d.logf("→ %s %s", task.Kind, acsTaskSummary(*task))
	}
	m.mu.Unlock()
	if out == nil {
		sess.current = ""
		w.WriteHeader(http.StatusNoContent)
		m.acsMaybeSave(false)
		return
	}
	w.Write(out)
}

func acsTaskSummary(t AcsTask) string {
	switch t.Kind {
	case "get", "names":
		return strings.Join(t.Names, ", ")
	case "set":
		var l []string
		for _, v := range t.Values {
			val := v.Value
			if t.Secret || strings.Contains(strings.ToLower(v.Name), "password") {
				val = "••••"
			}
			l = append(l, v.Name+"="+val)
		}
		return strings.Join(l, ", ")
	case "download":
		return t.URL
	}
	return ""
}

// acsInform: Gerät anlegen/aktualisieren, Standardaufträge einplanen. Rückgabe: Geräte-ID.
func (m *Monitor) acsInform(in *soapIn, ip string) string {
	oui, class, serial := xmlText(in.Body, "OUI"), xmlText(in.Body, "ProductClass"), xmlText(in.Body, "SerialNumber")
	id := oui + "-" + class + "-" + serial
	events := xmlTexts(in.Body, "EventCode")
	params := paramList(in.Body)
	now := time.Now().UnixMilli()
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.acsDevice(id)
	isNew := d == nil
	if isNew {
		m.cfg.Acs = append(m.cfg.Acs, AcsDevice{ID: id, OUI: oui, Class: class, Serial: serial, First: now, Params: map[string]string{}})
		d = &m.cfg.Acs[len(m.cfg.Acs)-1]
	}
	if d.Params == nil {
		d.Params = map[string]string{}
	}
	d.Maker = xmlText(in.Body, "Manufacturer")
	d.Last, d.IP, d.Events = now, ip, events
	for _, p := range params {
		d.Params[p.Name] = p.Value
		if d.Root == "" {
			if strings.HasPrefix(p.Name, "Device.") {
				d.Root = "Device."
			} else if strings.HasPrefix(p.Name, "InternetGatewayDevice.") {
				d.Root = "InternetGatewayDevice."
			}
		}
		switch {
		case strings.HasSuffix(p.Name, "DeviceInfo.SoftwareVersion"):
			d.SW = p.Value
		case strings.HasSuffix(p.Name, "DeviceInfo.HardwareVersion"):
			d.HW = p.Value
		case strings.HasSuffix(p.Name, "DeviceInfo.ProvisioningCode"):
			d.ProvCode = p.Value
		case strings.HasSuffix(p.Name, "DeviceInfo.ModelName") || strings.HasSuffix(p.Name, "DeviceInfo.Description"):
			if d.Model == "" || strings.HasSuffix(p.Name, "ModelName") {
				d.Model = p.Value
			}
		case strings.HasSuffix(p.Name, ".ExternalIPAddress") && p.Value != "":
			d.ExtIP = p.Value
		}
	}
	d.logf("← Inform %s (%s)", strings.Join(events, ", "), ip)
	boot := false
	for _, e := range events {
		if strings.HasPrefix(e, "0 ") || strings.HasPrefix(e, "1 ") { // BOOTSTRAP, BOOT
			boot = true
		}
	}
	pending := 0
	for _, t := range d.Tasks {
		if t.State == "queued" {
			pending++
		}
	}
	// Grunddaten beim ersten Kontakt und nach jedem Neustart lesen
	if (isNew || boot) && pending < 20 {
		d.Tasks = append(d.Tasks, AcsTask{ID: acsNewID(), Kind: "get", Names: d.basicParams(), Created: now, State: "queued", By: "automatisch"})
	}
	// Profil: nach BOOTSTRAP (Werkseinstellungen) oder wenn noch nie gesetzt
	bootstrap := false
	for _, e := range events {
		if strings.HasPrefix(e, "0 ") {
			bootstrap = true
		}
	}
	if prof, global, src := m.effectiveProfile(d); (prof != nil || len(global) > 0) && (bootstrap || d.Applied == 0) {
		d.Tasks = append(d.Tasks, d.profileTasks(prof, global)...)
		d.Applied = now
		d.logf("Profil eingeplant (%s, %s)", map[bool]string{true: "Werkseinstellungen", false: "erster Kontakt"}[bootstrap], map[bool]string{true: src, false: "Grundeinstellungen"}[src != ""])
	}
	if len(d.Tasks) > 100 {
		d.Tasks = d.Tasks[len(d.Tasks)-100:]
	}
	acsState.Lock()
	acsState.dirty = true
	acsState.Unlock()
	if isNew {
		log.Printf("ACS: neues Gerät %s (%s)", id, ip)
		m.saveConfigLocked()
	}
	return id
}

// acsResult: Antwort der Box auf einen Auftrag verarbeiten.
func (m *Monitor) acsResult(sess *acsSession, taskID, method string, in *soapIn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.acsDevice(sess.dev)
	if d == nil {
		return
	}
	var t *AcsTask
	for i := range d.Tasks {
		if d.Tasks[i].ID == taskID || (taskID == "" && method == "TransferComplete" && d.Tasks[i].Kind == "download" && d.Tasks[i].State == "sent") {
			t = &d.Tasks[i]
		}
	}
	now := time.Now().UnixMilli()
	res := ""
	switch method {
	case "GetParameterValuesResponse":
		ps := paramList(in.Body)
		for _, p := range ps {
			d.Params[p.Name] = p.Value
		}
		res = fmt.Sprintf("%d Werte gelesen", len(ps))
	case "GetParameterNamesResponse":
		ps := paramList(in.Body)
		var l []string
		for _, p := range ps {
			l = append(l, p.Name+" ("+strings.TrimPrefix(p.Type, "writable=")+")")
		}
		sort.Strings(l)
		res = strings.Join(l, "\n")
	case "SetParameterValuesResponse":
		res = "gesetzt (Status " + xmlText(in.Body, "Status") + ")"
		if t != nil {
			for _, v := range t.Values {
				if !strings.Contains(strings.ToLower(v.Name), "password") {
					d.Params[v.Name] = v.Value
				}
			}
		}
	case "RebootResponse":
		res = "Neustart ausgelöst"
	case "FactoryResetResponse":
		res = "Werkseinstellungen ausgelöst"
	case "DownloadResponse":
		res = "Download angenommen (Status " + xmlText(in.Body, "Status") + ")"
		if t != nil && xmlText(in.Body, "Status") == "1" {
			t.Result = res
			d.logf("← %s", res)
			return // erst mit TransferComplete fertig
		}
	case "TransferComplete":
		code := xmlText(in.Body, "FaultCode")
		res = "Übertragung abgeschlossen"
		if code != "" && code != "0" {
			res = "Übertragung fehlgeschlagen: " + xmlText(in.Body, "FaultString") + " (" + code + ")"
			if t != nil {
				t.State, t.Done, t.Result = "fault", now, res
			}
			d.logf("← %s", res)
			return
		}
	case "Fault":
		res = "Fehler: " + in.FaultMsg
		if t != nil {
			t.State, t.Done, t.Result = "fault", now, res
		}
		d.logf("← %s", res)
		acsState.Lock()
		acsState.dirty = true
		acsState.Unlock()
		return
	default:
		res = "Antwort " + method
	}
	if t != nil {
		t.State, t.Done, t.Result = "done", now, res
	}
	short := res
	if i := strings.IndexByte(short, '\n'); i > 0 {
		short = short[:i] + " …"
	}
	d.logf("← %s: %s", method, short)
	acsState.Lock()
	acsState.dirty = true
	acsState.Unlock()
}

// acsMaybeSave: geänderte Gerätedaten höchstens alle 2 Minuten speichern (oder sofort).
func (m *Monitor) acsMaybeSave(force bool) {
	acsState.Lock()
	do := acsState.dirty && (force || time.Since(acsState.lastSave) > 2*time.Minute)
	if do {
		acsState.dirty = false
		acsState.lastSave = time.Now()
	}
	acsState.Unlock()
	if do {
		m.mu.Lock()
		m.saveConfigLocked()
		m.mu.Unlock()
	}
}

// ---------- Betreiber-API ----------

// maskProfile: Profil ohne Passwörter (für den Browser).
func maskProfile(p AcsProfile) AcsProfile {
	if p.PPPPass != "" {
		p.PPPPass = "••••"
	}
	vs := make([]AcsVoip, len(p.Voip))
	for i, v := range p.Voip {
		if v.Pass != "" {
			v.Pass = "••••"
		}
		vs[i] = v
	}
	p.Voip = vs
	return p
}

// keepSecrets: leere oder maskierte Passwörter aus dem alten Profil übernehmen.
func keepSecrets(np *AcsProfile, old *AcsProfile) {
	if old == nil {
		return
	}
	if np.PPPPass == "" || np.PPPPass == "••••" {
		np.PPPPass = old.PPPPass
	}
	for i := range np.Voip {
		if np.Voip[i].Pass == "" || np.Voip[i].Pass == "••••" {
			for _, o := range old.Voip {
				if o.Number == np.Voip[i].Number {
					np.Voip[i].Pass = o.Pass
				}
			}
		}
	}
}

// acsView: Gerät ohne Passwörter.
func acsView(d AcsDevice) AcsDevice {
	v := d
	if d.Profile != nil {
		p := maskProfile(*d.Profile)
		v.Profile = &p
	}
	v.Tasks = make([]AcsTask, len(d.Tasks))
	for i, t := range d.Tasks {
		if t.Secret || t.Kind == "set" {
			vals := make([]AcsParam, len(t.Values))
			for j, x := range t.Values {
				if strings.Contains(strings.ToLower(x.Name), "password") {
					x.Value = "••••"
				}
				vals[j] = x
			}
			t.Values = vals
		}
		v.Tasks[i] = t
	}
	p2 := map[string]string{}
	for k, val := range d.Params {
		if strings.Contains(strings.ToLower(k), "password") || strings.Contains(strings.ToLower(k), "passphrase") || strings.Contains(strings.ToLower(k), "presharedkey") || strings.HasSuffix(k, ".KeyPassphrase") {
			if val != "" {
				val = "••••"
			}
		}
		p2[k] = val
	}
	v.Params = p2
	return v
}

// GET /api/acs → Geräte · POST /api/acs {action, id, …}
func (s *Server) handleACS(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p == nil || !p.Super {
		http.Error(w, "Nur für den Betreiber", 403)
		return
	}
	m := s.m
	if r.Method == http.MethodGet {
		m.mu.RLock()
		out := make([]AcsDevice, 0, len(m.cfg.Acs))
		for _, d := range m.cfg.Acs {
			out = append(out, acsView(d))
		}
		m.mu.RUnlock()
		profs := make([]AcsCodeProfile, 0, len(m.cfg.AcsProfiles))
		for _, cp := range m.cfg.AcsProfiles {
			cp.Profile = maskProfile(cp.Profile)
			profs = append(profs, cp)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
		writeJSON(w, map[string]any{"enabled": acsEnabled(), "auth": os.Getenv("ACS_CPE_USER") != "", "url": publicBase(r) + "/cwmp", "devices": out, "profiles": profs})
		return
	}
	var req struct {
		Action  string     `json:"action"` // task, profile, name, delete, cancel, codeprofile, codedelete
		Code    string     `json:"code"`
		ID      string     `json:"id"`
		Task    AcsTask    `json:"task"`
		Profile AcsProfile `json:"profile"`
		Name    string     `json:"name"`
		Note    string     `json:"note"`
		TaskID  string     `json:"taskId"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Action == "codeprofile" || req.Action == "codedelete" {
		code := strings.TrimSpace(req.Code)
		if code == "" || len(code) > 64 {
			http.Error(w, "Provisioning-Code fehlt (höchstens 64 Zeichen)", 400)
			return
		}
		idx := -1
		for i := range m.cfg.AcsProfiles {
			if strings.EqualFold(m.cfg.AcsProfiles[i].Code, code) {
				idx = i
			}
		}
		if req.Action == "codedelete" {
			if idx >= 0 {
				m.cfg.AcsProfiles = append(m.cfg.AcsProfiles[:idx], m.cfg.AcsProfiles[idx+1:]...)
			}
		} else {
			np := req.Profile
			if idx >= 0 {
				keepSecrets(&np, &m.cfg.AcsProfiles[idx].Profile)
				m.cfg.AcsProfiles[idx] = AcsCodeProfile{Code: code, Name: strings.TrimSpace(req.Name), Profile: np}
			} else {
				m.cfg.AcsProfiles = append(m.cfg.AcsProfiles, AcsCodeProfile{Code: code, Name: strings.TrimSpace(req.Name), Profile: np})
			}
		}
		m.saveConfigLocked()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	d := m.acsDevice(req.ID)
	if d == nil {
		http.Error(w, "Gerät unbekannt", 404)
		return
	}
	who := "Betreiber"
	if p.User != nil {
		who = p.User.Email
	}
	now := time.Now().UnixMilli()
	switch req.Action {
	case "task":
		t := req.Task
		switch t.Kind {
		case "get", "names":
			if len(t.Names) == 0 {
				http.Error(w, "Parameter fehlt", 400)
				return
			}
		case "set":
			if len(t.Values) == 0 {
				http.Error(w, "Werte fehlen", 400)
				return
			}
		case "reboot", "factoryreset":
		case "download":
			if !strings.HasPrefix(t.URL, "http") {
				http.Error(w, "Download-URL fehlt", 400)
				return
			}
		default:
			http.Error(w, "unbekannter Auftrag", 400)
			return
		}
		t.ID, t.Created, t.State, t.By, t.Done, t.Result = acsNewID(), now, "queued", who, 0, ""
		d.Tasks = append(d.Tasks, t)
		if len(d.Tasks) > 100 {
			d.Tasks = d.Tasks[len(d.Tasks)-100:]
		}
		d.logf("Auftrag %s von %s eingeplant", t.Kind, who)
	case "cancel":
		for i := range d.Tasks {
			if d.Tasks[i].ID == req.TaskID && (d.Tasks[i].State == "queued" || d.Tasks[i].State == "sent") {
				d.Tasks[i].State, d.Tasks[i].Result, d.Tasks[i].Done = "fault", "abgebrochen", now
			}
		}
	case "profile":
		np := req.Profile
		keepSecrets(&np, d.Profile) // leer = unverändert
		if np.PPPUser == "" && len(np.Extra) == 0 && np.Interval == 0 && len(np.Voip) == 0 {
			d.Profile = nil
		} else {
			d.Profile = &np
			_, global, _ := m.effectiveProfile(d)
			d.Tasks = append(d.Tasks, d.profileTasks(d.Profile, global)...)
			d.Applied = now
		}
		d.logf("Profil geändert von %s", who)
	case "name":
		d.Name, d.Note = strings.TrimSpace(req.Name), strings.TrimSpace(req.Note)
	case "delete":
		for i := range m.cfg.Acs {
			if m.cfg.Acs[i].ID == req.ID {
				m.cfg.Acs = append(m.cfg.Acs[:i], m.cfg.Acs[i+1:]...)
				break
			}
		}
	default:
		http.Error(w, "unbekannte Aktion", 400)
		return
	}
	m.saveConfigLocked()
	writeJSON(w, map[string]bool{"ok": true})
}

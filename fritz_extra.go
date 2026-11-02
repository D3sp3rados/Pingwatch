package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Erweiterte Fritzbox-Auslesung (alle 5 Minuten): Geräte im Heimnetz, WLAN, Datenverkehr,
// Firmware-Update, Ereignisprotokoll, Rufnummern/Telefonie-Anmeldung und Smart-Home-Geräte.
// Jede Abfrage ist für sich optional – was die Box (oder der Benutzer) nicht kann, fehlt einfach.
// Anrufliste und Rufumleitungen: Rufnummern werden im Browser gekürzt, außer „vollständig anzeigen“ ist gesetzt.

type fritzDev struct {
	Name   string `json:"name,omitempty"`
	IP     string `json:"ip,omitempty"`
	MAC    string `json:"mac,omitempty"`
	Iface  string `json:"iface,omitempty"` // Ethernet, 802.11 …
	Active bool   `json:"active"`
	Speed  int    `json:"speed,omitempty"` // Mbit/s
	Guest  bool   `json:"guest,omitempty"`
}

type fritzWLAN struct {
	Idx      int    `json:"idx"`
	SSID     string `json:"ssid"`
	On       bool   `json:"on"`
	Channel  int    `json:"channel,omitempty"`
	Standard string `json:"standard,omitempty"`
	Band     string `json:"band,omitempty"`
	Clients  int    `json:"clients"`
}

type fritzVoIP struct {
	Number    string `json:"number"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"` // Registered / Not registered …
	Registrar string `json:"registrar,omitempty"`
	User      string `json:"user,omitempty"` // SIP-Benutzername (kein Passwort)
}

type fritzCall struct {
	Type   string `json:"type"` // in, out, missed, rejected, active
	Date   string `json:"date"`
	Other  string `json:"other,omitempty"` // Gegenstelle
	Name   string `json:"name,omitempty"`
	Own    string `json:"own,omitempty"` // eigene Rufnummer
	Device string `json:"device,omitempty"`
	Dur    string `json:"dur,omitempty"`
}

type fritzDefl struct {
	On     bool   `json:"on"`
	Number string `json:"number,omitempty"` // für welche Rufnummer (leer = alle)
	To     string `json:"to,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

type fritzDect struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Model  string `json:"model,omitempty"`
	Active bool   `json:"active"`
	Update bool   `json:"update,omitempty"` // Firmware-Update für das Mobilteil verfügbar
	UpdOK  string `json:"updOk,omitempty"`  // Ergebnis des letzten Updates
	Info   string `json:"info,omitempty"`   // Update-Info (Version)
}

type fritzPhone struct { // IP-Telefon (SIP) an der Box
	Name     string `json:"name"`
	User     string `json:"user,omitempty"`
	Internal string `json:"internal,omitempty"` // interne Nummer, z. B. **620
	Out      string `json:"out,omitempty"`      // ausgehende Rufnummer
	External bool   `json:"external,omitempty"` // Anmeldung aus dem Internet erlaubt
}

type fritzTAM struct {
	Name  string `json:"name"`
	On    bool   `json:"on"`
	New   int    `json:"new"`
	Total int    `json:"total"`
}

type fritzSmart struct {
	Name    string  `json:"name"`
	Product string  `json:"product,omitempty"`
	Present bool    `json:"present"`
	TempC   float64 `json:"temp,omitempty"`
	Target  float64 `json:"target,omitempty"`
	Switch  string  `json:"switch,omitempty"` // ON / OFF
	PowerW  float64 `json:"power,omitempty"`
	EnergyW int64   `json:"energy,omitempty"` // Wh
}

type fritzExtra struct {
	At         int64        `json:"at"`
	Devices    []fritzDev   `json:"devices,omitempty"`
	WLAN       []fritzWLAN  `json:"wlan,omitempty"`
	BytesSent  int64        `json:"bytesSent,omitempty"` // seit Neueinwahl bzw. Zählerstand der Box
	BytesRecv  int64        `json:"bytesRecv,omitempty"`
	RateSent   int64        `json:"rateSent,omitempty"` // Byte/s
	RateRecv   int64        `json:"rateRecv,omitempty"`
	UpdAvail   bool         `json:"updAvail,omitempty"`
	UpdVersion string       `json:"updVersion,omitempty"`
	Log        []string     `json:"log,omitempty"`
	VoIP       []fritzVoIP  `json:"voip,omitempty"`
	Dect       []fritzDect  `json:"dect,omitempty"`
	Calls      []fritzCall  `json:"calls,omitempty"`
	Phones     []fritzPhone `json:"phones,omitempty"`
	TAM        []fritzTAM   `json:"tam,omitempty"`
	Ports      []string     `json:"ports,omitempty"` // analoge Anschlüsse FON 1–3
	Defl       []fritzDefl  `json:"defl,omitempty"`
	Smart      []fritzSmart `json:"smart,omitempty"`
	Missing    []string     `json:"missing,omitempty"` // was nicht abfragbar war (Rechte, Modell)
}

const fritzExtraEvery = 5 * time.Minute

// callArgs: TR-064-Aktion mit Eingabewerten.
func (c *tr064Client) callArgs(control, service, action string, args map[string]string) (map[string]string, error) {
	if len(args) == 0 {
		return c.call(control, service, action)
	}
	var in strings.Builder
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(args[k]))
		fmt.Fprintf(&in, "<%s>%s</%s>", k, b.String(), k)
	}
	saved := c.bodyOverride
	c.bodyOverride = fmt.Sprintf(soapEnvArgs, action, service, in.String(), action)
	defer func() { c.bodyOverride = saved }()
	return c.call(control, service, action)
}

const soapEnvArgs = `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%s xmlns:u="%s">%s</u:%s></s:Body></s:Envelope>`

// fetchPath lädt eine von der Box genannte Datei (z. B. /devicehostlist.lua?sid=…).
func (c *tr064Client) fetchPath(p string) ([]byte, error) {
	u := p
	if !strings.HasPrefix(p, "http") {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		u = c.base + p
	}
	b, err := c.get(u)
	// Fernzugriff: die Fritzbox liefert Dateipfade teils ohne bzw. mit /tr064 – den jeweils anderen Weg probieren
	if err != nil && !strings.HasPrefix(p, "http") && strings.HasSuffix(c.base, "/tr064") {
		if b2, err2 := c.get(strings.TrimSuffix(c.base, "/tr064") + p); err2 == nil {
			return b2, nil
		}
	}
	return b, err
}

func (c *tr064Client) get(u string) ([]byte, error) {
	resp, err := c.hc.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// xmlItems: Liste von <Item>…</Item> mit ihren Blatt-Elementen.
func xmlItems(b []byte, item string) []map[string]string {
	var out []map[string]string
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var cur map[string]string
	var name string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == item {
				cur = map[string]string{}
			}
			name = t.Name.Local
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if t.Name.Local == item && cur != nil {
				out = append(out, cur)
				cur = nil
			} else if cur != nil && name == t.Name.Local {
				cur[name] = strings.TrimSpace(text.String())
			}
			name = ""
		}
	}
	return out
}

func atoiI(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }

func fritzExtraQuery(c *tr064Client) *fritzExtra {
	x := &fritzExtra{At: time.Now().UnixMilli()}
	miss := func(what string, err error) {
		x.Missing = append(x.Missing, what+": "+shortErr(err))
	}
	// Geräte im Heimnetz
	if v, err := c.call("/upnp/control/hosts", "urn:dslforum-org:service:Hosts:1", "X_AVM-DE_GetHostListPath"); err == nil && v["NewX_AVM-DE_HostListPath"] != "" {
		if b, err := c.fetchPath(v["NewX_AVM-DE_HostListPath"]); err == nil {
			for _, it := range xmlItems(b, "Item") {
				d := fritzDev{Name: it["HostName"], IP: it["IPAddress"], MAC: strings.ToLower(it["MACAddress"]), Iface: it["InterfaceType"],
					Active: it["Active"] == "1", Speed: atoiI(it["X_AVM-DE_Speed"]), Guest: it["X_AVM-DE_Guest"] == "1"}
				if d.MAC != "" || d.IP != "" {
					x.Devices = append(x.Devices, d)
				}
			}
		} else {
			miss("Geräteliste", err)
		}
	} else if err != nil {
		miss("Geräteliste", err)
	}
	// WLAN (1 = 2,4 GHz, 2 = 5 GHz, 3 = Gäste bzw. 6 GHz – je nach Modell)
	for i := 1; i <= 4; i++ {
		ctl, svc := "/upnp/control/wlanconfig"+strconv.Itoa(i), "urn:dslforum-org:service:WLANConfiguration:"+strconv.Itoa(i)
		v, err := c.call(ctl, svc, "GetInfo")
		if err != nil {
			if i == 1 {
				miss("WLAN", err)
			}
			break
		}
		w := fritzWLAN{Idx: i, SSID: v["NewSSID"], On: v["NewEnable"] == "1", Channel: atoiI(v["NewChannel"]), Standard: v["NewStandard"], Band: v["NewX_AVM-DE_FrequencyBand"]}
		w.Band = wlanBand(w, i)
		if a, err := c.call(ctl, svc, "GetTotalAssociations"); err == nil {
			w.Clients = atoiI(a["NewTotalAssociations"])
		}
		x.WLAN = append(x.WLAN, w)
	}
	// Datenverkehr
	if v, err := c.call("/upnp/control/wancommonifconfig1", svcCommonIf, "GetAddonInfos"); err == nil {
		x.RateSent, x.RateRecv = atoi64(v["NewByteSendRate"]), atoi64(v["NewByteReceiveRate"])
		x.BytesSent, x.BytesRecv = atoi64(v["NewX_AVM_DE_TotalBytesSent64"]), atoi64(v["NewX_AVM_DE_TotalBytesReceived64"])
	} else {
		miss("Datenverkehr", err)
	}
	if x.BytesSent == 0 {
		if v, err := c.call("/upnp/control/wancommonifconfig1", svcCommonIf, "GetTotalBytesSent"); err == nil {
			x.BytesSent = atoi64(v["NewTotalBytesSent"])
		}
		if v, err := c.call("/upnp/control/wancommonifconfig1", svcCommonIf, "GetTotalBytesReceived"); err == nil {
			x.BytesRecv = atoi64(v["NewTotalBytesReceived"])
		}
	}
	// Firmware-Update
	if v, err := c.call("/upnp/control/userif", "urn:dslforum-org:service:UserInterface:1", "GetInfo"); err == nil {
		x.UpdAvail = v["NewUpgradeAvailable"] == "1"
		x.UpdVersion = v["NewX_AVM-DE_Version"]
	} else {
		miss("Firmware-Update", err)
	}
	// Ereignisprotokoll (neueste zuerst, höchstens 60 Zeilen)
	if v, err := c.call("/upnp/control/deviceinfo", svcDeviceInfo, "X_AVM-DE_GetDeviceLogPath"); err == nil && v["NewDeviceLogPath"] != "" {
		if b, err := c.fetchPath(v["NewDeviceLogPath"]); err == nil {
			for _, it := range xmlItems(b, "Event") {
				line := strings.TrimSpace(it["Date"] + " " + it["Time"] + " " + it["Msg"])
				if line != "" {
					x.Log = append(x.Log, line)
				}
			}
			if len(x.Log) == 0 { // anderes Format: Textzeilen
				for _, l := range strings.Split(string(b), "\n") {
					if l = strings.TrimSpace(stripTags(l)); len(l) > 8 {
						x.Log = append(x.Log, l)
					}
				}
			}
		}
	} else if v, err := c.call("/upnp/control/deviceinfo", svcDeviceInfo, "GetDeviceLog"); err == nil {
		for _, l := range strings.Split(v["NewDeviceLog"], "\n") {
			if l = strings.TrimSpace(l); l != "" {
				x.Log = append(x.Log, l)
			}
		}
	} else {
		miss("Ereignisprotokoll", err)
	}
	if len(x.Log) > 60 {
		x.Log = x.Log[:60]
	}
	// Telefonie: Rufnummern und Anmeldestatus der Internet-Rufnummern
	if v, err := c.call("/upnp/control/x_voip", "urn:dslforum-org:service:X_VoIP:1", "X_AVM-DE_GetNumbers"); err == nil {
		var idx []int
		for _, it := range xmlItems([]byte(v["NewNumberList"]), "Item") {
			if n := it["Number"]; n != "" {
				x.VoIP = append(x.VoIP, fritzVoIP{Number: n, Name: it["Name"]})
				i := len(x.VoIP) - 1
				if it["Index"] != "" && (it["Type"] == "" || strings.EqualFold(it["Type"], "eVoIP")) {
					i = atoiI(it["Index"])
				} else if it["Type"] != "" && !strings.EqualFold(it["Type"], "eVoIP") {
					i = -1 // Festnetz/ISDN/Mobil: keine SIP-Anmeldung
				}
				idx = append(idx, i)
			}
		}
		for k := 0; k < len(x.VoIP) && k < 20; k++ {
			i := idx[k]
			if i < 0 {
				continue
			}
			a := map[string]string{"NewX_AVM-DE_VoIPAccountIndex": strconv.Itoa(i)}
			if st, err := c.callArgs("/upnp/control/x_voip", svcVoIPx, "X_AVM-DE_GetVoIPStatus", a); err == nil {
				x.VoIP[k].Status = st["NewX_AVM-DE_VoIPStatus"]
			}
			if ac, err := c.callArgs("/upnp/control/x_voip", svcVoIPx, "X_AVM-DE_GetVoIPAccount", map[string]string{"NewVoIPAccountIndex": strconv.Itoa(i)}); err == nil {
				x.VoIP[k].Registrar, x.VoIP[k].User = ac["NewVoIPRegistrar"], ac["NewVoIPUsername"]
			}
		}
	} else {
		miss("Telefonie", err)
	}
	// Anrufliste (letzte 14 Tage, höchstens 40) und Rufumleitungen
	const svcOnTel = "urn:dslforum-org:service:X_AVM-DE_OnTel:1"
	if v, err := c.call("/upnp/control/x_contact", svcOnTel, "GetCallList"); err == nil && v["NewCallListURL"] != "" {
		p := v["NewCallListURL"]
		if u, err := url.Parse(p); err == nil && u.Path != "" {
			p = u.Path + "?" + u.RawQuery // Host aus der URL (fritz.box) ist von außen nicht erreichbar
		}
		if b, err := c.fetchPath(p + "&max=40&days=14"); err == nil {
			for _, it := range xmlItems(b, "Call") {
				cl := fritzCall{Date: it["Date"], Name: it["Name"], Device: it["Device"], Dur: it["Duration"]}
				in := true
				switch it["Type"] {
				case "1":
					cl.Type = "in"
				case "2":
					cl.Type = "missed"
				case "3":
					cl.Type, in = "out", false
				case "10":
					cl.Type = "rejected"
				case "9":
					cl.Type = "active"
				case "11":
					cl.Type, in = "active", false
				default:
					cl.Type = it["Type"]
				}
				if in {
					cl.Other, cl.Own = it["Caller"], firstNonEmpty(it["CalledNumber"], it["Called"])
				} else {
					cl.Other, cl.Own = it["Called"], firstNonEmpty(it["CallerNumber"], it["Caller"])
				}
				x.Calls = append(x.Calls, cl)
				if len(x.Calls) >= 40 {
					break
				}
			}
		} else {
			miss("Anrufliste", err)
		}
	} else if err != nil {
		miss("Anrufliste", err)
	}
	if v, err := c.call("/upnp/control/x_contact", svcOnTel, "GetDeflections"); err == nil {
		for _, it := range xmlItems([]byte(v["NewDeflectionList"]), "Item") {
			x.Defl = append(x.Defl, fritzDefl{On: it["Enable"] == "1", Number: it["Number"], To: it["DeflectionToNumber"], Mode: it["Mode"]})
		}
	} else {
		miss("Rufumleitungen", err)
	}
	// IP-Telefone und analoge Anschlüsse
	if v, err := c.call("/upnp/control/x_voip", svcVoIPx, "X_AVM-DE_GetNumberOfClients"); err == nil {
		n := atoiI(v["NewX_AVM-DE_NumberOfClients"])
		for i := 0; i < n && i < 30; i++ {
			e, err := c.callArgs("/upnp/control/x_voip", svcVoIPx, "X_AVM-DE_GetClient2", map[string]string{"NewX_AVM-DE_ClientIndex": strconv.Itoa(i)})
			if err != nil {
				break
			}
			x.Phones = append(x.Phones, fritzPhone{Name: e["NewX_AVM-DE_PhoneName"], User: e["NewX_AVM-DE_ClientUsername"], Internal: e["NewX_AVM-DE_InternalNumber"],
				Out: e["NewX_AVM-DE_OutGoingNumber"], External: e["NewX_AVM-DE_ExternalRegistration"] == "1"})
		}
	}
	for i := 1; i <= 3; i++ {
		e, err := c.callArgs("/upnp/control/x_voip", svcVoIPx, "X_AVM-DE_GetPhonePort", map[string]string{"NewIndex": strconv.Itoa(i)})
		if err != nil {
			break
		}
		if n := strings.TrimSpace(e["NewX_AVM-DE_PhoneName"]); n != "" {
			x.Ports = append(x.Ports, "FON "+strconv.Itoa(i)+": "+n)
		}
	}
	// Anrufbeantworter
	const svcTAM = "urn:dslforum-org:service:X_AVM-DE_TAM:1"
	for i := 0; i < 5; i++ {
		e, err := c.callArgs("/upnp/control/x_tam", svcTAM, "GetInfo", map[string]string{"NewIndex": strconv.Itoa(i)})
		if err != nil {
			break
		}
		if e["NewName"] == "" && e["NewEnable"] != "1" {
			continue // nicht eingerichtet
		}
		t := fritzTAM{Name: e["NewName"], On: e["NewEnable"] == "1"}
		if m, err := c.callArgs("/upnp/control/x_tam", svcTAM, "GetMessageList", map[string]string{"NewIndex": strconv.Itoa(i)}); err == nil && m["NewURL"] != "" {
			p := m["NewURL"]
			if u, err := url.Parse(p); err == nil && u.Path != "" {
				p = u.Path + "?" + u.RawQuery
			}
			if b, err := c.fetchPath(p); err == nil {
				for _, it := range xmlItems(b, "Message") {
					t.Total++
					if it["New"] == "1" {
						t.New++
					}
				}
			}
		}
		x.TAM = append(x.TAM, t)
	}
	// DECT-Mobilteile
	const svcDect = "urn:dslforum-org:service:X_AVM-DE_Dect:1"
	if v, err := c.call("/upnp/control/x_dect", svcDect, "GetNumberOfDectEntries"); err == nil {
		n := atoiI(v["NewNumberOfEntries"])
		for i := 0; i < n && i < 30; i++ {
			e, err := c.callArgs("/upnp/control/x_dect", svcDect, "GetGenericDectEntry", map[string]string{"NewIndex": strconv.Itoa(i)})
			if err != nil {
				break
			}
			x.Dect = append(x.Dect, fritzDect{ID: e["NewID"], Name: e["NewName"], Model: e["NewModel"], Active: e["NewActive"] == "1", Update: e["NewUpdateAvailable"] == "1",
				UpdOK: e["NewUpdateSuccessful"], Info: e["NewUpdateInfo"]})
		}
	} else {
		miss("DECT", err)
	}
	// Smart Home (DECT-Thermostate, Steckdosen …)
	for i := 0; i < 40; i++ {
		v, err := c.callArgs("/upnp/control/x_homeauto", "urn:dslforum-org:service:X_AVM-DE_Homeauto:1", "GetGenericDeviceInfos", map[string]string{"NewIndex": strconv.Itoa(i)})
		if err != nil {
			if i == 0 && !strings.Contains(err.Error(), "713") && !strings.Contains(err.Error(), "SpecifiedArrayIndexInvalid") {
				miss("Smart Home", err)
			}
			break
		}
		s := fritzSmart{Name: v["NewDeviceName"], Product: v["NewProductName"], Present: v["NewPresent"] == "CONNECTED", Switch: v["NewSwitchState"]}
		if t := atoi64(v["NewTemperatureCelsius"]); v["NewTemperatureIsEnabled"] == "ENABLED" {
			s.TempC = float64(t) / 10
		}
		if v["NewHkrIsEnabled"] == "ENABLED" {
			s.Target = float64(atoi64(v["NewHkrSetTemperature"])) / 10
		}
		if v["NewMultimeterIsEnabled"] == "ENABLED" {
			s.PowerW = float64(atoi64(v["NewMultimeterPower"])) / 100
			s.EnergyW = atoi64(v["NewMultimeterEnergy"])
		}
		if v["NewSwitchIsEnabled"] != "ENABLED" {
			s.Switch = ""
		}
		x.Smart = append(x.Smart, s)
	}
	return x
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fritzExtraIngest (unter fh.mu): letzten Stand merken, neue Geräte und Firmware-Update melden.
func (m *Monitor) fritzExtraIngest(hc HostCfg, fh *fritzHost, x *fritzExtra, recent bool) []notice {
	var notes []notice
	m.fritzLogLoad(hc.ID, fh)
	var newMACs []string
	first := fh.extra == nil && len(fh.known) == 0
	fh.extra = x
	logChanged := false
	fh.logHist, logChanged = mergeFritzLog(fh.logHist, x.Log)
	if x.Devices != nil {
		if fh.known == nil {
			fh.known = map[string]bool{}
		}
		var neu []string
		for _, d := range x.Devices {
			if d.MAC == "" || fh.known[d.MAC] {
				continue
			}
			fh.known[d.MAC] = true
			newMACs = append(newMACs, d.MAC)
			if !first && d.Active {
				n := d.Name
				if n == "" {
					n = "unbekannt"
				}
				neu = append(neu, fmt.Sprintf("%s (%s, %s)", n, d.IP, d.MAC))
			}
		}
		if len(neu) > 0 && hc.FritzNewDev && recent && !m.inMaint(hc.Org, hc.ID, time.Now()) {
			if len(neu) > 10 {
				neu = append(neu[:10], fmt.Sprintf("… und %d weitere", len(neu)-10))
			}
			notes = append(notes, notice{"fritzdev", hc.ID, "🆕 Neues Gerät im Netz: " + hc.Name,
				m.withInfo(hc.ID, hc.Site, strings.Join(neu, "\n")), hc.Org, ""})
		}
	}
	if x.UpdAvail {
		if fh.fwNoted != x.UpdVersion && !first && recent {
			v := x.UpdVersion
			if v == "" {
				v = "neue Version"
			}
			notes = append(notes, notice{"fritzfw", hc.ID, "⬆️ Fritzbox-Update verfügbar: " + hc.Name,
				m.withInfo(hc.ID, hc.Site, "FRITZ!OS "+v+" kann installiert werden."), hc.Org, ""})
		}
		fh.fwNoted = x.UpdVersion
	} else {
		fh.fwNoted = ""
	}
	if logChanged || len(newMACs) > 0 {
		m.fritzLogSave(hc.ID, fh)
	}
	return notes
}

// maskNumber zeigt nur die letzten vier Ziffern (Datensparsamkeit).
func maskNumber(n string) string {
	d := 0
	for i := len(n) - 1; i >= 0; i-- {
		if n[i] >= '0' && n[i] <= '9' {
			d++
			if d == 4 {
				return "…" + n[i:]
			}
		}
	}
	return n
}

// view: Kopie für den Browser – Rufnummern gekürzt, außer „vollständig anzeigen“ ist gesetzt.
func (x *fritzExtra) view(full bool) *fritzExtra {
	if x == nil || full || len(x.VoIP)+len(x.Calls)+len(x.Defl) == 0 {
		return x
	}
	c := *x
	c.VoIP = make([]fritzVoIP, len(x.VoIP))
	for i, v := range x.VoIP {
		v.Number, v.User = maskNumber(v.Number), maskNumber(v.User)
		c.VoIP[i] = v
	}
	c.Calls = make([]fritzCall, len(x.Calls))
	for i, v := range x.Calls {
		v.Other, v.Own = maskNumber(v.Other), maskNumber(v.Own)
		c.Calls[i] = v
	}
	c.Phones = make([]fritzPhone, len(x.Phones))
	for i, v := range x.Phones {
		v.Out = maskNumber(v.Out)
		c.Phones[i] = v
	}
	c.Defl = make([]fritzDefl, len(x.Defl))
	for i, v := range x.Defl {
		v.Number, v.To = maskNumber(v.Number), maskNumber(v.To)
		c.Defl[i] = v
	}
	return &c
}

const svcVoIPx = "urn:dslforum-org:service:X_VoIP:1"

func firstNonEmpty(a ...string) string {
	for _, x := range a {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// wlanBand: lesbares Frequenzband – aus der Angabe der Box, sonst aus Kanal/Standard/Position.
func wlanBand(w fritzWLAN, i int) string {
	b := strings.ToLower(w.Band)
	switch {
	case strings.HasPrefix(b, "24") || strings.HasPrefix(b, "2.4") || strings.HasPrefix(b, "2,4"):
		b = "2,4 GHz"
	case strings.HasPrefix(b, "5"):
		b = "5 GHz"
	case strings.HasPrefix(b, "6"):
		b = "6 GHz"
	default:
		b = ""
	}
	if b == "" {
		switch {
		case w.Channel >= 1 && w.Channel <= 14:
			b = "2,4 GHz"
		case w.Channel >= 36:
			b = "5 GHz"
		case strings.Contains(w.Standard, "ac") || strings.Contains(w.Standard, "ax"):
			b = "5 GHz"
		case i == 1:
			b = "2,4 GHz"
		case i == 2:
			b = "5 GHz"
		}
	}
	if strings.Contains(strings.ToLower(w.SSID), "gast") || strings.Contains(strings.ToLower(w.SSID), "guest") {
		if b != "" {
			return "Gastnetz (" + b + ")"
		}
		return "Gastnetz"
	}
	return b
}

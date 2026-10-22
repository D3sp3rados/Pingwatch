package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Erweiterte Fritzbox-Auslesung (alle 5 Minuten): Geräte im Heimnetz, WLAN, Datenverkehr,
// Firmware-Update, Ereignisprotokoll, Rufnummern/Telefonie-Anmeldung und Smart-Home-Geräte.
// Jede Abfrage ist für sich optional – was die Box (oder der Benutzer) nicht kann, fehlt einfach.
// Die Anrufliste wird bewusst nicht übertragen (Rufnummern Dritter).

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
	Number string `json:"number"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"` // Registered / Not registered …
}

type fritzDect struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Model  string `json:"model,omitempty"`
	Active bool   `json:"active"`
	Update bool   `json:"update,omitempty"` // Firmware-Update für das Mobilteil verfügbar
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
		for _, it := range xmlItems([]byte(v["NewNumberList"]), "Item") {
			if n := it["Number"]; n != "" {
				x.VoIP = append(x.VoIP, fritzVoIP{Number: n, Name: it["Name"]})
			}
		}
		for i := 0; i < len(x.VoIP) && i < 20; i++ {
			if st, err := c.callArgs("/upnp/control/x_voip", "urn:dslforum-org:service:X_VoIP:1", "X_AVM-DE_GetVoIPStatus", map[string]string{"NewX_AVM-DE_VoIPAccountIndex": strconv.Itoa(i)}); err == nil {
				x.VoIP[i].Status = st["NewX_AVM-DE_VoIPStatus"]
			}
		}
	} else {
		miss("Telefonie", err)
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
			x.Dect = append(x.Dect, fritzDect{ID: e["NewID"], Name: e["NewName"], Model: e["NewModel"], Active: e["NewActive"] == "1", Update: e["NewUpdateAvailable"] == "1"})
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
	first := fh.extra == nil
	fh.extra = x
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
	if x == nil || full || len(x.VoIP) == 0 {
		return x
	}
	c := *x
	c.VoIP = make([]fritzVoIP, len(x.VoIP))
	for i, v := range x.VoIP {
		v.Number = maskNumber(v.Number)
		c.VoIP[i] = v
	}
	return &c
}

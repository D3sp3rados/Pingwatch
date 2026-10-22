package main

import (
	"html"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFritzExtraQuery(t *testing.T) {
	f := &fakeFritz{user: "u", pass: "p", conn: "Connected", connUp: 60, boxUp: 600}
	f.files = map[string]string{
		"/devicehostlist.lua": `<List><Item><Index>1</Index><IPAddress>192.168.178.20</IPAddress><MACAddress>AA:BB:CC:00:11:22</MACAddress><Active>1</Active><HostName>NAS</HostName><InterfaceType>Ethernet</InterfaceType><X_AVM-DE_Speed>1000</X_AVM-DE_Speed></Item><Item><IPAddress>192.168.178.30</IPAddress><MACAddress>AA:BB:CC:00:11:33</MACAddress><Active>0</Active><HostName>Handy</HostName><InterfaceType>802.11</InterfaceType><X_AVM-DE_Guest>1</X_AVM-DE_Guest></Item></List>`,
		"/devlog.lua":         `<DeviceLog><Event><Date>23.10.26</Date><Time>00:12:01</Time><Msg>Internetverbindung wurde erfolgreich hergestellt.</Msg></Event></DeviceLog>`,
	}
	var voipArgs []string
	f.more = func(act, body string) map[string]string {
		switch act {
		case "urn:dslforum-org:service:Hosts:1#X_AVM-DE_GetHostListPath":
			return map[string]string{"NewX_AVM-DE_HostListPath": "/devicehostlist.lua?sid=abc"}
		case "urn:dslforum-org:service:WLANConfiguration:1#GetInfo":
			return map[string]string{"NewSSID": "Büro", "NewEnable": "1", "NewChannel": "6", "NewX_AVM-DE_FrequencyBand": "2400"}
		case "urn:dslforum-org:service:WLANConfiguration:1#GetTotalAssociations":
			return map[string]string{"NewTotalAssociations": "7"}
		case svcCommonIf + "#GetAddonInfos":
			return map[string]string{"NewByteSendRate": "1000", "NewByteReceiveRate": "5000", "NewX_AVM_DE_TotalBytesSent64": "123456789", "NewX_AVM_DE_TotalBytesReceived64": "987654321"}
		case "urn:dslforum-org:service:UserInterface:1#GetInfo":
			return map[string]string{"NewUpgradeAvailable": "1", "NewX_AVM-DE_Version": "8.02"}
		case svcDeviceInfo + "#X_AVM-DE_GetDeviceLogPath":
			return map[string]string{"NewDeviceLogPath": "/devlog.lua?sid=abc"}
		case "urn:dslforum-org:service:X_VoIP:1#X_AVM-DE_GetNumbers":
			return map[string]string{"NewNumberList": html.EscapeString(`<List><Item><Number>0711123456</Number><Name>Zentrale</Name></Item></List>`)}
		case "urn:dslforum-org:service:X_VoIP:1#X_AVM-DE_GetVoIPStatus":
			voipArgs = append(voipArgs, body)
			return map[string]string{"NewX_AVM-DE_VoIPStatus": "Registered"}
		case "urn:dslforum-org:service:X_AVM-DE_Dect:1#GetNumberOfDectEntries":
			return map[string]string{"NewNumberOfEntries": "1"}
		case "urn:dslforum-org:service:X_AVM-DE_Dect:1#GetGenericDectEntry":
			return map[string]string{"NewID": "1", "NewName": "Empfang", "NewModel": "FRITZ!Fon C6", "NewActive": "1", "NewUpdateAvailable": "0"}
		case "urn:dslforum-org:service:X_AVM-DE_Homeauto:1#GetGenericDeviceInfos":
			if strings.Contains(body, "<NewIndex>0</NewIndex>") {
				return map[string]string{"NewDeviceName": "Heizung Büro", "NewPresent": "CONNECTED", "NewTemperatureIsEnabled": "ENABLED", "NewTemperatureCelsius": "215", "NewHkrIsEnabled": "ENABLED", "NewHkrSetTemperature": "220"}
			}
		}
		return nil
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	x := fritzExtraQuery(newTR064(srv.URL, "u", "p"))
	if len(x.Devices) != 2 || x.Devices[0].Name != "NAS" || !x.Devices[0].Active || x.Devices[0].Speed != 1000 || x.Devices[0].MAC != "aa:bb:cc:00:11:22" || !x.Devices[1].Guest {
		t.Fatalf("Geräte: %+v", x.Devices)
	}
	if len(x.WLAN) != 1 || x.WLAN[0].SSID != "Büro" || x.WLAN[0].Clients != 7 {
		t.Fatalf("WLAN: %+v", x.WLAN)
	}
	if x.BytesRecv != 987654321 || x.RateRecv != 5000 || !x.UpdAvail || x.UpdVersion != "8.02" {
		t.Fatalf("Verkehr/Update: %+v", x)
	}
	if len(x.Log) != 1 || !strings.Contains(x.Log[0], "erfolgreich") {
		t.Fatalf("Protokoll: %v", x.Log)
	}
	if len(x.VoIP) != 1 || x.VoIP[0].Status != "Registered" || len(voipArgs) != 1 || !strings.Contains(voipArgs[0], "<NewX_AVM-DE_VoIPAccountIndex>0</NewX_AVM-DE_VoIPAccountIndex>") {
		t.Fatalf("Telefonie: %+v %v", x.VoIP, voipArgs)
	}
	if len(x.Dect) != 1 || x.Dect[0].Name != "Empfang" || !x.Dect[0].Active {
		t.Fatalf("DECT: %+v", x.Dect)
	}
	if v := x.view(false); v.VoIP[0].Number != "…3456" || x.VoIP[0].Number != "0711123456" {
		t.Fatalf("Kürzen: %q / %q", v.VoIP[0].Number, x.VoIP[0].Number)
	}
	if x.view(true).VoIP[0].Number != "0711123456" {
		t.Fatal("vollständig")
	}
	if len(x.Smart) != 1 || x.Smart[0].TempC != 21.5 || x.Smart[0].Target != 22 || !x.Smart[0].Present {
		t.Fatalf("Smart Home: %+v", x.Smart)
	}
}

func TestFritzExtraNotify(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	hc := HostCfg{ID: "h1", Name: "Box", Org: "o1", Fritz: true, FritzNewDev: true}
	fh := &fritzHost{}
	x1 := &fritzExtra{Devices: []fritzDev{{MAC: "a", Active: true}}}
	if n := m.fritzExtraIngest(hc, fh, x1, true); len(n) != 0 {
		t.Fatalf("erste Abfrage darf nichts melden: %+v", n)
	}
	x2 := &fritzExtra{Devices: []fritzDev{{MAC: "a", Active: true}, {MAC: "b", Name: "Neu", IP: "10.0.0.9", Active: true}}, UpdAvail: true, UpdVersion: "8.02"}
	n := m.fritzExtraIngest(hc, fh, x2, true)
	if len(n) != 2 || n[0].kind != "fritzdev" || !strings.Contains(n[0].body, "Neu (10.0.0.9, b)") || n[1].kind != "fritzfw" {
		t.Fatalf("Meldungen: %+v", n)
	}
	if n := m.fritzExtraIngest(hc, fh, x2, true); len(n) != 0 {
		t.Fatalf("doppelt gemeldet: %+v", n)
	}
	hc.FritzNewDev = false
	x3 := &fritzExtra{Devices: []fritzDev{{MAC: "c", Active: true}}, UpdAvail: true, UpdVersion: "8.02"}
	if n := m.fritzExtraIngest(hc, fh, x3, true); len(n) != 0 {
		t.Fatalf("ohne Opt-in: %+v", n)
	}
}

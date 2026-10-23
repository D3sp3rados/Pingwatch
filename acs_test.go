package main

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

const testInform = `<?xml version="1.0"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
<SOAP-ENV:Header><cwmp:ID SOAP-ENV:mustUnderstand="1">42</cwmp:ID></SOAP-ENV:Header>
<SOAP-ENV:Body><cwmp:Inform>
<DeviceId><Manufacturer>AVM</Manufacturer><OUI>00040E</OUI><ProductClass>FRITZ!Box</ProductClass><SerialNumber>AABBCC112233</SerialNumber></DeviceId>
<Event><EventStruct><EventCode>0 BOOTSTRAP</EventCode><CommandKey></CommandKey></EventStruct><EventStruct><EventCode>1 BOOT</EventCode><CommandKey></CommandKey></EventStruct></Event>
<MaxEnvelopes>1</MaxEnvelopes><CurrentTime>2026-10-23T02:00:00</CurrentTime><RetryCount>0</RetryCount>
<ParameterList>
<ParameterValueStruct><Name>InternetGatewayDevice.DeviceInfo.SoftwareVersion</Name><Value xsi:type="xsd:string">164.08.00</Value></ParameterValueStruct>
<ParameterValueStruct><Name>InternetGatewayDevice.DeviceInfo.ModelName</Name><Value xsi:type="xsd:string">FRITZ!Box 5590 Fiber</Value></ParameterValueStruct>
<ParameterValueStruct><Name>InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.1.ExternalIPAddress</Name><Value xsi:type="xsd:string">203.0.113.9</Value></ParameterValueStruct>
</ParameterList></cwmp:Inform></SOAP-ENV:Body></SOAP-ENV:Envelope>`

func cwmpResp(id, body string) string {
	return `<?xml version="1.0"?><SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-0"><SOAP-ENV:Header><cwmp:ID SOAP-ENV:mustUnderstand="1">` + id + `</cwmp:ID></SOAP-ENV:Header><SOAP-ENV:Body>` + body + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
}

func TestACSSession(t *testing.T) {
	t.Setenv("ACS_ENABLE", "1")
	s, ts := newTestServer(t)
	jar, _ := cookiejar.New(nil)
	cpe := &http.Client{Jar: jar}
	post := func(body string) (int, string) {
		resp, err := cpe.Post(ts.URL+"/cwmp", "text/xml", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Profil vorab: Gerät erst nach dem ersten Inform bekannt → erster Kontakt
	code, body := post(testInform)
	if code != 200 || !strings.Contains(body, "InformResponse") || !strings.Contains(body, ">42<") {
		t.Fatal(code, body)
	}
	s.m.mu.RLock()
	d := *s.m.acsDevice("00040E-FRITZ!Box-AABBCC112233")
	s.m.mu.RUnlock()
	if d.Model != "FRITZ!Box 5590 Fiber" || d.SW != "164.08.00" || d.ExtIP != "203.0.113.9" {
		t.Fatalf("Gerät: %+v", d)
	}
	// leerer POST → Grunddaten abfragen
	code, body = post("")
	if code != 200 || !strings.Contains(body, "GetParameterValues") || !strings.Contains(body, "InternetGatewayDevice.DeviceInfo.") {
		t.Fatal(code, body)
	}
	id := between(body, `mustUnderstand="1">`, "</cwmp:ID>")
	code, body = post(cwmpResp(id, `<cwmp:GetParameterValuesResponse><ParameterList><ParameterValueStruct><Name>InternetGatewayDevice.WANDevice.1.WANConnectionDevice.2.WANPPPConnection.1.Username</Name><Value xsi:type="xsd:string"></Value></ParameterValueStruct><ParameterValueStruct><Name>InternetGatewayDevice.WANDevice.1.WANConnectionDevice.2.WANPPPConnection.1.Password</Name><Value xsi:type="xsd:string">geheim</Value></ParameterValueStruct></ParameterList></cwmp:GetParameterValuesResponse>`))
	if code != 204 {
		t.Fatal("Sitzungsende erwartet", code, body)
	}
	// Betreiber setzt Profil mit PPPoE-Zugang → wird beim nächsten Kontakt gesetzt
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	if code, b := c.do("POST", "/api/acs", map[string]any{"action": "profile", "id": d.ID, "profile": map[string]any{"pppUser": "kunde4711@rst", "pppPass": "pw-123"}}); code != 200 {
		t.Fatal(code, b)
	}
	code, body = post(informWith("2 PERIODIC"))
	if code != 200 {
		t.Fatal(code)
	}
	code, body = post("")
	if !strings.Contains(body, "SetParameterValues") || !strings.Contains(body, "WANConnectionDevice.2.WANPPPConnection.1.Username") || !strings.Contains(body, "kunde4711@rst") {
		t.Fatal("Profil nicht gesendet:", body)
	}
	id = between(body, `mustUnderstand="1">`, "</cwmp:ID>")
	code, body = post(cwmpResp(id, `<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>`))
	if code != 204 {
		t.Fatal(code, body)
	}
	// Ansicht für den Browser: keine Passwörter
	_, view := c.do("GET", "/api/acs", nil)
	if strings.Contains(view, "pw-123") || strings.Contains(view, "geheim") || !strings.Contains(view, "kunde4711@rst") || !strings.Contains(view, "gesetzt (Status 0)") {
		t.Fatal("Ansicht:", view)
	}
	// Fehler der Box wird als Fehler des Auftrags vermerkt
	c.do("POST", "/api/acs", map[string]any{"action": "task", "id": d.ID, "task": map[string]any{"kind": "reboot"}})
	post(informWith("6 CONNECTION REQUEST"))
	_, body = post("")
	id = between(body, `mustUnderstand="1">`, "</cwmp:ID>")
	post(cwmpResp(id, `<SOAP-ENV:Fault><faultcode>Client</faultcode><faultstring>CWMP fault</faultstring><detail><cwmp:Fault><FaultCode>9001</FaultCode><FaultString>Request denied</FaultString></cwmp:Fault></detail></SOAP-ENV:Fault>`))
	_, view = c.do("GET", "/api/acs", nil)
	if !strings.Contains(view, "Request denied (9001)") {
		t.Fatal("Fehler:", view)
	}
}

func TestACSDisabledAndAuth(t *testing.T) {
	_, ts := newTestServer(t)
	resp, _ := http.Post(ts.URL+"/cwmp", "text/xml", strings.NewReader(testInform))
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("ohne ACS_ENABLE:", resp.StatusCode)
	}
	t.Setenv("ACS_ENABLE", "1")
	t.Setenv("ACS_CPE_USER", "cpe")
	t.Setenv("ACS_CPE_PASS", "pw")
	resp, _ = http.Post(ts.URL+"/cwmp", "text/xml", strings.NewReader(testInform))
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("ohne Anmeldung:", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/cwmp", strings.NewReader(testInform))
	req.SetBasicAuth("cpe", "pw")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("mit Anmeldung:", resp.StatusCode)
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

func informWith(ev string) string {
	s := strings.Replace(testInform, "0 BOOTSTRAP", ev, 1)
	return strings.Replace(s, "<EventStruct><EventCode>1 BOOT</EventCode><CommandKey></CommandKey></EventStruct>", "", 1)
}

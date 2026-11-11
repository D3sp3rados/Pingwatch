package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// Aufbau wie data.lua?page=sipQual (FRITZ!OS 8.2x), Rufnummern erfunden.
const sipQualHTML = `<link rel="stylesheet" type="text/css" href="/css/default/wds.css">
<table class="zebra_reverse noborder cut_table" id="uiListOfActivCalls">
<colgroup><col width="16%"></colgroup>
<tr><td class="sip_quality_align" colspan="9">Eigene Rufnummer: 07741111111 (Verfügbarkeit 100%)</td></tr>
<tr><td>11.11.2026 14:01<br>&nbsp;&nbsp;0:00:42</td><td>+4977412222<br>185.51.184.162</td>
<td><span class="LedSpan"><img src="/x.png"></span><span class="LedDesc">G.711</span><br><span class="LedSpan"><img src="/x.png"></span><span class="LedDesc">G.711</span></td>
<td>2100 (-)<br>2090 (-)</td><td>-<br>0.5 %</td><td>9.5 ms</td><td>1 ms<br>3 ms</td><td>-<br>0 ms (0 %)</td><td></td></tr>
</table>
<table class="zebra_reverse noborder cut_table" id="uiListOfAllCalls">
<tr><td class="sip_quality_align" colspan="9">Eigene Rufnummer: 07741111111 (Verfügbarkeit 100%)</td></tr>
<tr><td>11.11.2026 13:35<br>&nbsp;&nbsp;0:01:06</td><td>+4977412222<br>185.51.184.162</td>
<td><span class="LedDesc">G.722-HD</span><br><span class="LedDesc">G.722-HD</span></td>
<td>15743 (-)<br>3206 (-)</td><td>-<br>0.2 %</td><td>9.5 ms</td><td>1 ms<br>3 ms</td><td>-<br>18 ms (100 %)</td>
<td><button type="submit" class="icon feedback" data-started="11.11.2026 13:35"></button></td></tr>
<tr><td>11.11.2026 11:36<br>&nbsp;&nbsp;0:05:12 (2713 ms)</td><td>07413333<br>185.51.184.162</td>
<td><span class="LedDesc">G.711</span><br><span class="LedDesc">G.711</span></td>
<td>15296 (-)<br>10110 (-)</td><td>28 %<br>35 %</td><td>7.5 ms</td><td>2 ms<br>16 ms</td><td>-<br>42988 ms (41 %)</td><td></td></tr>
<tr><td class="sip_quality_align" colspan="9">Eigene Rufnummer: 07749999999</td></tr>
<tr><td>11.11.2026 13:46<br>&nbsp;&nbsp;0:03:30</td><td>01701234567<br>185.51.184.162</td>
<td><span class="LedDesc">G.722-HD</span><br><span class="LedDesc">G.722-HD</span></td>
<td>10186 (-)<br>10224 (-)</td><td>0.1 %<br>-</td><td>9 ms</td><td>1 ms<br>3 ms</td><td>-<br>0 ms (0 %)</td><td></td></tr>
</table>`

func TestParseSipQual(t *testing.T) {
	act, done := parseSipQual(sipQualHTML)
	if len(act) != 1 || !act[0].Active || act[0].Sec != 42 || act[0].LossRx != 0.5 || act[0].LossTx != -1 || act[0].Own != "07741111111" {
		t.Fatalf("aktive Gespräche falsch: %+v", act)
	}
	if len(done) != 3 {
		t.Fatalf("erwartet 3 beendete Gespräche, %d: %+v", len(done), done)
	}
	a := done[0]
	if a.Start != "11.11.2026 13:35" || a.Sec != 66 || a.Other != "+4977412222" || a.Remote != "185.51.184.162" || a.CodecTx != "G.722-HD" ||
		a.PktTx != 15743 || a.PktRx != 3206 || a.LossTx != -1 || a.LossRx != 0.2 || a.Delay != 9.5 || a.JitTx != 1 || a.JitRx != 3 || a.Burst != "18 ms (100 %)" {
		t.Fatalf("Gespräch 1 falsch: %+v", a)
	}
	b := done[1]
	if b.Sec != 312 || b.LossTx != 28 || b.LossRx != 35 || b.JitRx != 16 || !voiceBad(b, 3) {
		t.Fatalf("Gespräch 2 falsch: %+v", b)
	}
	if done[2].Own != "07749999999" || done[2].LossTx != 0.1 || done[2].LossRx != -1 {
		t.Fatalf("zweite Rufnummer falsch: %+v", done[2])
	}
	want := time.Date(2026, 11, 11, 13, 35, 0, 0, berlin).UnixMilli()
	if a.T != want {
		t.Fatalf("Zeit falsch: %d != %d", a.T, want)
	}
}

// Beispiele aus der AVM-Dokumentation „Session-IDs im FRITZ!Box Webinterface“.
func TestFritzLoginResponse(t *testing.T) {
	r, err := fritzLoginResponse("2$10000$5A1711$2000$5A1722", "1example!")
	if err != nil || r != "5A1722$1798a1672bca7c6463d6b245f82b53703b0f50813401b03e4045a5861e689adb" {
		t.Fatalf("PBKDF2: %q %v", r, err)
	}
	r, _ = fritzLoginResponse("1234567z", "äbc")
	if r != "1234567z-9e224a41eeefa284df7bb0f26c2913e2" {
		t.Fatalf("MD5: %q", r)
	}
}

func TestFritzWebBase(t *testing.T) {
	for in, want := range map[[2]any]string{
		{"192.168.178.1", 49000}:         "http://192.168.178.1",
		{"192.168.178.1", 0}:             "http://192.168.178.1",
		{"fritz.box", 49443}:             "https://fritz.box",
		{"185.51.186.63", 443}:           "https://185.51.186.63",
		{"https://x.myfritz.net/", 4431}: "https://x.myfritz.net:4431",
	} {
		if got := fritzWebBase(in[0].(string), in[1].(int)); got != want {
			t.Errorf("%v: %q != %q", in, got, want)
		}
	}
}

func TestNumDigits(t *testing.T) {
	for in, want := range map[string]string{"+49775392170": "0775392170", "0049775392170@sip.rst.de": "0775392170", "0775392170": "0775392170", "0040265311021": "0040265311021"} {
		if got := numDigits(in); got != want {
			t.Errorf("%q: %q != %q", in, got, want)
		}
	}
}

// Text der Push-Mail „FRITZ!Box-Info“ (so wie ein Mailprogramm sie anzeigt), Rufnummern erfunden.
const voiceMailText = "Ihre gesamte FRITZ!Box Verbindungsübersicht bis zum 11.11.2026 14:24:24 Uhr \n" +
	"Zu den FRITZ!Box-Einstellungen: https://abcdef123456.myfritz.net \n\n" +
	"Internettelefonie Sprachübertragung \n\n" +
	"\t>|\t Senderichtung \t<|\t Empfangsrichtung \n\n" +
	"Datum \tRufnummer \tSprachübertragung \n" +
	"Dauer (*) \tGegenstelle \t\tKodierung \tPakete (**) \tVerloren \tVerzögerung \tJitter \tBurst \tSonstiges \n\n" +
	"Eigene Rufnummer: 07741111111  (100 %) \n" +
	"09.11.2026 8:46\t0040700000001@sip.rst.de\n" +
	">|\tG.711\t0 (-)\t0.0 %\t\t2 ms\t- \t\n" +
	"0:02 (4721 ms)\t185.51.184.162\t<|\tG.711\t5499 (-)\t0.3 %\t\t4 ms\t0 ms (0 %)\t\n" +
	"11.11.2026 11:36\t07413333@sip.rst.de\n" +
	">|\tG.711\t15296 (-)\t28 %\t7.5 ms\t2 ms\t- \t\n" +
	"0:06 (2713 ms)\t185.51.184.162\t<|\tG.711\t10110 (-)\t35 %\t7.5 ms\t16 ms\t42988 ms (41 %)\t\n\n" +
	"Eigene Rufnummer: 07749999999  (0 %) \n" +
	"11.11.2026 13:46\t01701234567@sip.rst.de\n" +
	">|\tG.722-HD\t10186 (-)\t0.1 %\t9 ms\t1 ms\t- \t\n" +
	"0:04 (1572 ms)\t185.51.184.162\t<|\tG.722-HD\t10224 (-)\t-\t9 ms\t3 ms\t0 ms (0 %)\t\n\n\n" +
	"(*) \tAnzeige der Dauer in [hh:mm] \n\n" +
	"Ereignisse \n" +
	"11.11.26 14:08:10 \tPush Service deaktiviert. \n\n" +
	"Einstellungsübersicht vom 11.11.2026 14:24:24 Uhr \n" +
	"Produktname: \tFRITZ!Box 7690 \n"

func TestParseVoiceMail(t *testing.T) {
	d := parseVoiceMail(voiceMailText)
	if d.MyFritz != "abcdef123456.myfritz.net" || d.Model != "FRITZ!Box 7690" {
		t.Fatalf("Kopfdaten: %q %q", d.MyFritz, d.Model)
	}
	if len(d.Order) != 2 || len(d.ByOwn["07741111111"]) != 2 || len(d.ByOwn["07749999999"]) != 1 {
		t.Fatalf("Gruppen falsch: %v %+v", d.Order, d.ByOwn)
	}
	a := d.ByOwn["07741111111"][0]
	if a.Other != "0040700000001" || a.Sec != 120 || a.PktTx != 0 || a.PktRx != 5499 || a.LossTx != 0 || a.LossRx != 0.3 || a.Delay != -1 || a.JitTx != 2 || a.JitRx != 4 || a.Src != "mail" {
		t.Fatalf("Gespräch 1: %+v", a)
	}
	b := d.ByOwn["07741111111"][1]
	if b.LossTx != 28 || b.LossRx != 35 || b.Delay != 7.5 || b.Sec != 360 || b.Burst != "42988 ms (41 %)" || b.Remote != "185.51.184.162" {
		t.Fatalf("Gespräch 2: %+v", b)
	}
	// dasselbe Gespräch aus Live-Abfrage und Mail hat denselben Schlüssel
	_, live := parseSipQual(sipQualHTML)
	if live[1].key() != b.key() {
		t.Fatalf("Schlüssel unterschiedlich: %q / %q", live[1].key(), b.key())
	}
}

func TestVoiceMailHTML(t *testing.T) {
	h := `<html><head><style>td{color:red}</style></head><body><p>Internettelefonie Sprachübertragung</p><table>
<tr><td colspan="9">Eigene Rufnummer: 07741111111&nbsp; (100 %)</td></tr>
<tr><td>11.11.2026 11:36</td><td>07413333@sip.rst.de</td></tr>
<tr><td><img src="cid:a" alt="&gt;|"></td><td>G.711</td><td>15296 (-)</td><td>28 %</td><td>7.5 ms</td><td>2 ms</td><td>-</td></tr>
<tr><td>0:06 (2713 ms)</td><td>185.51.184.162</td><td><img alt="&lt;|" src="cid:b"></td><td>G.711</td><td>10110 (-)</td><td>35 %</td><td>7.5 ms</td><td>16 ms</td><td>42988 ms (41 %)</td></tr>
</table><p>Ereignisse</p></body></html>`
	raw := "From: push@example.org\r\nSubject: =?utf-8?Q?FRITZ!Box-Info?=\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"XX\"\r\n\r\n" +
		"--XX\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		strings.ReplaceAll(strings.ReplaceAll(h, "=", "=3D"), "ü", "=C3=BC") + "\r\n--XX--\r\n"
	sub, text, err := mailText([]byte(raw))
	if err != nil || sub != "FRITZ!Box-Info" {
		t.Fatalf("Mail: %q %v", sub, err)
	}
	d := parseVoiceMail(text)
	l := d.ByOwn["07741111111"]
	if len(l) != 1 || l[0].LossTx != 28 || l[0].LossRx != 35 || l[0].PktRx != 10110 {
		t.Fatalf("HTML-Mail falsch ausgewertet: %+v\n%s", l, text)
	}
}

func TestMergeVoiceAndNotes(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	hc := HostCfg{ID: "h1", Name: "Box", Org: "o1", Fritz: true, FritzVoice: true}
	fh := &fritzHost{voiceLoaded: true}
	now := time.Now().In(berlin)
	mk := func(min int, loss float64, sec int) fritzVoice {
		tt := now.Add(-time.Duration(min) * time.Minute).Truncate(time.Minute)
		return fritzVoice{T: tt.UnixMilli(), Start: tt.Format("02.01.2006 15:04"), Sec: sec, Own: "07741111111", Other: "0170123456", PktTx: min, LossTx: loss, LossRx: 0, Src: "live"}
	}
	n := m.fritzVoiceIngest(hc, fh, &fritzVoiceSnap{At: now.UnixMilli(), Calls: []fritzVoice{mk(5, 0.1, 120), mk(10, 12, 200), mk(15, 50, 10)}})
	if len(fh.voice) != 3 || len(n) != 1 || n[0].kind != "fritzvoice" || !strings.Contains(n[0].body, "12 %") || strings.Contains(n[0].body, "0170123456") {
		t.Fatalf("Meldung falsch: %+v / %d", n, len(fh.voice))
	}
	// erneut gemeldet (z. B. aus der Mail): keine doppelte Meldung, Live-Werte bleiben
	again := mk(10, 12, 0)
	again.Src = "mail"
	if n := m.fritzVoiceIngest(hc, fh, &fritzVoiceSnap{At: now.UnixMilli(), Calls: []fritzVoice{again}}); len(n) != 0 || len(fh.voice) != 3 {
		t.Fatalf("doppelt: %+v", n)
	}
}

// IMAP-Abruf gegen einen minimalen Schein-Server (ohne TLS).
func TestVoiceMailFetchIMAP(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	hc := m.UpsertHost(HostCfg{Name: "Box", Addr: "192.0.2.1", Site: "s1", Method: "icmp", Fritz: true, FritzVoiceNums: "07741111111"})
	mailBody := "From: push@example.org\r\nSubject: FRITZ!Box-Info\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + strings.ReplaceAll(voiceMailText, "\n", "\r\n")
	cli, srv := net.Pipe()
	go func() {
		r := bufio.NewReader(srv)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			f := strings.Fields(l)
			tag, cmd := f[0], strings.ToUpper(strings.Join(f[1:], " "))
			switch {
			case strings.HasPrefix(cmd, "SELECT"):
				fmt.Fprintf(srv, "* OK [UIDVALIDITY 42] ok\r\n%s OK [READ-WRITE] done\r\n", tag)
			case strings.HasPrefix(cmd, "UID SEARCH"):
				fmt.Fprintf(srv, "* SEARCH 7\r\n%s OK done\r\n", tag)
			case strings.HasPrefix(cmd, "UID FETCH"):
				fmt.Fprintf(srv, "* 1 FETCH (UID 7 BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(mailBody), mailBody, tag)
			default:
				fmt.Fprintf(srv, "%s OK\r\n", tag)
			}
		}
	}()
	ic := &imapConn{c: cli, r: bufio.NewReader(cli)}
	if err := m.voiceMailFetch(ic, "INBOX"); err != nil {
		t.Fatal(err)
	}
	fh := getFritzHost(hc.ID)
	fh.mu.Lock()
	n := len(fh.voice)
	fh.mu.Unlock()
	if n != 2 {
		t.Fatalf("erwartet 2 übernommene Gespräche, %d", n)
	}
	v := m.voiceMailView()
	if len(v.Unassigned) != 1 || numDigits(v.Unassigned[0].Number) != "07749999999" || v.Unassigned[0].MyFritz != "abcdef123456.myfritz.net" {
		t.Fatalf("nicht zugeordnet falsch: %+v", v.Unassigned)
	}
	// zweiter Abruf: Mail schon verarbeitet
	if err := m.voiceMailFetch(ic, "INBOX"); err != nil {
		t.Fatal(err)
	}
	// Zuordnen übernimmt die gesammelten Gespräche
	if k, err := m.voiceAssign(hc, "07749999999"); err != nil || k != 1 {
		t.Fatalf("Zuordnen: %d %v", k, err)
	}
	if v := m.voiceMailView(); len(v.Unassigned) != 0 {
		t.Fatalf("noch nicht zugeordnet: %+v", v.Unassigned)
	}
}

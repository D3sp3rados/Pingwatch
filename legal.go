package main

import (
	"html/template"
	"net/http"
)

// Impressum und Datenschutzerklärung – öffentlich unter /impressum und /datenschutz.
// Anbieterdaten über Umgebungsvariablen änderbar (LEGAL_*).

type legalInfo struct {
	Name, Owner, Street, City, Phone, Email, Tax, Host, HostCity, Authority, Updated string
}

func legal() legalInfo {
	return legalInfo{
		Name:      envOr("LEGAL_NAME", "Raphael Wagner ICT-Dienstleistungen"),
		Owner:     envOr("LEGAL_OWNER", "Raphael Wagner"),
		Street:    envOr("LEGAL_STREET", "Rüttehof 2"),
		City:      envOr("LEGAL_CITY", "79804 Dogern"),
		Phone:     envOr("LEGAL_PHONE", "+49 172 4147503"),
		Email:     envOr("LEGAL_EMAIL", "info@ict-dienstleistungen.de"),
		Tax:       envOr("LEGAL_TAX", "Steuernummer 20356/10194 (Finanzamt Waldshut-Tiengen)"),
		Host:      envOr("LEGAL_HOST", "RST-Datentechnik GmbH, Albbruck"),
		HostCity:  envOr("LEGAL_HOST_LOCATION", "Rechenzentrum in Frankfurt am Main, Deutschland"),
		Authority: envOr("LEGAL_AUTHORITY", "Der Landesbeauftragte für den Datenschutz und die Informationsfreiheit Baden-Württemberg, Lautenschlagerstraße 20, 70173 Stuttgart"),
		Updated:   "Oktober 2026",
	}
}

const legalHead = `<!doctype html><html lang="de"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.T}} – PingWacht</title><link rel="icon" href="/favicon.ico"><style>
:root{--bg:#f3f5f9;--card:#fff;--fg:#0f172a;--mut:#64748b;--line:#e2e8f0;--acc:#059669}
@media (prefers-color-scheme:dark){:root{--bg:#0b1120;--card:#111a2e;--fg:#e2e8f0;--mut:#8b9ab4;--line:#1e2a44;--acc:#34d399}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
main{max-width:760px;margin:0 auto;background:var(--card);border:1px solid var(--line);border-radius:16px;padding:28px}
h1{font-size:24px;margin:0 0 4px}h2{font-size:17px;margin:26px 0 6px}p,li{color:var(--fg)}.mut{color:var(--mut);font-size:13px}
a{color:var(--acc)}.brand{display:flex;align-items:center;gap:9px;font-weight:700;font-size:17px;margin:0 auto 14px;max-width:760px;text-decoration:none;color:var(--fg)}
.brand img{width:28px;height:28px;border-radius:7px}nav{max-width:760px;margin:14px auto 0;font-size:13px;color:var(--mut)}nav a{margin-right:14px}
</style></head><body><a class="brand" href="/"><img src="/icon.svg" alt=""><span>Ping<span style="color:var(--acc)">Wacht</span></span></a><main>`

const legalFoot = `</main><nav><a href="/">Zur App</a><a href="/impressum">Impressum</a><a href="/datenschutz">Datenschutz</a></nav></body></html>`

var impressumTmpl = template.Must(template.New("i").Parse(legalHead + `
<h1>Impressum</h1>
<p class="mut">Angaben gemäß § 5 Digitale-Dienste-Gesetz (DDG)</p>
<p><b>{{.L.Name}}</b><br>Inhaber: {{.L.Owner}}<br>{{.L.Street}}<br>{{.L.City}}<br>Deutschland</p>
<h2>Kontakt</h2>
<p>Telefon: <a href="tel:{{.L.Phone}}">{{.L.Phone}}</a><br>E-Mail: <a href="mailto:{{.L.Email}}">{{.L.Email}}</a></p>
{{if .L.Tax}}<h2>Steuerliche Angaben</h2><p>{{.L.Tax}}</p>{{end}}
<h2>Verantwortlich für den Inhalt nach § 18 Abs. 2 MStV</h2>
<p>{{.L.Owner}}, {{.L.Street}}, {{.L.City}}</p>
<h2>Betrieb und Hosting</h2>
<p>PingWacht wird auf Servern der {{.L.Host}} betrieben – {{.L.HostCity}}.</p>
<h2>Verbraucherstreitbeilegung</h2>
<p>Wir sind nicht bereit und nicht verpflichtet, an Streitbeilegungsverfahren vor einer Verbraucherschlichtungsstelle teilzunehmen.</p>
<h2>Haftung für Inhalte und Links</h2>
<p>Die Inhalte dieser Anwendung werden mit größter Sorgfalt erstellt. Für die Richtigkeit, Vollständigkeit und Aktualität können wir jedoch keine Gewähr übernehmen. Für Inhalte externer Links sind ausschließlich deren Betreiber verantwortlich.</p>
<p class="mut">Stand: {{.L.Updated}}</p>
` + legalFoot))

var datenschutzTmpl = template.Must(template.New("d").Parse(legalHead + `
<h1>Datenschutzerklärung</h1>
<p class="mut">für die Netzwerk- und Geräteüberwachung PingWacht (Web-App, Android-/iPhone-App und Agenten)</p>

<h2>1. Verantwortlicher</h2>
<p>{{.L.Name}}, Inhaber {{.L.Owner}}, {{.L.Street}}, {{.L.City}}<br>E-Mail: <a href="mailto:{{.L.Email}}">{{.L.Email}}</a> · Telefon: {{.L.Phone}}</p>

<h2>2. Hosting – Daten bleiben in Deutschland</h2>
<p>PingWacht läuft ausschließlich auf eigener Infrastruktur der {{.L.Host}} im {{.L.HostCity}}. Datenbank und Anwendung liegen auf diesen Servern; Zugriff haben nur der Verantwortliche und die mit dem Betrieb betrauten Mitarbeiter der {{.L.Host}}. Mit dem Hoster besteht ein Vertrag zur Auftragsverarbeitung (Art. 28 DSGVO).</p>

<h2>3. Welche Daten wir verarbeiten</h2>
<ul>
<li><b>Benutzerkonto:</b> Name, E-Mail-Adresse, Rolle, Passwort (nur als sicherer Hash gespeichert), Zeitpunkt der letzten Anmeldung.</li>
<li><b>Überwachte Geräte und Dienste:</b> Bezeichnungen, IP-Adressen bzw. Hostnamen und Webadressen, die Sie selbst eintragen, sowie die Messwerte (Antwortzeiten, Ausfälle, Ereignisse).</li>
<li><b>Agenten vor Ort:</b> öffentliche IP-Adresse des Standorts, Agent-Version, bei aktiviertem UniFi-Livestatus Gerätenamen, Modelle, MAC- und IP-Adressen, Laufzeit und Auslastung der UniFi-Geräte. Der UniFi-API-Schlüssel wird nur für diesen Zweck gespeichert.</li>
<li><b>Benachrichtigungen:</b> Empfänger-Adressen der eingerichteten Alarmwege (E-Mail, Teams-, Telegram- oder Webhook-Ziele) und Gerätekennungen für Push-Mitteilungen.</li>
<li><b>Technische Zugriffsdaten:</b> IP-Adresse, Zeitpunkt und aufgerufene Adresse in den Server-Protokollen (zur Fehlersuche und Abwehr von Angriffen).</li>
</ul>

<h2>4. Zwecke und Rechtsgrundlagen</h2>
<p>Wir verarbeiten die Daten, um die vereinbarte Überwachung zu erbringen, Sie bei Störungen zu benachrichtigen und Berichte zu erstellen (Art. 6 Abs. 1 lit. b DSGVO – Vertrag). Server-Protokolle verarbeiten wir zur Sicherheit und Stabilität des Dienstes (Art. 6 Abs. 1 lit. f DSGVO – berechtigtes Interesse). Soweit Sie für Ihre Kunden Geräte überwachen, verarbeiten wir deren Daten in Ihrem Auftrag (Art. 28 DSGVO); einen Vertrag zur Auftragsverarbeitung stellen wir auf Anfrage bereit.</p>

<h2>5. Cookies und lokale Speicherung</h2>
<p>PingWacht setzt nur ein technisch notwendiges Sitzungs-Cookie für die Anmeldung. Ansichtseinstellungen (z. B. Kacheln oder Liste) werden lokal in Ihrem Browser gespeichert. Es gibt kein Tracking, keine Werbung und keine Analyse-Dienste Dritter.</p>

<h2>6. Push-Mitteilungen</h2>
<p>Wenn Sie Benachrichtigungen auf einem Gerät aktivieren, wird die Mitteilung über den Push-Dienst des Geräteherstellers zugestellt: bei der iPhone-App über den Apple Push Notification Service (Apple Inc.), im Browser bzw. in der Android-App über den Push-Dienst Ihres Browsers (z. B. Google, Mozilla, Microsoft oder Apple). Dabei werden eine Gerätekennung und der Inhalt der Mitteilung übertragen; Browser-Mitteilungen sind Ende-zu-Ende-verschlüsselt. Soweit diese Anbieter Daten in den USA verarbeiten, stützen sie sich auf das EU-US Data Privacy Framework bzw. Standardvertragsklauseln. Sie können Push-Mitteilungen jederzeit in PingWacht oder in den Einstellungen Ihres Geräts abschalten.</p>

<h2>7. E-Mail und weitere Alarmwege</h2>
<p>E-Mails (Alarme, Einladungen, Monatsberichte) versenden wir über einen eigenen Mailserver in Deutschland. Richten Sie Alarmwege zu Microsoft Teams, Telegram oder eigenen Webhooks ein, werden die Meldungstexte an diese von Ihnen gewählten Dienste übermittelt; dafür gelten deren Datenschutzbestimmungen.</p>

<h2>8. Speicherdauer</h2>
<p>Messwerte und Ereignisse speichern wir entsprechend Ihres Pakets (je nach Paket 7 bis 365 Tage) und löschen sie danach automatisch. Kontodaten speichern wir, solange das Konto besteht; nach Kündigung werden sie gelöscht, soweit keine gesetzlichen Aufbewahrungspflichten (z. B. für Rechnungen) bestehen. Server-Protokolle werden nur kurzzeitig vorgehalten und laufend überschrieben.</p>

<h2>9. Empfänger</h2>
<p>Eine Weitergabe an Dritte erfolgt nur an den Hoster (Abschnitt 2), an die Push-Dienste (Abschnitt 6) und an die von Ihnen eingerichteten Alarmwege (Abschnitt 7) oder wenn wir gesetzlich dazu verpflichtet sind. Ein Verkauf von Daten findet nicht statt.</p>

<h2>10. Ihre Rechte</h2>
<p>Sie haben das Recht auf Auskunft (Art. 15), Berichtigung (Art. 16), Löschung (Art. 17), Einschränkung der Verarbeitung (Art. 18), Datenübertragbarkeit (Art. 20) und Widerspruch (Art. 21 DSGVO). Wenden Sie sich dazu an <a href="mailto:{{.L.Email}}">{{.L.Email}}</a>. Sie können sich außerdem bei einer Datenschutz-Aufsichtsbehörde beschweren, z. B. bei: {{.L.Authority}}.</p>

<h2>11. Sicherheit</h2>
<p>Alle Verbindungen sind TLS-verschlüsselt. Passwörter werden nur als Hash gespeichert, Agenten melden sich mit einem eigenen Schlüssel je Standort an, Agent-Updates werden über eine Prüfsumme abgesichert.</p>

<p class="mut">Stand: {{.L.Updated}}</p>
` + legalFoot))

func (s *Server) handleLegal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{"L": legal()}
	if r.URL.Path == "/datenschutz" {
		data["T"] = "Datenschutz"
		datenschutzTmpl.Execute(w, data)
		return
	}
	data["T"] = "Impressum"
	impressumTmpl.Execute(w, data)
}

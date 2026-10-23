package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Nutzungsbedingungen (Entwurf – vor dem Einsatz bei Kunden anwaltlich prüfen lassen) und Zustimmungen:
//   - bei der Registrierung und bei jeder neuen Fassung einmal beim Anmelden
//   - einmal je Konto beim ersten Einschalten von „Updates, Neustarts und Scripts“

const termsVersion = "2026-10-24"

// actionsConsentText: Text, dem beim ersten Einschalten der Aktionen zugestimmt wird.
const actionsConsentText = `Mit dem Einschalten können Inhaber und Admins dieses Kontos über PingWacht auf den Rechnern des Standorts Betriebssystem-Updates installieren, Neustarts auslösen und Scripts aus der eigenen Script-Bibliothek ausführen – sofort oder per Plan.

• Diese Aktionen werden ausschließlich von Ihnen bzw. Ihren Benutzern ausgelöst oder geplant. Sie sind dafür verantwortlich, dass Updates, Neustarts und Scripts für die jeweiligen Systeme geeignet sind und zu einem passenden Zeitpunkt laufen.
• Für Folgen von Updates, Neustarts oder Scripts (z. B. Ausfall von Anwendungen, Datenverlust) haftet der Anbieter nur nach Abschnitt 7 der Nutzungsbedingungen, also insbesondere nicht bei einfacher Fahrlässigkeit über den vorhersehbaren, vertragstypischen Schaden hinaus.
• Für kritische Infrastrukturen (KRITIS), medizinische oder sicherheitsrelevante Steuerungen sind diese Funktionen nicht bestimmt.
• Halten Sie aktuelle Datensicherungen vor. PingWacht ersetzt keine Datensicherung.
• Auf einzelnen Rechnern lassen sich die Aktionen mit "noActions": true in der agent.json dauerhaft abschalten.`

type Consent struct {
	Ver string `json:"ver"`
	At  int64  `json:"at"`
	By  string `json:"by"`
	IP  string `json:"ip,omitempty"`
}

// termsSummary: Kurzfassung der AGB für die KI-Vorprüfung von Änderungswünschen.
const termsSummary = `Nur für Unternehmer, nicht für Verbraucher. Leistung: Überwachung nach bestem Bemühen, keine Garantie lückenloser Erkennung/Zustellung. Kunde muss zur Überwachung der eingetragenen Systeme berechtigt sein und Betroffene informieren. Keine Überwachung oder Scans fremder Systeme ohne Berechtigung, keine Angriffe, kein Umgehen von Sicherheitsmaßnahmen, keine Überlastung. Aktionen auf Rechnern (Updates, Neustarts, Scripts) nur vom Kunden ausgelöst, mit Zwei-Faktor-Code, nicht für KRITIS/Medizin/Sicherheitssteuerungen. Kein Bildschirm-Fernzugriff, keine Aufzeichnung von Kommunikationsinhalten. Gespeicherte Zugangsdaten nur verschlüsselt und nur für den jeweiligen Zweck. Datenverarbeitung in Deutschland, Auftragsverarbeitung nach Art. 28 DSGVO, keine Weitergabe an Dritte außer genannten Unterauftragnehmern. Strikte Trennung der Kundenkonten. Änderungswünsche: kein Anspruch auf Umsetzung, Entscheidung beim Anbieter, umgesetzte Funktionen stehen allen Kunden zur Verfügung, Rechte beim Anbieter, Individualentwicklung nur nach gesondertem Angebot. KI-Assistent: Antworten können fehlerhaft sein, keine Rechtsberatung. Haftung bei einfacher Fahrlässigkeit begrenzt auf vertragstypischen Schaden, max. 12 Monatsvergütungen. Monatliche Laufzeit, Kündigung zum Ende des Abrechnungszeitraums, Daten-Export und Löschung nach Vertragsende.`

var termsTmpl = template.Must(template.New("t").Parse(legalHead + `
<h1>Allgemeine Geschäftsbedingungen</h1>
<p class="mut">Nutzungsbedingungen für PingWacht · Fassung {{.V}} · Anbieter: {{.L.Name}}, {{.L.Street}}, {{.L.City}}</p>

<h2>1. Geltungsbereich</h2>
<p>Diese Allgemeinen Geschäftsbedingungen (AGB) gelten für alle Verträge über die Nutzung von PingWacht (Web-Oberfläche, Apps, Agent und zugehörige Schnittstellen) zwischen der {{.L.Name}} („Anbieter“) und ihren Kunden. PingWacht richtet sich ausschließlich an Unternehmer im Sinne von § 14 BGB, juristische Personen des öffentlichen Rechts und öffentlich-rechtliche Sondervermögen, nicht an Verbraucher. Abweichende Bedingungen des Kunden gelten nur, wenn der Anbieter ihnen ausdrücklich in Textform zustimmt.</p>

<h2>2. Vertragsschluss und Testphase</h2>
<p>Der Vertrag kommt mit der Registrierung bzw. der Bestätigung eines Angebots durch den Anbieter zustande. Neue Konten können PingWacht für einen begrenzten Zeitraum kostenlos mit erweitertem Funktionsumfang testen. Nach Ende der Testphase läuft das Konto im kostenlosen Paket weiter, sofern der Kunde kein kostenpflichtiges Paket wählt; Hosts über dem Umfang des Pakets werden pausiert, Daten werden dabei nicht gelöscht.</p>

<h2>3. Leistung</h2>
<p>PingWacht überwacht die Erreichbarkeit von Geräten, Diensten und Webseiten, sammelt Messwerte, stellt sie dar und meldet Auffälligkeiten über die vom Kunden eingerichteten Kanäle. Der jeweilige Funktionsumfang ergibt sich aus dem gewählten Paket und der Beschreibung in der Anwendung. Die Überwachung erfolgt nach bestem Bemühen; eine lückenlose Erkennung oder Zustellung jeder Störung kann technisch nicht garantiert werden, etwa bei Ausfällen von Internetverbindungen, Push-, E-Mail- oder Cloud-Diensten Dritter. PingWacht ersetzt keine Datensicherung und keine Notfallplanung des Kunden.</p>
<p>Der Anbieter darf PingWacht weiterentwickeln und Funktionen ändern, soweit der vertragliche Kern der Leistung erhalten bleibt und die Änderung für den Kunden zumutbar ist.</p>

<h2>4. Preise und Zahlung</h2>
<p>Es gelten die bei Vertragsschluss in der Anwendung („Abo“) bzw. im Angebot genannten Preise. Alle Preise verstehen sich netto zuzüglich der gesetzlichen Umsatzsteuer. Die Vergütung ist im Voraus für den jeweiligen Abrechnungszeitraum fällig und wird online (z. B. über Stripe) eingezogen oder per Rechnung gestellt. Rechnungen sind innerhalb von 14 Tagen ohne Abzug zahlbar. Bei Zahlungsverzug kann der Anbieter den Zugang nach vorheriger Ankündigung in Textform auf das kostenlose Paket beschränken. Preisänderungen kündigt der Anbieter mindestens sechs Wochen vorher in Textform an; der Kunde kann dann zum Wirksamwerden der Änderung kündigen.</p>

<h2>5. Pflichten des Kunden und zulässige Nutzung</h2>
<p>Der Kunde hält seine Zugangsdaten geheim, nutzt die Zwei-Faktor-Anmeldung, wenn er Aktionen (Abschnitt 6) verwendet, und benachrichtigt den Anbieter unverzüglich bei Verdacht auf Missbrauch. Er ist für die Benutzer verantwortlich, die er in sein Konto einlädt, und für die Rechte, die er ihnen gibt.</p>
<p>Der Kunde darf nur Systeme überwachen, scannen oder abfragen, zu deren Überwachung er berechtigt ist, und informiert Betroffene (z. B. Mitarbeitende, Endkunden) soweit erforderlich. Unzulässig ist insbesondere: das Überwachen oder Scannen fremder Systeme ohne Erlaubnis, Angriffe oder Lasttests gegen Dritte, das Umgehen von Sicherheitsmaßnahmen, eine Nutzung, die PingWacht oder andere Kunden beeinträchtigt, sowie das Hinterlegen rechtswidriger Inhalte. Der Anbieter darf den Zugang bei einem schwerwiegenden Verstoß vorübergehend sperren; er informiert den Kunden unverzüglich.</p>

<h2>6. Aktionen auf Rechnern (Updates, Neustarts, Scripts)</h2>
<p>Aktionen sind standardmäßig ausgeschaltet und werden vom Kunden je Standort eingeschaltet. Sie werden ausschließlich vom Kunden bzw. dessen Benutzern ausgelöst oder geplant und sind durch einen Zwei-Faktor-Code abgesichert. Der Kunde ist für Inhalt, Eignung und Zeitpunkt der Aktionen verantwortlich, insbesondere für selbst erstellte Scripts. Jede Aktion wird protokolliert. Die Funktionen sind nicht für kritische Infrastrukturen, medizinische Geräte oder sicherheitsrelevante Steuerungen bestimmt. Ein Fernzugriff auf Bildschirme und eine Aufzeichnung von Kommunikationsinhalten finden nicht statt.</p>

<h2>7. Daten, Datenschutz und Zugangsdaten</h2>
<p>Es gilt die <a href="/datenschutz">Datenschutzerklärung</a>. Die Daten werden auf Servern des Anbieters in Deutschland verarbeitet; die Konten verschiedener Kunden sind strikt voneinander getrennt. Soweit der Anbieter personenbezogene Daten im Auftrag verarbeitet, schließen die Parteien einen Vertrag zur Auftragsverarbeitung nach Art. 28 DSGVO. Zugangsdaten, die der Kunde für Prüfungen hinterlegt (z. B. für UniFi, Fritzbox oder SNMP), werden nur für den jeweiligen Zweck verwendet und verschlüsselt bzw. zugriffsgeschützt gespeichert.</p>
<p>Nach Vertragsende kann der Kunde seine Daten innerhalb von 30 Tagen exportieren. Danach werden sie gelöscht, soweit keine gesetzlichen Aufbewahrungspflichten bestehen.</p>

<h2>8. KI-Assistent</h2>
<p>PingWacht bietet je nach Paket einen KI-Assistenten, der auf Servern in Deutschland läuft. Antworten werden automatisch erzeugt und können unvollständig oder fehlerhaft sein; sie sind keine Rechts-, Steuer- oder sonstige Fachberatung. Der Assistent sieht nur Daten des eigenen Kontos und ändert keine Einstellungen.</p>

<h2>9. Änderungswünsche</h2>
<p>Kunden können über den Assistenten oder direkt beim Anbieter Wünsche für neue Funktionen oder Änderungen einreichen. Der Anbieter prüft Wünsche unter anderem auf Machbarkeit, Verträglichkeit mit bestehenden Funktionen, Vereinbarkeit mit diesen AGB und dem Datenschutz und entscheidet nach eigenem Ermessen, ob und wann er sie umsetzt. Ein Anspruch auf Umsetzung besteht nicht. Umgesetzte Funktionen werden Teil von PingWacht und können allen Kunden zur Verfügung gestellt werden; alle Rechte daran liegen beim Anbieter. Eine Individualentwicklung ausschließlich für einen Kunden erfolgt nur nach gesondertem Angebot.</p>

<h2>10. Verfügbarkeit und Wartung</h2>
<p>Der Anbieter bemüht sich um eine hohe Verfügbarkeit. Wartungsarbeiten werden nach Möglichkeit angekündigt und in nutzungsarme Zeiten gelegt. Ein Anspruch auf eine bestimmte Verfügbarkeit besteht nur, wenn sie gesondert vereinbart ist.</p>

<h2>11. Nutzungsrechte</h2>
<p>Der Kunde erhält für die Vertragslaufzeit das nicht ausschließliche, nicht übertragbare Recht, PingWacht im Rahmen dieser AGB für eigene Zwecke und – soweit im Paket vorgesehen – für die Betreuung seiner eigenen Kunden zu nutzen. Der Agent darf nur zusammen mit PingWacht verwendet werden.</p>

<h2>12. Haftung</h2>
<p>Der Anbieter haftet unbeschränkt bei Vorsatz und grober Fahrlässigkeit, bei Verletzung von Leben, Körper oder Gesundheit, bei Übernahme einer Garantie sowie nach dem Produkthaftungsgesetz. Bei einfacher Fahrlässigkeit haftet der Anbieter nur bei Verletzung einer wesentlichen Vertragspflicht und begrenzt auf den vorhersehbaren, vertragstypischen Schaden, höchstens auf die vom Kunden in den letzten zwölf Monaten gezahlte Vergütung. Im Übrigen ist die Haftung ausgeschlossen. Die Haftung für Datenverlust ist auf den Aufwand beschränkt, der bei ordnungsgemäßer Datensicherung durch den Kunden zur Wiederherstellung erforderlich wäre. Für Schäden aus Aktionen nach Abschnitt 6, die der Kunde ausgelöst oder geplant hat, sowie aus nicht oder verspätet zugestellten Meldungen haftet der Anbieter nur nach diesem Abschnitt.</p>

<h2>13. Laufzeit und Kündigung</h2>
<p>Kostenpflichtige Pakete laufen je nach Wahl monatlich oder jährlich und verlängern sich jeweils um denselben Zeitraum, wenn sie nicht bis zum Ende des Abrechnungszeitraums gekündigt werden, soweit nichts anderes vereinbart ist. Ein Wechsel in ein anderes Paket ist jederzeit möglich. Das Recht zur außerordentlichen Kündigung aus wichtigem Grund bleibt unberührt. Kündigungen bedürfen der Textform oder erfolgen in der Anwendung.</p>

<h2>14. Änderungen dieser AGB</h2>
<p>Der Anbieter kann diese AGB mit Wirkung für die Zukunft ändern, wenn dies aus rechtlichen, technischen oder wirtschaftlichen Gründen erforderlich ist und den Kunden nicht unangemessen benachteiligt. Neue Fassungen werden beim nächsten Anmelden angezeigt und gelten mit Zustimmung des Kunden. Stimmt der Kunde nicht zu, kann er den Vertrag zum Wirksamwerden der Änderung kündigen.</p>

<h2>15. Schlussbestimmungen</h2>
<p>Es gilt das Recht der Bundesrepublik Deutschland unter Ausschluss des UN-Kaufrechts. Gerichtsstand ist, soweit zulässig, der Sitz des Anbieters. Sollte eine Bestimmung dieser AGB unwirksam sein, bleibt die Wirksamkeit der übrigen Bestimmungen unberührt.</p>

<p class="mut">Stand: {{.V}}</p>
` + legalFoot))

func (s *Server) handleTerms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	termsTmpl.Execute(w, map[string]any{"L": legal(), "T": "Nutzungsbedingungen", "V": termsVersion})
}

// POST /api/terms/accept → Zustimmung zur aktuellen Fassung
func (s *Server) handleTermsAccept(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.User == nil {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	s.m.mu.Lock()
	for i := range s.m.cfg.Users {
		if s.m.cfg.Users[i].ID == p.User.ID {
			s.m.cfg.Users[i].Terms = &Consent{Ver: termsVersion, At: time.Now().UnixMilli(), By: p.User.Email, IP: clientIP(r)}
		}
	}
	s.m.saveConfigLocked()
	s.m.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/actions/consent → {needed, text} · POST {accept: true}
func (s *Server) handleActionsConsent(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	o := s.m.orgByID(p.Org)
	if o == nil {
		http.Error(w, "Konto unbekannt", 404)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"needed": o.ActionsConsent == nil, "text": actionsConsentText, "consent": o.ActionsConsent})
		return
	}
	if !p.isOwner() && !p.canEdit() {
		http.Error(w, "Nur Inhaber und Admins", 403)
		return
	}
	var req struct{ Accept bool }
	if !readJSON(w, r, &req) || !req.Accept {
		http.Error(w, "Bitte zustimmen", 400)
		return
	}
	by := "Betreiber"
	if p.User != nil {
		by = p.User.Email
	}
	c := &Consent{Ver: termsVersion, At: time.Now().UnixMilli(), By: by, IP: clientIP(r)}
	s.m.mu.Lock()
	for i := range s.m.cfg.Orgs {
		if s.m.cfg.Orgs[i].ID == p.Org && s.m.cfg.Orgs[i].ActionsConsent == nil {
			s.m.cfg.Orgs[i].ActionsConsent = c
		}
	}
	s.m.saveConfigLocked()
	s.m.mu.Unlock()
	log.Printf("Zustimmung Aktionen: Konto %s durch %s", p.Org, by)
	writeJSON(w, map[string]bool{"ok": true})
}

// termsMailText: Absatz für E-Mails nach Registrierung bzw. Abschluss eines Abos.
func termsMailText(base string) string {
	return "Es gelten unsere Nutzungsbedingungen (" + strings.TrimRight(base, "/") + "/nutzungsbedingungen) und die Datenschutzerklärung (" +
		strings.TrimRight(base, "/") + "/datenschutz). Kurz zusammengefasst: PingWacht überwacht nach bestem Bemühen, ersetzt keine Datensicherung, " +
		"und Aktionen auf Ihren Rechnern (Updates, Neustarts, Scripts) lösen Sie selbst aus und verantworten sie selbst. " +
		"Die Haftung ist bei einfacher Fahrlässigkeit begrenzt (Abschnitt 7).\n"
}

// publicBaseDefault: öffentliche Adresse ohne Anfrage (z. B. für E-Mails aus Webhooks).
func publicBaseDefault() string {
	if u := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"); u != "" {
		return u
	}
	return defaultPublicURL
}

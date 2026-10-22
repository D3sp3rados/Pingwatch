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

const termsVersion = "2026-10-23"

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

var termsTmpl = template.Must(template.New("t").Parse(legalHead + `
<h1>Nutzungsbedingungen</h1>
<p class="mut">Fassung {{.V}} · Anbieter: {{.L.Name}}, {{.L.Street}}, {{.L.City}}</p>

<h2>1. Geltungsbereich</h2>
<p>Diese Bedingungen gelten für die Nutzung von PingWacht (Web-Oberfläche, Apps und Agent) durch Unternehmen, Freiberufler und öffentliche Stellen. PingWacht richtet sich nicht an Verbraucher.</p>

<h2>2. Leistung</h2>
<p>PingWacht überwacht die Erreichbarkeit von Geräten und Diensten, sammelt Messwerte und meldet Auffälligkeiten. Je nach Paket und Einstellung kann der Agent außerdem auf Anforderung Updates installieren, Rechner neu starten und Scripts des Kunden ausführen (Abschnitt 5). Die Überwachung erfolgt nach bestem Bemühen; eine lückenlose Erkennung oder Zustellung jeder Störung kann technisch nicht garantiert werden (z. B. bei Ausfall von Internet, Push-, E-Mail- oder Cloud-Diensten Dritter).</p>

<h2>3. Pflichten des Kunden</h2>
<p>Der Kunde hält seine Zugangsdaten geheim, nutzt die Zwei-Faktor-Anmeldung, wenn er Aktionen (Abschnitt 5) verwendet, und benachrichtigt den Anbieter unverzüglich bei Verdacht auf Missbrauch. Er stellt sicher, dass er zur Überwachung der eingetragenen Systeme berechtigt ist und Betroffene (z. B. Mitarbeitende, Endkunden) soweit erforderlich informiert sind.</p>

<h2>4. Verfügbarkeit</h2>
<p>Der Anbieter bemüht sich um eine hohe Verfügbarkeit. Wartungsarbeiten werden nach Möglichkeit angekündigt. Ein Anspruch auf eine bestimmte Verfügbarkeit besteht nur, wenn sie gesondert vereinbart ist.</p>

<h2>5. Aktionen auf Rechnern (Updates, Neustarts, Scripts)</h2>
<p>Aktionen sind standardmäßig ausgeschaltet und werden vom Kunden je Standort eingeschaltet. Sie werden ausschließlich vom Kunden bzw. dessen Benutzern ausgelöst oder geplant und sind durch einen Zwei-Faktor-Code abgesichert. Der Kunde ist für Inhalt, Eignung und Zeitpunkt der Aktionen verantwortlich, insbesondere für selbst erstellte Scripts. Jede Aktion wird protokolliert. Die Funktionen sind nicht für kritische Infrastrukturen, medizinische Geräte oder sicherheitsrelevante Steuerungen bestimmt.</p>

<h2>6. Daten und Datenschutz</h2>
<p>Es gilt die <a href="/datenschutz">Datenschutzerklärung</a>. Soweit der Anbieter personenbezogene Daten im Auftrag verarbeitet, schließen die Parteien einen Vertrag zur Auftragsverarbeitung (Art. 28 DSGVO). PingWacht ersetzt keine Datensicherung; der Kunde hält eigene Sicherungen seiner Systeme vor.</p>

<h2>7. Haftung</h2>
<p>Der Anbieter haftet unbeschränkt bei Vorsatz und grober Fahrlässigkeit, bei Verletzung von Leben, Körper oder Gesundheit sowie nach dem Produkthaftungsgesetz. Bei einfacher Fahrlässigkeit haftet der Anbieter nur bei Verletzung einer wesentlichen Vertragspflicht und begrenzt auf den vorhersehbaren, vertragstypischen Schaden, höchstens auf die vom Kunden in den letzten zwölf Monaten gezahlte Vergütung. Im Übrigen ist die Haftung ausgeschlossen. Die Haftung für Datenverlust ist auf den Aufwand beschränkt, der bei ordnungsgemäßer Datensicherung durch den Kunden zur Wiederherstellung erforderlich wäre. Für Schäden aus Aktionen nach Abschnitt 5, die der Kunde ausgelöst oder geplant hat, haftet der Anbieter nur nach diesem Abschnitt.</p>

<h2>8. Laufzeit und Kündigung</h2>
<p>Kostenpflichtige Pakete laufen monatlich und sind zum Ende des Abrechnungszeitraums kündbar, soweit nichts anderes vereinbart ist. Die Testphase endet automatisch.</p>

<h2>9. Änderungen</h2>
<p>Der Anbieter kann diese Bedingungen mit Wirkung für die Zukunft ändern. Neue Fassungen werden beim nächsten Anmelden angezeigt und gelten mit Zustimmung des Kunden.</p>

<h2>10. Schlussbestimmungen</h2>
<p>Es gilt deutsches Recht. Gerichtsstand ist, soweit zulässig, der Sitz des Anbieters.</p>

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

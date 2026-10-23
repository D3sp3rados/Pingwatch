package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// KI-Assistent: beantwortet Fragen zu PingWacht und zum eigenen Konto und nimmt
// Änderungswünsche entgegen. Sprachmodell OpenAI-kompatibel (Standard: RST KI-Computing).
//
// Umgebung: AI_API_KEY (ohne = aus), AI_BASE_URL, AI_MODEL, AI_MODEL_<PAKET>, AI_REQUEST_TO.

func aiEnabled() bool { return strings.TrimSpace(os.Getenv("AI_API_KEY")) != "" }

func aiBaseURL() string {
	return strings.TrimRight(envOr("AI_BASE_URL", "https://api.rstcloud.de/v1"), "/")
}

// aiModel: Modell je Paket (AI_MODEL_PRO usw.), sonst AI_MODEL, sonst qwen3-8b.
func aiModel(plan string) string {
	if v := strings.TrimSpace(os.Getenv("AI_MODEL_" + strings.ToUpper(plan))); v != "" {
		return v
	}
	return envOr("AI_MODEL", "qwen3-8b")
}

// Fragen pro Kunde und Tag (0 = unbegrenzt)
func aiDailyLimit(plan string) int {
	switch plan {
	case "internal":
		return 0
	case "pro":
		return 200
	}
	return 50
}

type aiMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var aiHTTP = &http.Client{Timeout: 60 * time.Second}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThinking entfernt „lautes Denken“ von Qwen3 aus der Antwort.
func stripThinking(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	if i := strings.Index(s, "</think>"); i >= 0 { // Anfang fehlt
		s = s[i+len("</think>"):]
	}
	if i := strings.Index(s, "<think>"); i >= 0 { // nicht abgeschlossen
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// aiChat schickt einen Chat an das Sprachmodell und liefert die Antwort ohne Denk-Teil.
func aiChat(ctx context.Context, model string, msgs []aiMsg, maxTokens int) (string, error) {
	if !aiEnabled() {
		return "", errors.New("KI ist nicht eingerichtet")
	}
	if len(msgs) > 0 && msgs[0].Role == "system" && !strings.Contains(msgs[0].Content, "/no_think") {
		msgs[0].Content += "\n/no_think"
	}
	body, _ := json.Marshal(map[string]any{
		"model": model, "messages": msgs, "temperature": 0.3, "max_tokens": maxTokens,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", aiBaseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(os.Getenv("AI_API_KEY")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := aiHTTP.Do(req)
	if err != nil {
		return "", errors.New("KI-Dienst nicht erreichbar")
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("KI-Dienst antwortet mit HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || len(out.Choices) == 0 {
		return "", errors.New("KI-Antwort unlesbar")
	}
	return stripThinking(out.Choices[0].Message.Content), nil
}

// ---------- Tageszähler ----------

var aiCount = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

// aiTake zählt eine Frage; false, wenn das Tageslimit erreicht ist.
func aiTake(org, plan string, now time.Time) bool {
	lim := aiDailyLimit(plan)
	day := now.Format("2006-01-02")
	k := org + "|" + day
	aiCount.Lock()
	defer aiCount.Unlock()
	if len(aiCount.m) > 5000 { // alte Tage aufräumen
		for kk := range aiCount.m {
			if !strings.HasSuffix(kk, "|"+day) {
				delete(aiCount.m, kk)
			}
		}
	}
	if lim > 0 && aiCount.m[k] >= lim {
		return false
	}
	aiCount.m[k]++
	return true
}

// ---------- Preise (deterministisch, die KI rechnet nicht selbst) ----------

func euro(cents int) string {
	return fmt.Sprintf("%d,%02d €", cents/100, cents%100)
}

// planPrice: Monatspreis netto für n Standorte; ok=false, wenn das Paket so nicht buchbar ist.
func planPrice(p Plan, n int) (int, bool) {
	if p.MaxSites > 0 && n > p.MaxSites {
		return 0, false
	}
	if p.MinSites > 0 && n < p.MinSites {
		if p.ID == "partner" {
			return 0, false
		}
		n = p.MinSites
	}
	return p.PriceCents * n, true
}

func priceSheet(o *Org) string {
	var b strings.Builder
	pl := planByID(o.Plan)
	billed := o.siteLimit()
	switch {
	case o.Billing == "free" || pl.PriceCents == 0:
		fmt.Fprintf(&b, "Aktuelle Kosten dieses Kunden: 0,00 € pro Monat (Paket %s).\n", pl.Name)
	default:
		c, _ := planPrice(pl, billed)
		fmt.Fprintf(&b, "Aktuelle Kosten dieses Kunden: %s netto pro Monat (%d Standort(e) × %s, Paket %s), zzgl. MwSt.\n", euro(c), max(billed, 1), euro(pl.PriceCents), pl.Name)
	}
	b.WriteString("Pakete (Preis pro Standort und Monat, netto, monatlich kündbar):\n")
	for _, p := range publicPlans() {
		extra := ""
		if p.MinSites > 1 {
			extra = fmt.Sprintf(", erst ab %d Standorten", p.MinSites)
		}
		if p.MaxSites > 0 {
			extra = fmt.Sprintf(", höchstens %d Standort", p.MaxSites)
		}
		fmt.Fprintf(&b, "- %s: %s%s – %s\n", p.Name, euro(p.PriceCents), extra, p.Text)
	}
	b.WriteString("Monatskosten netto nach Anzahl Standorte (Basis / Pro):\n")
	bp, pp := planByID("basis"), planByID("pro")
	for n := 1; n <= 20; n++ {
		c1, _ := planPrice(bp, n)
		c2, _ := planPrice(pp, n)
		fmt.Fprintf(&b, "- %d: %s / %s\n", n, euro(c1), euro(c2))
	}
	if o.Plan == "partner" {
		fmt.Fprintf(&b, "Dieser Kunde hat Partnerkonditionen: %s pro Standort.\n", euro(planByID("partner").PriceCents))
	}
	b.WriteString("Für Systemhäuser und Partner gibt es Sonderkonditionen auf Anfrage beim Anbieter – nenne dafür keine Preise.\n")
	return b.String()
}

// ---------- Systemprompt ----------

const aiProductHelp = `PRODUKTHILFE PINGWACHT (nur das hier gilt – erfinde keine weiteren Funktionen):
Menü oben: Übersicht (#/), Ereignisse (#/events), Statistik (#/stats), Netzkarte (#/map), Installieren (#/install: Agenten für Windows/Mac/Linux/Home Assistant und Apps für iPhone/Android mit Anleitung), Einstellungen (#/settings, mit Reitern: Hosts & Gruppen, Standorte, Alarme & Wartung, Team, Statusseite, Mein Konto, System), Abo (#/billing). Oben rechts der Knopf „🔔 Meldungen“ zum Stummschalten.
- Übersicht: alle Hosts als Kacheln oder als Liste (Umschalter oben), Status, Antwortzeiten, Ausfälle.
- Host anlegen: Einstellungen → Hosts & Gruppen → „+ Host hinzufügen“. Name, Adresse (IP oder Hostname), Standort und Methode wählen. Methoden: ICMP (klassischer Ping), TCP-Verbindung (Port), Webseite HTTP/HTTPS (Pro), DNS-Auflösung (Pro), SNMP für Switches/Router (Pro). Viele Hosts auf einmal: „Liste importieren“ (Liste aus Excel/CSV einfügen).
- Standort „Cloud“: Prüfung aus dem Rechenzentrum, nur für öffentlich erreichbare Adressen und Webseiten. Kein Standort im Sinne der Abrechnung.
- Standort mit Agent: für lokale IPs (z. B. 192.168.x.x). Ein Standort = ein lokales Netz mit einem Agent. Einstellungen → Standorte → „+ Standort hinzufügen“, dann „agent.json“ und den Agent herunterladen.
  Windows: agent.json und pingwatch-agent.exe in denselben Ordner legen (z. B. C:\PingWacht) und starten; Symbol im Infobereich, startet automatisch mit Windows.
  Linux/Raspberry Pi: „pingwatch-agent-linux-arm64 agent -config agent.json“ (amd64-Variante für PCs). Mac: Mac-Agent aus derselben Zeile herunterladen.
  Home Assistant: Add-on „PingWacht Agent“ aus dem Repository https://github.com/D3sp3rados/Pingwatch hinzufügen und das Token aus der agent.json eintragen.
  Fällt das Internet am Standort aus, puffert der Agent die Messwerte und schickt sie nach; ist ein Agent länger weg, kommt die Meldung „Agent nicht erreichbar“. Agenten aktualisieren sich selbst (je Standort abschaltbar, Knopf „Jetzt aktualisieren“).
- Am Standort: „Netzwerk scannen“ findet Geräte im Netz, „UniFi-Import“ übernimmt UniFi-Geräte als Hosts.
- UniFi-Livestatus: Standort-Karte → „UniFi-Livestatus“: Adresse der UniFi-Konsole und API-Schlüssel (UniFi Network → Einstellungen → Control Plane → Integrationen → API-Schlüssel erstellen). Geräte werden jede Minute abgefragt, Alarm bei „offline“.
- UniFi ohne Agent (über die UniFi-Cloud): Einstellungen → Standorte → Bereich „UniFi-Cloud (ohne Agent)“: in unifi.ui.com einen API-Key erstellen (API / Site Manager API → „Create API Key“) und dort einfügen. Dann „+ UniFi-Cloud-Standort“ anlegen (oder bei einem Standort „UniFi-Livestatus“ → Quelle „Über die UniFi-Cloud“), „Konsolen laden“, Konsole wählen, „Verbindung testen“, speichern. PingWacht fragt die Konsole dann jede Minute über die Cloud von Ubiquiti ab (Geräte, Status, Laufzeit, Auslastung, Neustarts); WAN-Details des Gateways sind über die Cloud evtl. nicht verfügbar. Voraussetzung: Konsole ab Firmware 5.0.3, UniFi Network ab 9.3. Ist die Konsole über die Cloud nicht erreichbar (z. B. Internet beim Kunden weg), kommt nach 3 Minuten eine Meldung. Ping auf interne Geräte, Fritzbox und Rechner-Überwachung brauchen weiterhin einen Agenten vor Ort.
- Fritzbox (TR-064): Host mit der LAN-IP der Fritzbox (z. B. 192.168.178.1) an einem Agent-Standort anlegen, Methode ICMP oder TCP, Haken „Fritzbox auslesen (TR-064)“, Benutzer, Passwort, Port 49000. In der Fritzbox vorher: System → FRITZ!Box-Benutzer → Benutzer anlegen mit Recht „FRITZ!Box Einstellungen“; Heimnetz → Netzwerk → Netzwerkeinstellungen → „Zugriff für Anwendungen zulassen“. Dann zeigt PingWacht Internet-Status, externe IP, Sync-Rate, Abbrüche und Neustarts und meldet „Internet getrennt“.
- UniFi Protect: Kameras und NVR einer UniFi-Konsole werden mit angezeigt (je Standort im UniFi-Dialog abschaltbar); einzelne Geräte lassen sich per Klick pausieren (gelb).
- Rechner-Überwachung (ab Basis; Basis/Partner 5, Pro 25 Rechner je Standort): Einstellungen → Standorte → bei einem Standort „Rechner überwachen“ einschalten. Der Rechner mit dem Agent meldet dann jede Minute CPU, Arbeitsspeicher, Laufwerke, Laufzeit, Betriebssystem; unter Windows zusätzlich Ereignisprotokoll (kritische Einträge), ausstehende Windows-Updates, Virenschutz (Defender oder anderes Produkt) und ausstehenden Neustart; unter Linux Updates (apt) und Neustart. Dienste (z. B. „Spooler“, „MSSQLSERVER“, unter Linux „smbd“) im Feld „Dienste“ eintragen, eine pro Zeile. Schwellwerte (Laufwerk voll ab %, CPU/RAM dauerhaft hoch) dort einstellbar. Meldungen: Laufwerk fast voll, CPU/RAM dauerhaft hoch, Dienst gestoppt, Virenschutz aus/Signaturen veraltet, Neustart seit Tagen ausstehend, Rechner meldet sich nicht. Weitere Server/PCs im selben Netz: Installieren → Windows/Linux/Mac → „Weitere Rechner überwachen“: dort die agent.json „nur Rechner“ herunterladen und mit dem Agent auf den Rechner legen (Linux: Startparameter -sysonly). Diese Rechner prüfen keine Hosts und kosten keinen zusätzlichen Standort. Rechner erscheinen in der Übersicht bei ihrem Standort bzw. ihrer Gruppe; Klick zeigt Verlauf, Laufwerke, Dienste. Keine Fernsteuerung des Bildschirms, keine Softwareverteilung.
- Aktionen auf Rechnern (Updates, Neustarts, Scripts): je Standort einschaltbar (Standard aus, einmalige Zustimmung, Zwei-Faktor-Code nötig). Dann am Rechner: Windows-Updates installieren, Neustart, eigene Scripts aus der Script-Bibliothek ausführen – sofort oder geplant; jede Aktion wird protokolliert. Auf einem Rechner abschaltbar mit "noActions": true in der agent.json.
- Inventar: Rechner anklicken → Reiter „Inventar“: Hardware (Hersteller, Modell, Seriennummer, BIOS, CPU, RAM, Datenträger, Netzwerkadapter), Betriebssystem (Version, Build, Windows-Aktivierungsstatus, Domäne, angemeldeter Benutzer, Installationsdatum, letzter Neustart) und installierte Software mit Suche. Wird beim Start des Agenten und alle 24 Stunden ausgelesen, Knopf „Jetzt aktualisieren“. Änderungen (Software neu/entfernt/aktualisiert, Hardware) stehen im Änderungsprotokoll und als Ereignis „Inventar“. Export: „CSV (dieser Rechner)“, „Inventarliste aller Rechner (CSV)“, „Software aller Rechner (CSV)“. Lizenz-/Produktschlüssel werden nie ausgelesen.
- Selbstheilung (Dienste automatisch neu starten): Einstellungen → Standorte → „💻 Rechner-Überwachung“ → „Selbstheilung erlauben“ einschalten und bei einzelnen überwachten Diensten „automatisch neu starten“ anhaken (Standard: aus). Der Agent startet dann nur diese Dienste neu, wenn sie gestoppt sind: höchstens 3 Versuche im Abstand von 1 Minute, danach Meldung „Neustart fehlgeschlagen“. Erfolgreiche Neustarts erscheinen als Ereignis. Unter Windows braucht der Agent dafür Administratorrechte; auf dem Mac nicht möglich. Auf dem Rechner abschaltbar mit "noSelfHeal": true in der agent.json. Dienstnamen mit Sonderzeichen (z. B. $) können nicht automatisch neu gestartet werden.
- Gruppen (Kunden): Einstellungen → Hosts & Gruppen → Gruppen → „+ Gruppe“: Hosts eines Kunden zusammenfassen; je Gruppe Monatsbericht per E-Mail und Stummschalten möglich.
- Alarme (ab Basis): Einstellungen → Alarme & Wartung → „+ Alarmweg“: E-Mail, Microsoft Teams, Telegram oder eigener Webhook; auf bestimmte Kunden/Hosts begrenzbar; mit Test-Knopf.
- Push aufs Handy: iPhone-App oder im Browser/Android über „App installieren“ und Push einschalten. Einstellungen → Mein Konto → Benachrichtigungen aufs Handy, dort „Testnachricht senden“.
- Meine Benachrichtigungen (Einstellungen → Mein Konto): Push an/aus, E-Mail bei Ausfällen, Urlaubsmodus (Pause bis Datum), einzelne Kunden stummschalten.
- Eigenes Konto: Einstellungen → Mein Konto: Name und Login-E-Mail ändern (zur Bestätigung aktuelles Passwort), Passwort ändern (aktuelles + neues Passwort zweimal). Vorläufiges Passwort aus der Einladung bitte dort ändern.
- Stummschalten/Pausieren: „🔔 Meldungen“ oben oder „⏸ Pause“ in der Hosts-Tabelle: alles, ein Kunde oder ein Host, für einen Zeitraum oder dauerhaft. Pausierte Hosts werden nicht geprüft.
- Wartungsfenster: Einstellungen → Alarme & Wartung → Wartungsfenster → „+ Wartung planen“ (einmalig oder wöchentlich): keine Alarme, zählt nicht in die Verfügbarkeit.
- Öffentliche Statusseite (Pro): Einstellungen → Statusseite (nur Name und Status, keine IP-Adressen).
- Monatsbericht PDF (Pro, Partner): Statistik → Monatsbericht; automatisch am 1. per E-Mail über einen Alarmweg mit Haken „Monatsbericht“ oder je Gruppe.
- Team: Einstellungen → Team → „+ Benutzer einladen“. Rollen: Inhaber (alles inkl. Abo), Admin (Hosts, Standorte, Benutzer), Nur lesen.
- Rechte: Beim Einladen (Einstellungen → Team) „Nur ausgewählte Kunden / Hosts“ wählen und Kunden, Standorte, Gruppen oder einzelne Hosts zuweisen – jeweils nur lesen oder Admin. Ein Benutzer kann Mitglied in mehreren Konten sein und oben zwischen ihnen umschalten.
- Netzkarte (#/map): Geräte und Verbindungen wie im MikroTik Dude, Import aus dem Dude (JSON). Erreichbarkeit aller Geräte alle 5 Minuten über einen Agenten („Erreichbarkeit über …“): grün = erreichbar, rot = nicht erreichbar, grau = Ping aus. Suche mit Trefferliste und Schnellfiltern, Orte (📍 Orte: umbenennen, zusammenlegen, Geräte zuweisen), Mehrfachauswahl mit Strg + Ziehen, Rechtsklick-Menü (verbinden, Linientyp, Linie löschen, Ping aus, Ort, Name/IP), Geräte mit der Maus verschieben, doppelte IPs/Namen anzeigen, Statistik je Gerät (Verfügbarkeit, Ping, Ausfälle, Traffic per SNMP über 📶 Traffic), Meldungen bei Ausfall eines Geräts, Sektors, Orts oder vieler Geräte (🔔 Meldungen). In der iPhone-App als Liste im Reiter „Netz“.
- Darstellung: Hell/Dunkel über den Knopf ◐ oben (folgt sonst dem System). Kacheln oder Liste in der Übersicht. Eigene Hintergründe, Farben, Logos oder Designs gibt es derzeit nicht.
- Abo: Menü „Abo“ zeigt Paket, Standorte und Preise.
- Testphase: Neu registrierte Konten nutzen 14 Tage alle Funktionen von Pro (ohne Kreditkarte). Danach läuft das Konto automatisch im kostenlosen Paket Free weiter (1 Standort, 5 Hosts); weitere Hosts werden pausiert, nichts wird gelöscht. Ein Paket wählt man unter „Abo“ oder beim Anbieter.
- Verlauf: Messwerte werden je nach Paket 7 bis 365 Tage aufbewahrt.`

const aiFeatureList = `PingWacht kann: Ping (ICMP), TCP-Port-, Webseiten- (HTTP/HTTPS inkl. Zertifikat), DNS- und SNMP-Prüfungen; Cloud-Prüfung und lokale Agenten (Windows, Linux/Raspberry Pi, Mac, Home-Assistant-Add-on) mit Pufferung und Selbst-Update; Netzwerk-Scan; UniFi-Import und UniFi-Livestatus; Fritzbox-Auslesung per TR-064 (Internet-Status, Sync, Abbrüche, Neustarts); Rechner-Überwachung über den Agenten (CPU, RAM, Laufwerke, Dienste, Ereignisprotokoll, Windows-Updates, Virenschutz, ausstehender Neustart; weitere Rechner per Agent im Modus „nur Rechner“) – Messen, Inventar (Hardware, Betriebssystem, installierte Software, CSV-Export) und optional eng begrenzte Selbstheilung (nur ausdrücklich markierte Dienste neu starten); Aktionen auf Rechnern je Standort einschaltbar (Windows-Updates installieren, Neustarts, eigene Scripts – sofort oder geplant, mit Zwei-Faktor-Code); keine Fernsteuerung (Bildschirm)/Softwareverteilung; Netzkarte (Geräte und Verbindungen wie im MikroTik Dude, Import, Orte, Erreichbarkeit aller Geräte über einen Agenten, Statistik je Gerät mit Ping/Ausfällen/Traffic per SNMP, Meldungen bei Ausfall vieler Geräte); Rechteverwaltung (Teammitglieder nur für bestimmte Kunden/Standorte/Gruppen/Hosts, lesend oder Admin); Gruppen/Kunden; Alarme per E-Mail, Microsoft Teams, Telegram, Webhook; Push (iPhone-App, Web-Push/Android); persönliche Benachrichtigungen mit Urlaubsmodus; Stummschalten/Pausieren; Wartungsfenster; öffentliche Statusseite; Monatsbericht als PDF; Statistik und Heatmap; Traceroute; Listen-Import; Team mit Rollen; Mandanten (Kunden) mit Paketen und Abrechnungs-CSV. Daten liegen auf Servern der RST in Deutschland.`

func billingLabel(b string) string {
	switch b {
	case "stripe":
		return "online per Stripe"
	case "free":
		return "kostenlos"
	case "invoice":
		return "per Rechnung"
	}
	return "nicht festgelegt"
}

// aiContext beschreibt ausschließlich das eigene Konto (keine Zugangsdaten, keine IPs, keine fremden Kunden).
func (s *Server) aiContext(p *Principal, o *Org) string {
	m := s.m
	pl := planByID(o.Plan)
	usedSites, usedHosts := m.usage(o.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "Firma: %s\nPaket: %s\nAbrechnung: %s\n", o.Name, pl.Name, billingLabel(o.Billing))
	if l := o.siteLimit(); l > 0 {
		fmt.Fprintf(&b, "Standorte mit Agent: %d genutzt, %d gebucht/erlaubt\n", usedSites, l)
	} else {
		fmt.Fprintf(&b, "Standorte mit Agent: %d genutzt (unbegrenzt)\n", usedSites)
	}
	if l := o.hostLimit(); l > 0 {
		fmt.Fprintf(&b, "Hosts: %d von %d\n", usedHosts, l)
	} else {
		fmt.Fprintf(&b, "Hosts: %d (unbegrenzt)\n", usedHosts)
	}
	m.mu.RLock()
	var sites []SiteCfg
	for _, sc := range m.cfg.Sites {
		if sc.Org == o.ID {
			sites = append(sites, sc)
		}
	}
	m.mu.RUnlock()
	if len(sites) == 0 {
		b.WriteString("Agenten: noch keine\n")
	} else {
		b.WriteString("Agenten:\n")
		for _, sc := range sites {
			on, st := m.siteOnline(sc.ID)
			ver := ""
			if st != nil {
				st.mu.Lock()
				ver = st.version
				st.mu.Unlock()
			}
			state := "offline"
			if on {
				state = "online"
			}
			if ver == "" {
				ver = "unbekannt"
			}
			fmt.Fprintf(&b, "- %s: %s, Version %s\n", sc.Name, state, ver)
		}
	}
	down, paused := 0, 0
	for _, h := range m.hostsOf(o.ID) {
		h.mu.Lock()
		if h.cfg.Paused {
			paused++
		} else if h.lastT > 0 && h.consec >= h.cfg.OutageCount {
			down++
		}
		h.mu.Unlock()
	}
	fmt.Fprintf(&b, "Hosts aktuell gestört: %d, pausiert: %d\n", down, paused)
	role := map[string]string{"owner": "Inhaber", "admin": "Admin", "viewer": "Nur lesen"}[p.Role]
	if p.Super {
		role = "Betreiber"
	}
	if role != "" {
		fmt.Fprintf(&b, "Rolle der fragenden Person: %s\n", role)
	}
	return b.String()
}

func (s *Server) aiSystemPrompt(p *Principal, o *Org) string {
	pv := providerInfo()
	support := pv["name"]
	if pv["email"] != "" {
		support += " (" + pv["email"] + ")"
	}
	var b strings.Builder
	b.WriteString(`Du bist der Assistent von PingWacht, einer Überwachung für Netzwerke, Geräte und Webseiten. Anbieter: ` + support + `.
Regeln:
- Antworte auf Deutsch in der Sie-Form, kurz und konkret (meist 2–8 Sätze oder eine kurze Liste).
- Beantworte nur Fragen zu PingWacht und zu diesem Konto. Bei anderen Themen freundlich ablehnen.
- Erfinde nichts. Die PRODUKTHILFE unten ist vollständig: Was dort steht, gibt es – erkläre dann genau, wo. Was dort NICHT steht, gibt es (noch) nicht – sag das klar („Das gibt es derzeit nicht.“). Prüfe jede Frage gegen die Produkthilfe. Keine ausweichenden Formulierungen wie „sofern verfügbar“, „eventuell in den Einstellungen“ oder „falls vorhanden“.
- Gibt es etwas nicht, biete sofort an, es als Wunsch aufzunehmen (siehe ÄNDERUNGSWÜNSCHE). Verweise für Funktionen und Anpassungen NIE an den Anbieter oder eine E-Mail-Adresse – genau dafür ist der Assistent da. Den Anbieter ` + support + ` nennst du nur bei Vertrags-, Rechnungs- oder Kontoproblemen, die du nicht beantworten kannst.
- Einstellungen am Konto änderst du nicht selbst: Erkläre, wo man es macht, und verlinke nur so: [Einstellungen](#/settings), [Abo](#/billing), [Statistik](#/stats), [Ereignisse](#/events), [Übersicht](#/).
- Frage nie nach Passwörtern oder Schlüsseln.
- Preise: Verwende ausschließlich die Zahlen aus PREISE unten, rechne nicht selbst. Alle Preise netto zzgl. MwSt.
- Formatierung: nur **fett**, Listen mit „- “ und Zeilenumbrüche.
ÄNDERUNGSWÜNSCHE (neue Funktionen, Anpassungen): Nimm Wünsche gerne und positiv an. Sage NIE „Ich kann nichts einbauen“ oder Ähnliches und verweise nicht pauschal an den Anbieter – du nimmst den Wunsch selbst auf, er wird anschließend geprüft.
1. Prüfe anhand der Produkthilfe, ob es das schon gibt. Wenn ja: erkläre, wo und wie.
2. Wenn nicht oder wenn noch unklar ist, was gemeint ist (z. B. „kannst du was einbauen?“): frage freundlich und konkret nach – Was genau soll es tun? Wo in PingWacht (z. B. Übersicht, Netzkarte, Benachrichtigungen, App)? Wofür wird es gebraucht bzw. welches Problem soll es lösen? Wie wichtig/dringend? Höchstens zwei bis drei Fragen auf einmal.
3. Sobald klar ist, was gewünscht wird: fasse den Wunsch in zwei, drei Sätzen zusammen und frage „Soll ich den Wunsch so zur Prüfung weitergeben?“.
4. Erst wenn die Person zustimmt (oder den Wunsch von sich aus bereits vollständig und klar beschrieben und um Weitergabe gebeten hat), schreibe ans Ende deiner Antwort genau eine Zeile:
[[WUNSCH: präzise Zusammenfassung des Wunsches – was, wo, wofür]]
und sage dazu, dass der Wunsch jetzt technisch, auf Verträglichkeit mit bestehenden Funktionen, auf die AGB und den Datenschutz geprüft wird und der Anbieter entscheidet, ob er umgesetzt wird.
Diese Zeile NIEMALS bei normalen Fragen („Wie …?“, „Was kostet …?“, „Wo finde ich …?“), bei unklaren Wünschen oder wenn es das schon gibt. Versprich nie, dass ein Wunsch umgesetzt wird oder bis wann.

`)
	b.WriteString(aiProductHelp)
	if p.Super {
		b.WriteString("\n- Betreiber: Menü „Kunden“ (#/admin) zum Anlegen und Verwalten von Kunden, „Anfragen“ (#/requests) für Änderungswünsche.")
	}
	b.WriteString("\n\nKONTO DER FRAGENDEN PERSON:\n")
	b.WriteString(s.aiContext(p, o))
	b.WriteString("\nPREISE:\n")
	b.WriteString(priceSheet(o))
	return b.String()
}

// ---------- Änderungswünsche ----------

type ReqAssessment struct {
	OK               bool   `json:"ok"` // false = Vorprüfung nicht möglich
	GibtEsSchon      bool   `json:"gibtEsSchon"`
	WieGenau         string `json:"wieGenau,omitempty"`
	Machbar          string `json:"machbar"`
	Konflikte        string `json:"konflikte,omitempty"`
	DatenschutzRecht string `json:"datenschutzRecht,omitempty"`
	Aufwand          string `json:"aufwand"`
	Zusammenfassung  string `json:"zusammenfassung"`
	AGB              string `json:"agb,omitempty"`        // Vereinbarkeit mit den AGB
	Dafuer           string `json:"dafuer,omitempty"`     // was spricht dafür
	Dagegen          string `json:"dagegen,omitempty"`    // was spricht dagegen
	Empfehlung       string `json:"empfehlung,omitempty"` // bauen | nicht bauen | rückfrage
	Begruendung      string `json:"begruendung,omitempty"`
}

type FeatureRequest struct {
	ID         string        `json:"id"`
	Org        string        `json:"org"`
	OrgName    string        `json:"orgName"`
	User       string        `json:"user,omitempty"`
	UserName   string        `json:"userName,omitempty"`
	Text       string        `json:"text"`
	Chat       string        `json:"chat,omitempty"`
	Assessment ReqAssessment `json:"assessment"`
	Status     string        `json:"status"` // neu, umsetzen, abgelehnt, rueckfrage, erledigt, vorhanden
	Reply      string        `json:"reply,omitempty"`
	Created    int64         `json:"created"`
	Updated    int64         `json:"updated"`
}

var reqStatusLabel = map[string]string{"neu": "Neu", "umsetzen": "Wird umgesetzt", "abgelehnt": "Abgelehnt",
	"rueckfrage": "Rückfrage", "erledigt": "Erledigt", "vorhanden": "Gibt es schon"}

var wishRe = regexp.MustCompile(`(?is)\[\[\s*WUNSCH\s*:\s*(.+?)\s*\]\]`)

// extractWish entfernt die Wunsch-Markierung aus der Antwort.
func extractWish(reply string) (string, string) {
	mm := wishRe.FindStringSubmatch(reply)
	if mm == nil {
		return strings.TrimSpace(reply), ""
	}
	clean := strings.TrimSpace(wishRe.ReplaceAllString(reply, ""))
	return clean, strings.TrimSpace(mm[1])
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func oneOf(v string, allowed []string, def string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, a := range allowed {
		if v == a {
			return a
		}
	}
	if v == "gross" {
		return "groß"
	}
	return def
}

// parseAssessment liest das JSON der Vorprüfung, auch wenn Text drumherum steht.
func parseAssessment(s string) (ReqAssessment, bool) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return ReqAssessment{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(s[i:j+1]), &raw); err != nil {
		return ReqAssessment{}, false
	}
	str := func(k string) string {
		switch v := raw[k].(type) {
		case string:
			return clip(v, 600)
		case nil:
			return ""
		default:
			return clip(fmt.Sprint(v), 600)
		}
	}
	a := ReqAssessment{OK: true, WieGenau: str("wieGenau"), Konflikte: str("konflikte"),
		DatenschutzRecht: str("datenschutzRecht"), Zusammenfassung: str("zusammenfassung"),
		AGB: str("agb"), Dafuer: str("dafuer"), Dagegen: str("dagegen"), Begruendung: str("begruendung")}
	a.Empfehlung = oneOf(strings.ToLower(str("empfehlung")), []string{"bauen", "nicht bauen", "rückfrage"}, "")
	if a.Empfehlung == "" {
		if e := strings.ToLower(str("empfehlung")); strings.Contains(e, "nicht") {
			a.Empfehlung = "nicht bauen"
		} else if strings.Contains(e, "rück") || strings.Contains(e, "rueck") {
			a.Empfehlung = "rückfrage"
		} else if strings.Contains(e, "bau") {
			a.Empfehlung = "bauen"
		}
	}
	switch v := raw["gibtEsSchon"].(type) {
	case bool:
		a.GibtEsSchon = v
	case string:
		a.GibtEsSchon = strings.EqualFold(strings.TrimSpace(v), "true") || strings.EqualFold(strings.TrimSpace(v), "ja")
	}
	a.Machbar = oneOf(str("machbar"), []string{"ja", "vermutlich", "unklar", "nein"}, "unklar")
	a.Aufwand = oneOf(str("aufwand"), []string{"klein", "mittel", "groß"}, "unklar")
	return a, true
}

func (s *Server) assessWish(ctx context.Context, plan, wish, chat string) ReqAssessment {
	sys := `Du prüfst Änderungswünsche von Kunden für die Monitoring-Software PingWacht vor (für den Entwickler).
Funktionsumfang: ` + aiFeatureList + `
AGB (Kurzfassung): ` + termsSummary + `
Prüfe gründlich:
1. Gibt es das schon (dann wie genau)?
2. Ist es technisch machbar?
3. Gibt es Konflikte mit bestehenden Funktionen oder anderen Kunden (z. B. Mandantentrennung, Sicherheit, Leistung)?
4. Verstößt es gegen die AGB oder müssten die AGB angepasst werden?
5. Gibt es Datenschutz- (DSGVO) oder sonstige Rechtsfragen (z. B. fremde personenbezogene Daten, Zugangsdaten, Aufzeichnung von Inhalten, Fernzugriff, Haftung)?
6. Wie groß ist der Aufwand?
Wäge dann ab, was dafür und was dagegen spricht, und gib eine Empfehlung.
Antworte NUR mit JSON in genau dieser Form, ohne weiteren Text:
{"gibtEsSchon":false,"wieGenau":"","machbar":"ja|vermutlich|unklar|nein","konflikte":"","agb":"","datenschutzRecht":"","aufwand":"klein|mittel|groß","dafuer":"Stichpunkte, mit ; getrennt","dagegen":"Stichpunkte, mit ; getrennt","empfehlung":"bauen|nicht bauen|rückfrage","begruendung":"ein Satz","zusammenfassung":"ein Satz für den Entwickler: was der Kunde möchte"}`
	out, err := aiChat(ctx, aiModel(plan), []aiMsg{{Role: "system", Content: sys}, {Role: "user", Content: "Wunsch: " + wish + "\n\nGesprächsauszug:\n" + chat}}, 900)
	if err == nil {
		if a, ok := parseAssessment(out); ok {
			if a.Zusammenfassung == "" {
				a.Zusammenfassung = clip(wish, 300)
			}
			return a
		}
	}
	return ReqAssessment{OK: false, Machbar: "unklar", Aufwand: "unklar", Zusammenfassung: clip(wish, 300)}
}

// requestRecipients: AI_REQUEST_TO (Login-E-Mails, kommagetrennt) oder alle Betreiber.
func (m *Monitor) requestRecipients() (users map[string]bool, mails []string, adminDevices bool) {
	users = map[string]bool{}
	var want []string
	for _, e := range strings.Split(os.Getenv("AI_REQUEST_TO"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			want = append(want, e)
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(want) > 0 {
		for _, u := range m.cfg.Users {
			for _, w := range want {
				if strings.EqualFold(u.Email, w) {
					users[u.ID] = true
				}
			}
		}
		for _, w := range want {
			if validEmail(w) {
				mails = append(mails, w)
			}
		}
		return users, mails, false
	}
	for _, u := range m.cfg.Users {
		if u.isSuper() {
			users[u.ID] = true
			if validEmail(u.Email) {
				mails = append(mails, u.Email)
			}
		}
	}
	if len(mails) == 0 {
		if e := envOr("OPS_EMAIL", os.Getenv("PROVIDER_EMAIL")); validEmail(e) {
			mails = append(mails, e)
		}
	}
	return users, mails, true
}

// requestPushFn verschickt einen Push an ein Gerät (in Tests ersetzbar).
var requestPushFn = func(m *Monitor, d Device, n notice) error {
	if d.Kind == "web" {
		return m.sendWebPush(d, n)
	}
	if m.pusher == nil {
		return errors.New("Apple-Push nicht aktiv")
	}
	return m.pusher.send(d, n)
}

func (m *Monitor) notifyRequest(fr FeatureRequest) {
	users, mails, adminDevs := m.requestRecipients()
	a := fr.Assessment
	who := fr.OrgName
	if fr.UserName != "" && fr.UserName != "Betreiber" {
		who += " (" + fr.UserName + ")"
	}
	title := "💡 Kunde " + who + " wünscht eine Änderung"
	body := clip(a.Zusammenfassung, 160) + " – geprüft: machbar " + a.Machbar + ", Aufwand " + a.Aufwand
	if a.Empfehlung != "" {
		body += ", Empfehlung: " + a.Empfehlung
	}
	body += ". Soll ich es bauen?"
	if !a.OK {
		body = clip(fr.Text, 200) + " – Vorprüfung nicht möglich. Bitte ansehen."
	}
	n := notice{kind: "request", title: title, body: body, org: defaultOrgID}
	m.mu.RLock()
	var devs []Device
	for _, d := range m.cfg.Devices {
		ok := users[d.User] || (adminDevs && d.User == "" && (d.Org == defaultOrgID || d.AllOrgs))
		if !ok {
			continue
		}
		if d.User != "" {
			if u := m.userByIDLocked(d.User); u != nil && u.Notify != nil && u.Notify.NoPush {
				continue
			}
		}
		devs = append(devs, d)
	}
	m.mu.RUnlock()
	for _, d := range devs {
		if err := requestPushFn(m, d, n); err != nil {
			log.Printf("Push Änderungswunsch an %s: %v", d.Name, err)
		}
	}
	if len(mails) == 0 || !smtpConfigured() {
		return
	}
	link := dashboardURL() + "/#/requests"
	var t strings.Builder
	fmt.Fprintf(&t, "Kunde %s wünscht:\n%s\n\n", who, fr.Text)
	if a.OK {
		fmt.Fprintf(&t, "Ich habe es geprüft (KI-Vorprüfung):\n- Gibt es schon: %v %s\n- Technisch machbar: %s\n- Aufwand: %s\n- Konflikte mit anderen Funktionen: %s\n- AGB: %s\n- Datenschutz/Recht: %s\n\nDafür spricht:\n%s\n\nDagegen spricht:\n%s\n\nEmpfehlung: %s%s\n\nSoll ich es bauen?\n\n",
			map[bool]string{true: "ja", false: "nein"}[a.GibtEsSchon], a.WieGenau, a.Machbar, a.Aufwand, dash(a.Konflikte), dash(a.AGB), dash(a.DatenschutzRecht),
			bullets(a.Dafuer), bullets(a.Dagegen), dash(a.Empfehlung), map[bool]string{true: " – " + a.Begruendung, false: ""}[a.Begruendung != ""])
	} else {
		t.WriteString("Vorprüfung nicht möglich.\n\n")
	}
	fmt.Fprintf(&t, "Freigeben, ablehnen oder Rückfrage an den Kunden: %s\n(Gebaut wird erst nach deiner Freigabe.)\n", link)
	if err := sendMail(mails, title, t.String(), "", nil); err != nil {
		log.Printf("Mail Änderungswunsch: %v", err)
	}
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "–"
	}
	return s
}

func (m *Monitor) userByIDLocked(id string) *User {
	for i := range m.cfg.Users {
		if m.cfg.Users[i].ID == id {
			return &m.cfg.Users[i]
		}
	}
	return nil
}

func (m *Monitor) addRequest(fr FeatureRequest) {
	m.mu.Lock()
	m.cfg.Requests = append(m.cfg.Requests, fr)
	if n := len(m.cfg.Requests); n > 500 {
		m.cfg.Requests = append([]FeatureRequest(nil), m.cfg.Requests[n-500:]...)
	}
	m.saveConfigLocked()
	m.mu.Unlock()
}

// ---------- API: /api/assistant ----------

func (s *Server) handleAssistant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	p := principal(r)
	o := s.m.orgByID(p.Org)
	if o == nil {
		http.Error(w, "Kunde unbekannt", 404)
		return
	}
	if !aiEnabled() {
		http.Error(w, "Der KI-Assistent ist nicht eingerichtet.", http.StatusServiceUnavailable)
		return
	}
	if !planHas(o.Plan, "assistant") {
		http.Error(w, "Der KI-Assistent ist ab dem Paket Basis enthalten.", http.StatusForbidden)
		return
	}
	var req struct {
		Messages []aiMsg `json:"messages"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var msgs []aiMsg
	for _, mm := range req.Messages {
		if mm.Role != "user" && mm.Role != "assistant" {
			continue
		}
		c := strings.TrimSpace(mm.Content)
		if c == "" {
			continue
		}
		if utf8.RuneCountInString(c) > 2000 {
			c = string([]rune(c)[:2000])
		}
		msgs = append(msgs, aiMsg{Role: mm.Role, Content: c})
	}
	if len(msgs) > 12 {
		msgs = msgs[len(msgs)-12:]
	}
	for len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = msgs[1:]
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
		http.Error(w, "Bitte eine Frage eingeben.", 400)
		return
	}
	if !aiTake(o.ID, o.Plan, time.Now()) {
		http.Error(w, fmt.Sprintf("Das Tageslimit von %d Fragen ist erreicht. Morgen geht es weiter – bei dringenden Fragen wenden Sie sich bitte an %s.",
			aiDailyLimit(o.Plan), providerInfo()["name"]), http.StatusTooManyRequests)
		return
	}
	all := append([]aiMsg{{Role: "system", Content: s.aiSystemPrompt(p, o)}}, msgs...)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	reply, err := aiChat(ctx, aiModel(o.Plan), all, 900)
	if err != nil {
		log.Printf("KI-Assistent (%s): %v", o.ID, err)
		http.Error(w, "Der Assistent ist gerade nicht erreichbar. Bitte später noch einmal versuchen.", http.StatusBadGateway)
		return
	}
	reply, wish := extractWish(reply)
	if wish != "" && len(msgs) > 0 && !looksLikeWish(msgs[len(msgs)-1].Content) {
		wish = "" // reine Frage – keine Anfrage anlegen
	}
	out := map[string]any{}
	if wish != "" {
		if !planHas(o.Plan, "requests") {
			reply += "\n\nÄnderungswünsche können im Paket **Pro** eingereicht werden. [Abo ansehen](#/billing)"
		} else {
			var ex []string
			for _, mm := range msgs[max(0, len(msgs)-6):] {
				who := "Kunde"
				if mm.Role == "assistant" {
					who = "Assistent"
				}
				ex = append(ex, who+": "+clip(mm.Content, 300))
			}
			chat := strings.Join(ex, "\n")
			a := s.assessWish(ctx, o.Plan, wish, chat)
			now := time.Now().UnixMilli()
			fr := FeatureRequest{ID: randHex(6), Org: o.ID, OrgName: o.Name, Text: clip(wish, 600), Chat: chat,
				Assessment: a, Status: "neu", Created: now, Updated: now}
			if p.User != nil {
				fr.User, fr.UserName = p.User.ID, p.User.Name
			} else {
				fr.UserName = "Betreiber"
			}
			if a.OK && a.GibtEsSchon {
				// gibt es schon: nichts speichern, nur kurz erklären
				fr.Status = "vorhanden"
				how := a.WieGenau
				if how == "" {
					how = a.Zusammenfassung
				}
				if how != "" {
					reply += "\n\n**Hinweis:** Das gibt es bereits. " + how
				}
			} else {
				s.m.addRequest(fr)
				go s.m.notifyRequest(fr)
				reply += "\n\n✅ Ihr Wunsch wurde an den Anbieter weitergeleitet. Den Stand sehen Sie unter „Meine Anfragen“."
			}
			out["request"] = map[string]string{"id": fr.ID, "status": fr.Status}
		}
	}
	out["reply"] = reply
	writeJSON(w, out)
}

// ---------- API: /api/requests ----------

type requestView struct {
	FeatureRequest
	StatusLabel string `json:"statusLabel"`
	UserEmail   string `json:"userEmail,omitempty"`
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	switch r.Method {
	case http.MethodGet:
		m.mu.RLock()
		var out []requestView
		for _, fr := range m.cfg.Requests {
			if !p.Super && fr.Org != p.Org {
				continue
			}
			v := requestView{FeatureRequest: fr, StatusLabel: reqStatusLabel[fr.Status]}
			if p.Super {
				if u := m.userByIDLocked(fr.User); u != nil {
					v.UserEmail = u.Email
				}
			} else {
				v.Assessment = ReqAssessment{} // interne Vorprüfung nur für den Betreiber
				v.Chat = ""
			}
			out = append(out, v)
		}
		m.mu.RUnlock()
		sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
		if out == nil {
			out = []requestView{}
		}
		writeJSON(w, out)
	case http.MethodPost:
		if !p.Super {
			http.Error(w, "Nur für den Betreiber", http.StatusForbidden)
			return
		}
		var req struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Reply  string `json:"reply"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if _, ok := reqStatusLabel[req.Status]; !ok {
			http.Error(w, "unbekannter Status", 400)
			return
		}
		var fr FeatureRequest
		found, changed := false, false
		m.mu.Lock()
		for i := range m.cfg.Requests {
			x := &m.cfg.Requests[i]
			if x.ID != req.ID {
				continue
			}
			found = true
			reply := clip(req.Reply, 2000)
			changed = x.Status != req.Status || x.Reply != reply
			x.Status, x.Reply, x.Updated = req.Status, reply, time.Now().UnixMilli()
			fr = *x
		}
		if found {
			m.saveConfigLocked()
		}
		var to string
		if u := m.userByIDLocked(fr.User); u != nil {
			to = u.Email
		}
		m.mu.Unlock()
		if !found {
			http.Error(w, "Anfrage nicht gefunden", 404)
			return
		}
		mailed := false
		if changed && validEmail(to) && smtpConfigured() {
			lbl := reqStatusLabel[fr.Status]
			text := fmt.Sprintf("Guten Tag,\n\nzu Ihrem Änderungswunsch in PingWacht gibt es Neuigkeiten.\n\nIhr Wunsch: %s\nStand: %s\n", fr.Text, lbl)
			if fr.Reply != "" {
				text += "\nNachricht des Anbieters:\n" + fr.Reply + "\n"
			}
			if u := dashboardURL(); u != "" {
				text += "\nPingWacht öffnen: " + u + "/\n"
			}
			text += "\nViele Grüße\n" + providerInfo()["name"]
			if err := sendMail([]string{to}, "PingWacht: Ihr Änderungswunsch – "+lbl, text, "", nil); err != nil {
				log.Printf("Mail Anfrage-Status: %v", err)
			} else {
				mailed = true
			}
		}
		writeJSON(w, map[string]any{"ok": true, "mailed": mailed})
	case http.MethodDelete:
		if !p.Super {
			http.Error(w, "Nur für den Betreiber", http.StatusForbidden)
			return
		}
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		out := m.cfg.Requests[:0]
		for _, x := range m.cfg.Requests {
			if x.ID != id {
				out = append(out, x)
			}
		}
		m.cfg.Requests = out
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "Methode nicht erlaubt", 405)
	}
}

// aiAccountInfo ergänzt /api/state und /api/me.
func (s *Server) aiAccountInfo(p *Principal, o *Org, out map[string]any) {
	on := aiEnabled()
	out["aiOn"] = on
	out["assistant"] = on && o != nil && planHas(o.Plan, "assistant")
	out["requests"] = on && o != nil && planHas(o.Plan, "requests")
	if p.Super {
		n := 0
		s.m.mu.RLock()
		for _, fr := range s.m.cfg.Requests {
			if fr.Status == "neu" {
				n++
			}
		}
		s.m.mu.RUnlock()
		out["requestsNew"] = n
	}
}

// looksLikeWish: enthält die letzte Nachricht einen Änderungswunsch (und nicht nur eine Frage)?
func looksLikeWish(t string) bool {
	t = strings.ToLower(t)
	for _, w := range []string{"fehlt", "vermisse", "wünsch", "hätte gern", "hätte ich gern", "gerne hätte", "wäre schön", "wäre toll", "wäre gut", "möchte", "einbauen", "hinzufügen", "ergänzen", "ändern", "anders", "feature", "wunsch", "bitte bau", "könnt ihr", "können sie"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	// kurze Bestätigung auf eine Rückfrage des Assistenten
	for _, w := range strings.FieldsFunc(t, func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch w {
		case "ja", "genau", "richtig", "korrekt", "bitte":
			return true
		}
	}
	return false
}

// bullets: „a; b; c“ → Aufzählung für die E-Mail.
func bullets(s string) string {
	var out []string
	for _, x := range strings.Split(s, ";") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, "- "+x)
		}
	}
	if len(out) == 0 {
		return "- –"
	}
	return strings.Join(out, "\n")
}

# PingWatch

Dauer-Ping mit Protokoll: Du siehst auf einen Blick, **wann** Pings verloren gingen oder **wann** die Antwortzeit deutlich höher war als normal – mit Zeitleiste, Ereignisliste und Statistik nach Tag und Uhrzeit.

- **Server** (Docker, z. B. auf der rstcloud): Dashboard für Browser und iPhone, speichert alle Messwerte und pingt öffentliche IPs selbst.
- **Agent** (Windows im Infobereich der Taskleiste, oder Linux/Raspberry Pi): pingt lokale IPs (192.168.x.x usw.) am Standort und schickt die Werte an den Server. Ist das Internet weg, puffert er und liefert nach.

```
 iPhone / Browser ──HTTPS──►  PingWatch-Server (rstcloud)  ──Ping──► öffentliche IPs (Antenne, 8.8.8.8 …)
                                     ▲
                        HTTPS (Token)│
                                     │
                     PingWatch-Agent am Standort ──Ping──► lokale Geräte (Router, AP, NAS …)
```

## Was erkannt wird

| Ereignis | Bedeutung |
|---|---|
| **Ausfall** | x Pings in Folge ohne Antwort (Standard 3, pro Host einstellbar). Dauer = bis zur ersten Antwort. |
| **Paketverlust** | einzelne verlorene Pings (weniger als „Ausfall ab“). Verluste innerhalb von 60 s werden zusammengefasst. |
| **Latenzspitze** | Antwortzeit über dem Schwellwert. Automatisch: 3× normaler Wert (Median der letzten 300 Antworten), mindestens +30 ms. Oder fester Wert in ms. |
| **Standort offline** | Agent hat sich > 45 s nicht gemeldet. |

## Auf der rstcloud einrichten

1. Dieses Verzeichnis als **GitHub-Repository** hochladen (privat reicht).
2. In der rstcloud unter **Apps** eine App aus dem Repo anlegen. Gebaut wird über das `Dockerfile`.
3. Einstellungen der App:
   - **Port:** `8080`
   - **Umgebungsvariable** `ADMIN_PASSWORD=<dein Passwort>` (Scope RUN_TIME, Secret).
   - **Umgebungsvariable** `DATABASE_URL=postgres://user:passwort@host:port/datenbank` (Scope RUN_TIME, Secret). Apps auf der rstcloud haben **keinen dauerhaften Speicher**, deshalb liegt alles in einer Managed PostgreSQL. Ohne `DATABASE_URL` speichert PingWatch in Dateien unter `/data`; das geht bei jedem Redeploy verloren.
     Alternativ zu `DATABASE_URL`: `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME` (optional `DB_SSLMODE`). Bietet der Server kein SSL, schaltet PingWatch automatisch auf `sslmode=disable` um.
4. Dashboard über die App-URL öffnen (HTTPS), anmelden und Hosts anlegen.

**ICMP in der Cloud:** Unter *Einstellungen → Allgemein* steht, ob der Container ICMP darf. Falls „nicht erlaubt“, Cloud-Hosts mit der Methode **TCP** anlegen, z. B. Port 443 oder 80. Ein geschlossener Port, der mit RST antwortet, zählt ebenfalls als erreichbar.

## Agent für lokale IPs

1. Im Dashboard unter **Einstellungen → Standorte** einen Standort anlegen, z. B. „Büro Albbruck“.
2. Dort **agent.json** und **Windows-Agent** herunterladen und beide in denselben Ordner legen, z. B. `C:\PingWatch`.
3. `pingwatch-agent.exe` starten. Das Symbol erscheint im Infobereich:
   - grün = alles erreichbar, orange = Paketverlust, rot = Host antwortet nicht, grau = Server nicht erreichbar
   - Linksklick öffnet das Dashboard, Rechtsklick das Menü (Autostart, Benachrichtigungen, Log-Ordner, Beenden)
   - startet automatisch mit Windows (im Menü abschaltbar)
   - zeigt eine Windows-Benachrichtigung bei Ausfall und bei Wiederkehr
4. Hosts mit „Pingt von: <Standort>“ anlegen. Der Agent übernimmt sie innerhalb weniger Sekunden.

Hinweis: Die exe ist nicht signiert. Beim ersten Start kann Windows SmartScreen warnen („Weitere Informationen → Trotzdem ausführen“).

**Raspberry Pi / Linux:**
```bash
chmod +x pingwatch-agent-linux-arm64
./pingwatch-agent-linux-arm64 agent -config agent.json
```
Als Dienst z. B. per systemd (`ExecStart=/opt/pingwatch/pingwatch-agent-linux-arm64 agent -config /opt/pingwatch/agent.json`, `Restart=always`). Für ICMP ohne root ggf. `sysctl net.ipv4.ping_group_range="0 2147483647"` setzen.

## iPhone

Dashboard in Safari öffnen → Teilen → **Zum Home-Bildschirm**. Danach startet es wie eine App im Vollbild.

## Kunden, Benutzer & Abo

- **Betreiber** (RST): Anmeldung mit E-Mail `admin` + `ADMIN_PASSWORD` (oder als Betreiber-Benutzer mit eigener E-Mail). Reiter **Kunden**: Kunden anlegen, Paket/Standorte setzen, sperren, löschen, per „Öffnen“ in den Kunden wechseln. Die bisherigen Hosts gehören zur Organisation „RST-Datentechnik“ (Paket Intern, unbegrenzt).
- **Kunden** melden sich mit E-Mail + Passwort an und sehen nur ihre eigenen Hosts, Standorte, Ereignisse und iPhones. Rollen: Inhaber (inkl. Abo), Admin, Nur lesen.
- **Pakete** (pro Standort/Monat netto): Free 0 € (1 Standort, 5 Hosts, 7 Tage), Basis 4,90 € (25 Hosts, 90 Tage), Pro 9,90 € (100 Hosts, 1 Jahr, Bericht, Traceroute, Statusseite), Partner 3,90 € (ab 10 Standorten).

Umgebungsvariablen (alle optional):

| Variable | Zweck |
|---|---|
| `ALLOW_SIGNUP=1` | Selbst-Registrierung unter `/signup` (startet im Free-Paket) |
| `STRIPE_SECRET_KEY` | Stripe-Geheimschlüssel (`sk_live_…` bzw. `sk_test_…`) |
| `STRIPE_WEBHOOK_SECRET` | Signatur-Geheimnis des Webhooks `https://<app>/api/stripe/webhook` (`whsec_…`) |
| `STRIPE_PRICE_BASIS`, `STRIPE_PRICE_PRO`, `STRIPE_PRICE_PARTNER` | Preis-IDs (`price_…`, monatlich, pro Einheit) |
| `STRIPE_AUTOMATIC_TAX=1` | Stripe Tax für die USt. verwenden |
| `PROVIDER_NAME`, `PROVIDER_EMAIL`, `PROVIDER_IMPRINT_URL`, `PROVIDER_PRIVACY_URL`, `PROVIDER_TERMS_URL` | Angaben auf Abo- und Registrierungsseite |

Stripe-Webhook-Ereignisse: `checkout.session.completed`, `customer.subscription.created`, `customer.subscription.updated`, `customer.subscription.deleted`, `invoice.payment_failed`. Für Rechnungen und Zahlungsmethode leitet PingWatch Kunden ins Stripe-Kundenportal (im Stripe-Dashboard aktivieren).

## Alarme & Monatsbericht

Unter **Einstellungen → Alarme** legt jeder Kunde eigene Alarmwege an (ab Paket Basis): E-Mail, Microsoft Teams (Workflow-Webhook), Telegram, eigener Webhook (JSON, optional HMAC-signiert). Der PDF-Monatsbericht (Pro/Partner) liegt unter **Statistik** und geht am 1. jedes Monats ab 7 Uhr an alle E-Mail-Alarmwege mit Haken „Monatsbericht“.

| Variable | Zweck |
|---|---|
| `SMTP_HOST`, `SMTP_PORT` (587) | eigener Mailserver |
| `SMTP_TLS` | `starttls` (Standard), `tls` (Port 465) oder `none` |
| `SMTP_USER`, `SMTP_PASSWORD` | Anmeldung am Mailserver (optional) |
| `SMTP_FROM` | Absender, z. B. `PingWatch <pingwatch@rst.de>` |
| `SMTP_INSECURE=1` | Zertifikat nicht prüfen (interner Server mit eigenem Zertifikat) |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_NAME` | gemeinsamer Bot des Anbieters (sonst trägt jeder Kunde seinen eigenen Bot ein) |
| `PUBLIC_URL` | Link in Alarmen, z. B. `https://pingwatch-a4029c.apps.rstcloud.de` |

Hinweis rstcloud: ausgehende Verbindungen auf 25/465/587 sind dort gesperrt – den Mailserver über einen freigegebenen Port oder das interne Netz ansprechen.

## Weitere Funktionen (Pro)

- **Webseiten- und DNS-Prüfung:** Methode „Webseite“ (Statuscode/Text, TLS-Zertifikat mit Warnung 14 Tage vor Ablauf) bzw. „DNS-Auflösung“ (optional eigener DNS-Server und erwartete Adresse).
- **Abhängigkeiten:** „Hängt ab von“ im Host – fällt z. B. der Router aus, gibt es für die Geräte dahinter keine Alarme.
- **Wartungsfenster:** einmalig oder wöchentlich; keine Alarme, zählt nicht in die Verfügbarkeit.
- **Traceroute:** startet bei jedem Ausfall automatisch (Cloud oder Agent), Ergebnis im Ereignis; von Hand im Host-Detail.
- **Netzwerk-Scan & UniFi-Import:** in der Standort-Karte; der Agent durchsucht sein Netz bzw. fragt die UniFi-Konsole (Integration-API, API-Schlüssel) ab.
- **Öffentliche Statusseite:** `https://<app>/status/<name>` mit 90-Tage-Verlauf und Vorfällen (ohne IP-Adressen).
- **Abrechnung:** Reiter Kunden → „Abrechnung (CSV)“ für das eigene Rechnungsprogramm.
- **SNMP (Switch/Router):** Methode „SNMP“ (v2c, Community). Schnittstellen mit Traffic, Fehlern und Status, Verlauf 24 h / 7 / 30 Tage; Haken „Alarm“ meldet, wenn ein Port auf „down“ geht.
- **UniFi-Livestatus:** in der Standort-Karte „UniFi-Livestatus“ – der Agent fragt die Konsole jede Minute ab und meldet Geräte, die offline gehen.
- **Quittieren & Eskalation:** laufende Ausfälle „quittieren“ (Web, iPhone: in der Ereignisliste nach rechts wischen). Alarmwege mit „Eskalation nach X Minuten“ bekommen nur Ausfälle, die bis dahin niemand quittiert hat.
- **Betreiber-Benutzer & Testzugänge:** im Team der Betreiber-Organisation „Zum Betreiber“; bei Kunden „Testzugang bis“.

Die neuen Funktionen am Standort brauchen den Agenten ab Version 1.1.0 – einfach die neue `pingwatch-agent.exe` über die alte kopieren.

## Daten

**Mit Datenbank (PostgreSQL):** PingWatch legt die Tabellen beim Start selbst an (`pw_config`, `pw_samples`, `pw_minutes`, `pw_events`).
- Jede einzelne Messung bleibt 3 Tage erhalten (Rohwerte), danach nur noch Minuten-Zusammenfassungen (Pings, Verluste, Spitzen, Ø/Min/Max).
- Minutenwerte und Ereignisse werden nach der Aufbewahrungszeit gelöscht (Standard 60 Tage, einstellbar).
- Platzbedarf: ca. 5 MB pro Host und Tag Rohwerte bei 1 s Intervall (nur 3 Tage), Minutenwerte ca. 100 KB pro Host und Tag.

**Ohne Datenbank (Dateien):**
- Messwerte: `/data/samples/<host>/<YYYY-MM-DD>.csv`. Ältere Tage werden gzip-komprimiert.
- Ereignisse: `/data/events.jsonl`, Konfiguration: `/data/config.json`
- Exporte als CSV (Excel-tauglich, Semikolon, deutsche Dezimalkommas): Ereignisse, Statistik nach Stunde, Rohwerte.
- Platzbedarf: ca. 1–1,5 MB pro Host und Tag bei 1 s Intervall, komprimiert deutlich weniger.

## Lokal testen

```bash
docker compose up --build        # → http://localhost:8080
# oder ohne Docker:
go run . server -data ./data -port 8080
PINGWATCH_SIM=1 go run . server  # mit simulierten Messwerten
```

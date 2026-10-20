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

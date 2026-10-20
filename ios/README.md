# PingWatch für iPhone

Native App (SwiftUI, iOS 17+) mit Übersicht, Zeitleiste, Ereignissen, Statistik,
Push-Benachrichtigungen und Widget. Spricht mit dem PingWatch-Server über dieselbe API wie das Dashboard.

## Einmalig auf dem Mac

```bash
# Xcode aus dem Mac App Store installieren und einmal starten
xcode-select --install            # Kommandozeilen-Tools (falls nicht vorhanden)
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"   # Homebrew
brew install xcodegen
git clone https://github.com/D3sp3rados/Pingwatch.git
cd Pingwatch/ios
xcodegen                           # erzeugt PingWatch.xcodeproj
open PingWatch.xcodeproj
```

In Xcode:
1. **Xcode → Settings → Accounts**: Apple-ID hinzufügen, die im Developer-Team ist.
2. Projekt **PingWatch** → Target **PingWatch** → **Signing & Capabilities** → **Team** wählen.
   Dasselbe beim Target **PingWatchWidget**.
   Xcode legt Bundle-IDs, App Group und Push-Berechtigung automatisch an.
3. iPhone per Kabel anschließen, oben als Ziel wählen, **▶︎** drücken.
   Beim ersten Mal auf dem iPhone: Einstellungen → Datenschutz & Sicherheit → **Entwicklermodus** einschalten.

Hinweis: Mit einem kostenlosen „Personal Team“ funktionieren Push und Widget nicht. Es muss das bezahlte Team sein.

## Push auf dem Server einrichten

Im Apple-Developer-Konto unter **Certificates, IDs & Profiles → Keys** einen Schlüssel mit
**Apple Push Notifications service (APNs)** anlegen und die `.p8`-Datei herunterladen (geht nur einmal).

In der rstcloud bei der App **pingwatch → Umgebung** (Scope RUN_TIME, Secret):

| Variable | Wert |
|---|---|
| `APNS_KEY` | kompletter Inhalt der `.p8`-Datei (inkl. BEGIN/END-Zeilen) |
| `APNS_KEY_ID` | Key-ID (10 Zeichen, steht beim Schlüssel) |
| `APNS_TEAM_ID` | Team-ID (oben rechts im Developer-Konto) |
| `APNS_TOPIC` | `de.rst.pingwatch` (Bundle-ID der App) |

Danach **Redeploy**. Im Dashboard unter Einstellungen → „iPhone-App & Push“ steht dann „Push über Apple ist eingerichtet“.

Aus Xcode installierte Apps nutzen den **Sandbox**-Push von Apple, TestFlight/App-Store-Versionen den normalen.
Die App meldet das dem Server automatisch.

## Verteilung an Kollegen (TestFlight)

Xcode → Ziel **Any iOS Device** → **Product → Archive** → **Distribute App → TestFlight & App Store**.
In App Store Connect bei der App unter **TestFlight** interne Tester hinzufügen.

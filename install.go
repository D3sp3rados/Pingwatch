package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Linux-Installation per Einzeiler:
//   curl -fsSL https://app.pingwacht.de/install.sh | sudo sh -s -- <STANDORT-TOKEN> [--sysonly]
// Das Programm selbst ist öffentlich (/dl/…); geheim ist nur der Standort-Token in der agent.json.

var publicAgentBinaries = map[string]bool{
	"pingwatch-agent.exe": true, "pingwatch-agent-linux-amd64": true, "pingwatch-agent-linux-arm64": true,
	"pingwatch-agent-darwin-arm64": true, "pingwatch-agent-darwin-amd64": true,
}

// handlePublicBinary: GET /dl/<name> – Agent-Programm ohne Anmeldung (enthält keine Zugangsdaten).
func (s *Server) handlePublicBinary(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/dl/")
	if !publicAgentBinaries[name] {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.agentDir, name)
	if _, err := os.Stat(p); err != nil {
		http.Error(w, "Datei nicht im Image enthalten", 404)
		return
	}
	if sum := fileSHA256(p); sum != "" {
		w.Header().Set("X-Sha256", sum)
	}
	w.Header().Set("X-Agent-Version", version)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, p)
}

// handleInstallScript: GET /install.sh – Installer für Linux (systemd).
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(strings.ReplaceAll(installScript, "@@SERVER@@", publicBase(r))))
}

const installScript = `#!/bin/sh
# PingWacht-Agent für Linux installieren (systemd)
# Aufruf: curl -fsSL @@SERVER@@/install.sh | sudo sh -s -- <STANDORT-TOKEN> [--sysonly]
# Den Token finden Sie in PingWacht unter Einstellungen → Standorte (agent.json) bzw. im Reiter Installieren.
set -eu
SERVER="@@SERVER@@"
TOKEN="${1:-${PINGWACHT_TOKEN:-}}"
MODE=""
for a in "$@"; do [ "$a" = "--sysonly" ] && MODE="sysonly"; done
if [ -z "$TOKEN" ] || [ "$TOKEN" = "--sysonly" ]; then
  echo "Bitte den Standort-Token angeben:  curl -fsSL $SERVER/install.sh | sudo sh -s -- <TOKEN>" >&2; exit 1
fi
if [ "$(id -u)" != "0" ]; then echo "Bitte mit sudo bzw. als root ausführen." >&2; exit 1; fi
case "$(uname -m)" in
  x86_64|amd64) BIN=pingwatch-agent-linux-amd64 ;;
  aarch64|arm64) BIN=pingwatch-agent-linux-arm64 ;;
  *) echo "Architektur $(uname -m) wird nicht unterstützt (amd64/arm64)." >&2; exit 1 ;;
esac
DIR=/opt/pingwacht; DATA=/var/lib/pingwacht
mkdir -p "$DIR" "$DATA"
echo "Lade $BIN …"
if command -v curl >/dev/null 2>&1; then curl -fsSL "$SERVER/dl/$BIN" -o "$DIR/pingwacht-agent.new"
else wget -qO "$DIR/pingwacht-agent.new" "$SERVER/dl/$BIN"; fi
chmod 755 "$DIR/pingwacht-agent.new" && mv -f "$DIR/pingwacht-agent.new" "$DIR/pingwacht-agent"
umask 077
if [ -n "$MODE" ]; then
  printf '{\n  "server": "%s",\n  "token": "%s",\n  "mode": "sysonly"\n}\n' "$SERVER" "$TOKEN" > "$DATA/agent.json"
else
  printf '{\n  "server": "%s",\n  "token": "%s"\n}\n' "$SERVER" "$TOKEN" > "$DATA/agent.json"
fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  cat > /etc/systemd/system/pingwacht-agent.service <<UNIT
[Unit]
Description=PingWacht-Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$DIR/pingwacht-agent agent -config $DATA/agent.json -data $DATA
Restart=always
RestartSec=10
AmbientCapabilities=CAP_NET_RAW

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now pingwacht-agent.service
  sleep 2
  systemctl --no-pager --lines=0 status pingwacht-agent.service || true
  echo
  echo "Fertig. Der Agent läuft als Dienst „pingwacht-agent“ und aktualisiert sich selbst."
  echo "Log: journalctl -u pingwacht-agent -f   ·   Entfernen: systemctl disable --now pingwacht-agent"
else
  echo "Kein systemd gefunden – Agent manuell starten:"
  echo "  $DIR/pingwacht-agent agent -config $DATA/agent.json -data $DATA"
fi
`

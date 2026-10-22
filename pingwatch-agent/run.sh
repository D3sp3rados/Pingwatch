#!/bin/sh
# Startet den PingWacht-Agenten. Bei jedem Start wird die aktuelle Agent-Version vom Server geladen.
set -u
OPT=/data/options.json
SERVER=$(jq -r '.server' "$OPT" | sed 's#/*$##')
TOKEN=$(jq -r '.token' "$OPT")
if [ -z "$TOKEN" ] || [ "$TOKEN" = "null" ]; then
  echo "Bitte in der Konfiguration des Add-ons das Token aus der agent.json eintragen."
  sleep 3600
  exit 1
fi
case "$(uname -m)" in
  aarch64|arm64) BIN=pingwatch-agent-linux-arm64 ;;
  x86_64|amd64)  BIN=pingwatch-agent-linux-amd64 ;;
  *) echo "Architektur $(uname -m) wird nicht unterstützt"; sleep 3600; exit 1 ;;
esac
echo "Lade $BIN von $SERVER …"
if curl -fsSL --retry 5 --retry-delay 5 -H "Authorization: Bearer $TOKEN" "$SERVER/api/agent/binary/$BIN" -o /data/agent.new; then
  chmod +x /data/agent.new && mv /data/agent.new /data/pingwatch-agent
else
  echo "Download fehlgeschlagen – verwende vorhandene Version (falls vorhanden)."
fi
if [ ! -x /data/pingwatch-agent ]; then
  echo "Kein Agent vorhanden – Server-Adresse und Token prüfen. Neuer Versuch in 60 s."
  sleep 60
  exit 1
fi
jq -n --arg s "$SERVER" --arg t "$TOKEN" '{server:$s, token:$t}' > /data/agent.json
chmod 600 /data/agent.json
exec /data/pingwatch-agent agent -config /data/agent.json -data /data

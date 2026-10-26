package main

import (
	"archive/zip"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Raspberry Pi als Standort-Agent: Startdateien (cloud-init) für Raspberry Pi OS Lite (64-bit).
// Raspberry Pi Imager schreibt das System auf USB-Stick/SD-Karte, danach werden user-data,
// network-config und meta-data auf dem Laufwerk „bootfs“ ersetzt. Beim ersten Start legt der Pi
// den Benutzer an, schaltet SSH ein und installiert den PingWacht-Agenten für den Standort.

var piHostRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

func piPassword() string {
	const abc = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 14)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(abc))))
		b[i] = abc[n.Int64()]
	}
	return string(b)
}

type piOpts struct {
	Server, Token, Site, Host, Pass string
	IP, GW, DNS                     string // leer = DHCP
}

func piUserData(o piOpts) string {
	return `#cloud-config
# PingWacht-Agent auf Raspberry Pi – Standort: ` + o.Site + `
hostname: ` + o.Host + `
manage_etc_hosts: true
timezone: Europe/Berlin
keyboard:
  layout: de
users:
- name: pingwacht
  groups: users,adm,dialout,netdev,video,plugdev,sudo
  shell: /bin/bash
  lock_passwd: false
chpasswd:
  expire: false
  users:
  - {name: pingwacht, password: "` + o.Pass + `", type: text}
ssh_pwauth: true
write_files:
- path: /etc/systemd/system/pingwacht-agent.service
  content: |
    [Unit]
    Description=PingWacht-Agent
    After=network-online.target
    Wants=network-online.target

    [Service]
    ExecStart=/opt/pingwacht/pingwacht-agent agent -config /var/lib/pingwacht/agent.json -data /var/lib/pingwacht
    Restart=always
    RestartSec=10
    AmbientCapabilities=CAP_NET_RAW

    [Install]
    WantedBy=multi-user.target
runcmd:
- [mkdir, -p, /opt/pingwacht, /var/lib/pingwacht]
- [install, -m, '0755', /boot/firmware/pingwacht/pingwacht-agent, /opt/pingwacht/pingwacht-agent]
- [install, -m, '0600', /boot/firmware/pingwacht/agent.json, /var/lib/pingwacht/agent.json]
- [rm, -f, /boot/firmware/pingwacht/agent.json]
- [systemctl, daemon-reload]
- [systemctl, enable, --now, pingwacht-agent.service]
- [systemctl, enable, --now, ssh]
- [sed, -i, 's/password: ".*"/password: "(entfernt)"/', /boot/firmware/user-data]
`
}

// piNetworkConfig nur bei fester IP – sonst bleibt die Vorgabe des Images (DHCP).
func piNetworkConfig(o piOpts) string {
	dns := o.DNS
	if dns == "" {
		dns = o.GW
	}
	return `network:
  version: 2
  ethernets:
    eth0:
      dhcp4: false
      addresses: [` + o.IP + `]
      routes:
      - to: default
        via: ` + o.GW + `
      nameservers:
        addresses: [` + dns + `]
`
}

func piReadme(o piOpts) string {
	ipTxt := "per DHCP (feste Zuweisung im Router empfohlen – die LAN-IP steht danach in PingWacht unter Standorte)"
	if o.IP != "" {
		ipTxt = "fest " + o.IP + ", Gateway " + o.GW
	}
	return strings.ReplaceAll(`PingWacht-Agent auf Raspberry Pi – Standort „`+o.Site+`“
================================================================

Geeignet: Raspberry Pi 3, 4, 5, 400, 500, Zero 2 W (64-bit).

1. Raspberry Pi Imager installieren: https://www.raspberrypi.com/software/
2. Imager: Gerät wählen → Betriebssystem „Raspberry Pi OS (other)“ →
   „Raspberry Pi OS Lite (64-bit)“ → Speicher = USB-Stick (oder SD-Karte).
   Die Frage nach „Anpassungen / OS customisation“ mit NEIN beantworten.
3. Nach dem Schreiben den Stick einmal ab- und wieder anstecken.
   Es erscheint ein Laufwerk „bootfs“.
4. ALLES aus diesem ZIP (user-data, meta-data, Ordner „pingwacht“`+map[bool]string{true: ", network-config", false: ""}[o.IP != ""]+`)
   auf „bootfs“ kopieren und die vorhandenen ERSETZEN.
5. Stick sicher auswerfen, in den Pi stecken, Netzwerkkabel dran, Strom dran.
   (Pi 4/5 starten direkt vom USB-Stick; beim Pi 3 einmal USB-Boot aktivieren
   oder eine SD-Karte verwenden.)

Nach ca. 3–5 Minuten meldet sich der Pi in PingWacht am Standort „`+o.Site+`“.
Er wird automatisch Haupt-Agent; ein bisheriger Agent (z. B. Büro-PC) bleibt als
Reserve in Bereitschaft und übernimmt selbständig, wenn der Pi ausfällt.

Zugang zum Pi (nur falls nötig):
  Name:      `+o.Host+`
  Benutzer:  pingwacht
  Passwort:  `+o.Pass+`
  SSH:       ssh pingwacht@`+o.Host+`.local
  Netzwerk:  `+ipTxt+`
Das Passwort wird nach dem ersten Start aus der Datei auf dem Stick entfernt –
bitte hier notieren bzw. diese Datei sicher aufbewahren und dann löschen.

Agent-Protokoll auf dem Pi:  journalctl -u pingwacht-agent -f
`, "\n", "\r\n")
}

// handlePiSetup: GET /download/pi-setup.zip?site=…&host=…&ip=…&gw=…&dns=…
func (s *Server) handlePiSetup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	site := s.siteFor(r, q.Get("site"))
	if site == nil || !principal(r).canEdit() {
		http.NotFound(w, r)
		return
	}
	o := piOpts{Server: publicBase(r), Token: site.Token, Site: site.Name, Host: strings.TrimSpace(q.Get("host")), Pass: piPassword()}
	if o.Host == "" {
		o.Host = "pingwacht-pi"
	}
	if !piHostRe.MatchString(o.Host) {
		http.Error(w, "Gerätename: nur Buchstaben, Ziffern und Bindestrich", 400)
		return
	}
	if ip := strings.TrimSpace(q.Get("ip")); ip != "" {
		if !strings.Contains(ip, "/") {
			ip += "/24"
		}
		gw, dns := strings.TrimSpace(q.Get("gw")), strings.TrimSpace(q.Get("dns"))
		if _, _, err := net.ParseCIDR(ip); err != nil || net.ParseIP(gw) == nil || (dns != "" && net.ParseIP(dns) == nil) {
			http.Error(w, "Feste IP: Adresse (z. B. 192.168.215.20/24) und Gateway angeben", 400)
			return
		}
		o.IP, o.GW, o.DNS = ip, gw, dns
	}
	bin := filepath.Join(s.agentDir, "pingwatch-agent-linux-arm64")
	exe, err := os.ReadFile(bin)
	if err != nil {
		http.Error(w, "Agent für Raspberry Pi (linux-arm64) ist auf dem Server nicht vorhanden", 500)
		return
	}
	aj, _ := json.MarshalIndent(map[string]string{"server": o.Server, "token": o.Token, "site": o.Site}, "", "  ")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="pingwacht-pi-`+o.Host+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	add := func(name, body string) {
		f, _ := zw.Create(name)
		f.Write([]byte(body))
	}
	add("user-data", piUserData(o))
	if o.IP != "" {
		add("network-config", piNetworkConfig(o))
	}
	add("meta-data", fmt.Sprintf("instance-id: pingwacht-%s\n", randHex(6)))
	add("LIES-MICH.txt", piReadme(o))
	add("pingwacht/agent.json", string(aj))
	if f, err := zw.CreateHeader(&zip.FileHeader{Name: "pingwacht/pingwacht-agent", Method: zip.Deflate}); err == nil {
		f.Write(exe)
	}
	zw.Close()
}

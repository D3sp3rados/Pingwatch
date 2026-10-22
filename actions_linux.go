//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Linux (Debian/Ubuntu/Raspberry Pi OS): apt. „Sicherheit“ über unattended-upgrades, „alle“ über apt-get upgrade.
func installUpdates(cat, reboot string) (any, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("Updates: der Agent läuft nicht als root")
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return nil, errors.New("Updates: nur Debian/Ubuntu (apt) werden unterstützt")
	}
	if _, err := aptRun(20*time.Minute, "apt-get", "update", "-q"); err != nil {
		return nil, errors.New("apt-get update: " + err.Error())
	}
	var out string
	var err error
	if cat != "all" {
		if _, e := exec.LookPath("unattended-upgrade"); e != nil {
			return nil, errors.New("Für „Sicherheitsupdates“ fehlt das Paket unattended-upgrades – bitte installieren oder „Alle Updates“ wählen")
		}
		out, err = aptRun(2*time.Hour, "unattended-upgrade", "-v")
	} else {
		out, err = aptRun(2*time.Hour, "apt-get", "-y", "-q", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "upgrade")
	}
	if err != nil {
		return nil, errors.New("Updates fehlgeschlagen: " + err.Error())
	}
	res := actionResult{OK: aptInstalled(out)}
	if _, e := os.Stat("/var/run/reboot-required"); e == nil {
		res.Reboot = true
	}
	if res.Reboot && (reboot == "ifneeded" || reboot == "now") || reboot == "now" {
		if rebootMachine(300) == nil {
			res.Planned = "in 5 Minuten"
		}
	}
	res.Message = actionSummary(len(res.OK), 0, res.Reboot, res.Planned)
	return res, nil
}

func aptRun(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
	b, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		return string(b), errors.New(strings.TrimSpace(lines[len(lines)-1]))
	}
	return string(b), nil
}

var reAptSetup = regexp.MustCompile(`(?m)^(?:Setting up|Unpacking) ([^ :]+)`)
var reUnattended = regexp.MustCompile(`(?m)^Packages that will be upgraded: (.+)$`)

func aptInstalled(out string) []string {
	seen := map[string]bool{}
	var l []string
	for _, m := range reUnattended.FindAllStringSubmatch(out, -1) {
		for _, p := range strings.Fields(m[1]) {
			if !seen[p] {
				seen[p] = true
				l = append(l, p)
			}
		}
	}
	for _, m := range reAptSetup.FindAllStringSubmatch(out, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			l = append(l, m[1])
		}
	}
	if len(l) > 200 {
		l = append(l[:200], "… und "+strconv.Itoa(len(l)-200)+" weitere")
	}
	return l
}

func rebootMachine(delaySec int) error {
	min := (delaySec + 59) / 60
	if b, err := exec.Command("shutdown", "-r", "+"+strconv.Itoa(min), "PingWacht: Neustart").CombinedOutput(); err != nil {
		return errors.New("Neustart fehlgeschlagen: " + strings.TrimSpace(string(b)))
	}
	return nil
}

//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type cpuState struct {
	total, idle uint64
	ok          bool
}

func osMachineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		}
	}
	return ""
}

func readCPU() (uint64, uint64, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseProcStat(b)
}

func collectFast(c *sysCollector, r *sysReport) {
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		r.OS = parseOSRelease(b)
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if r.OS == "" {
			r.OS = "Linux"
		}
		r.OS += " · Kernel " + strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				r.Uptime = int64(v)
			}
		}
	}
	// CPU: Differenz zur letzten Messung (beim ersten Mal 1 s messen)
	t, i, ok := readCPU()
	if ok && !c.cpu.ok {
		c.cpu = cpuState{t, i, true}
		time.Sleep(time.Second)
		t, i, ok = readCPU()
	}
	if ok {
		r.CPU = cpuPct(c.cpu.total, c.cpu.idle, t, i)
		c.cpu = cpuState{t, i, true}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		r.MemTotal, r.MemUsed, _ = parseMeminfo(b)
	}
	r.Disks = linuxDisks()
}

var linuxFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "f2fs": true, "jfs": true, "reiserfs": true}

func linuxDisks() []sysDisk {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	var out []sysDisk
	seenDev := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !linuxFS[f[2]] || seenDev[f[0]] {
			continue
		}
		mnt := strings.ReplaceAll(f[1], `\040`, " ")
		if strings.HasPrefix(mnt, "/snap/") || strings.HasPrefix(mnt, "/var/lib/docker") {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mnt, &st) != nil || st.Blocks == 0 {
			continue
		}
		seenDev[f[0]] = true
		bs := uint64(st.Bsize)
		out = append(out, sysDisk{Mount: mnt, Total: st.Blocks * bs, Free: st.Bavail * bs})
		if len(out) >= 30 {
			break
		}
	}
	return out
}

func serviceState(name string) string {
	b, err := runCmd(10*time.Second, "systemctl", "show", "-p", "LoadState", "-p", "ActiveState", "--", name)
	if err != nil && len(b) == 0 {
		return "unknown"
	}
	return parseSystemctlShow(b)
}

func eventCounts() (int, int, bool) {
	// Prioritäten 0–2 (emerg, alert, crit) und 3 (err) der letzten Stunde aus dem Journal
	c, err := runCmd(20*time.Second, "journalctl", "-q", "--no-pager", "-o", "cat", "-p", "0..2", "--since", "-1h")
	if err != nil && len(c) == 0 {
		return -1, -1, false
	}
	e, _ := runCmd(20*time.Second, "journalctl", "-q", "--no-pager", "-o", "cat", "-p", "3..3", "--since", "-1h")
	return parseJournalCount(c), parseJournalCount(e), true
}

func rebootPending() int {
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		return 1
	}
	if _, err := os.Stat("/var/lib/dpkg"); err == nil {
		return 0
	}
	return -1
}

func antivirus() *sysAV { return nil }

func pendingUpdates() (int, int, bool) {
	if _, err := os.Stat("/usr/lib/update-notifier/apt-check"); err == nil {
		// apt-check schreibt das Ergebnis nach stderr
		out, err := runCmdStderr(120*time.Second, "/usr/lib/update-notifier/apt-check")
		if err == nil {
			if n, ok := parseAptCheck(out); ok {
				return n, 0, true
			}
		}
	}
	if _, err := os.Stat("/usr/bin/apt"); err == nil {
		b, err := runCmd(120*time.Second, "apt", "list", "--upgradable")
		if err == nil {
			return parseAptList(b), 0, true
		}
	}
	return -1, 0, false
}

// restartService startet einen systemd-Dienst neu (nur für freigegebene Dienste, siehe selfHealer).
func restartService(name string) error {
	if !validServiceName(name) {
		return errors.New("ungültiger Dienstname")
	}
	out, err := runCmdCombined(60*time.Second, "systemctl", "restart", "--", name)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return errors.New(msg)
	}
	return nil
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	switch strings.ToLower(s) {
	case "to be filled by o.e.m.", "default string", "system serial number", "not specified", "none", "0":
		return ""
	}
	return s
}

func collectInventory() *sysInventory {
	inv := &sysInventory{}
	const dmi = "/sys/class/dmi/id/"
	inv.HW.Vendor, inv.HW.Model = readTrim(dmi+"sys_vendor"), readTrim(dmi+"product_name")
	inv.HW.Serial, inv.HW.BIOS = readTrim(dmi+"product_serial"), readTrim(dmi+"bios_version")
	if inv.HW.Model == "" { // Raspberry Pi u. ä.
		inv.HW.Model = strings.TrimRight(readTrim("/proc/device-tree/model"), "\x00")
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		cores := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch k {
			case "model name", "Model":
				if inv.HW.CPU == "" {
					inv.HW.CPU = v
				}
			case "processor":
				inv.HW.Threads++
			case "core id":
				cores[v] = true
			}
		}
		inv.HW.Cores = len(cores)
		if inv.HW.Cores == 0 {
			inv.HW.Cores = inv.HW.Threads
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		inv.HW.RAM, _, _ = parseMeminfo(b)
	}
	if b, err := runCmd(20*time.Second, "lsblk", "-J", "-d", "-b", "-o", "NAME,MODEL,SIZE,TYPE"); err == nil {
		var raw struct {
			Devs []struct {
				Name  string      `json:"name"`
				Model *string     `json:"model"`
				Size  json.Number `json:"size"`
				Type  string      `json:"type"`
			} `json:"blockdevices"`
		}
		if json.Unmarshal(b, &raw) == nil {
			for _, d := range raw.Devs {
				if d.Type != "disk" || strings.HasPrefix(d.Name, "zram") {
					continue
				}
				m := d.Name
				if d.Model != nil && strings.TrimSpace(*d.Model) != "" {
					m = strings.TrimSpace(*d.Model)
				}
				sz, _ := strconv.ParseUint(d.Size.String(), 10, 64)
				inv.HW.Disks = append(inv.HW.Disks, invDisk{Model: m, Size: sz})
			}
		}
	}
	inv.HW.NICs = netInventory()
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		inv.OS.Name = parseOSRelease(b)
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
				inv.OS.Version = strings.Trim(v, `"`)
			}
		}
	}
	inv.OS.Build = readTrim("/proc/sys/kernel/osrelease")
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				inv.OS.Boot = time.Now().Add(-time.Duration(v * float64(time.Second))).UnixMilli()
			}
		}
	}
	if fi, err := os.Stat("/etc/machine-id"); err == nil {
		inv.OS.Installed = fi.ModTime().UnixMilli() // Näherung: Zeitpunkt der Einrichtung
	}
	if d := readTrim("/proc/sys/kernel/domainname"); d != "" && d != "(none)" {
		inv.OS.Domain = d
	}
	if b, err := runCmd(5*time.Second, "who"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			inv.OS.User = f[0]
		}
	}
	if b, err := runCmd(120*time.Second, "dpkg-query", "-W", "-f", "${Package}\t${Version}\t${Maintainer}\n"); err == nil && len(b) > 0 {
		inv.SW = parseDpkg(b)
	} else if b, err := runCmd(120*time.Second, "rpm", "-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{VENDOR}\t%{INSTALLTIME}\n"); err == nil {
		inv.SW = parseRpm(b)
	}
	return inv
}

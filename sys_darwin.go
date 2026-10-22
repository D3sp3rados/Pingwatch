//go:build darwin

package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// macOS: best effort über Bordmittel (sysctl, vm_stat, top, launchctl, softwareupdate).

type cpuState struct{}

var reUUID = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

func osMachineID() string {
	b, err := runCmd(10*time.Second, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return ""
	}
	if m := reUUID.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

var reBoot = regexp.MustCompile(`sec\s*=\s*(\d+)`)

func collectFast(c *sysCollector, r *sysReport) {
	if b, err := runCmd(5*time.Second, "sw_vers", "-productVersion"); err == nil {
		r.OS = "macOS " + strings.TrimSpace(string(b))
	}
	if b, err := runCmd(5*time.Second, "sysctl", "-n", "kern.boottime"); err == nil {
		if m := reBoot.FindSubmatch(b); m != nil {
			if sec, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
				r.Uptime = time.Now().Unix() - sec
			}
		}
	}
	if b, err := runCmd(15*time.Second, "top", "-l", "2", "-n", "0", "-s", "1"); err == nil {
		r.CPU = parseTopCPU(b)
	}
	if b, err := runCmd(5*time.Second, "sysctl", "-n", "hw.memsize"); err == nil {
		r.MemTotal, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	}
	if b, err := runCmd(5*time.Second, "vm_stat"); err == nil {
		if u, ok := parseVMStat(b); ok {
			r.MemUsed = u
		}
	}
	for _, p := range []string{"/System/Volumes/Data", "/"} {
		var st syscall.Statfs_t
		if syscall.Statfs(p, &st) == nil && st.Blocks > 0 {
			bs := uint64(st.Bsize)
			r.Disks = append(r.Disks, sysDisk{Mount: "/", Total: st.Blocks * bs, Free: st.Bavail * bs})
			break
		}
	}
	if ents, err := os.ReadDir("/Volumes"); err == nil {
		for _, e := range ents {
			p := "/Volumes/" + e.Name()
			if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				continue // Verweis auf das Systemlaufwerk
			}
			var st syscall.Statfs_t
			if syscall.Statfs(p, &st) == nil && st.Blocks > 0 && st.Flags&0x1000 != 0 { // MNT_LOCAL
				bs := uint64(st.Bsize)
				r.Disks = append(r.Disks, sysDisk{Mount: p, Total: st.Blocks * bs, Free: st.Bavail * bs})
			}
		}
	}
}

func serviceState(name string) string {
	b, err := runCmd(10*time.Second, "launchctl", "list")
	if err != nil {
		return "unknown"
	}
	return parseLaunchctl(b, name)
}

func eventCounts() (int, int, bool) { return -1, -1, false }

func rebootPending() int { return -1 }

func antivirus() *sysAV { return nil }

func pendingUpdates() (int, bool) {
	b, err := runCmd(180*time.Second, "softwareupdate", "-l")
	if err != nil && len(b) == 0 {
		return -1, false
	}
	return parseSoftwareUpdate(b), true
}

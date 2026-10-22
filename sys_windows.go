//go:build windows

package main

import (
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Windows: Messwerte über die Windows-API (kernel32/advapi32), langsame Abfragen über PowerShell.

var (
	pGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	pGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	pGetLogicalDrives     = kernel32.NewProc("GetLogicalDrives")
	pGetDriveTypeW        = kernel32.NewProc("GetDriveTypeW")
	pGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
	pGetTickCount64       = kernel32.NewProc("GetTickCount64")
	pOpenSCManagerW       = advapi32.NewProc("OpenSCManagerW")
	pOpenServiceW         = advapi32.NewProc("OpenServiceW")
	pQueryServiceStatus   = advapi32.NewProc("QueryServiceStatus")
	pCloseServiceHandle   = advapi32.NewProc("CloseServiceHandle")
	pGetServiceKeyNameW   = advapi32.NewProc("GetServiceKeyNameW")
)

const hkeyLocalMachine = 0x80000002

type cpuState struct {
	total, idle uint64
	ok          bool
}

func ft64(ft [2]uint32) uint64 { return uint64(ft[1])<<32 | uint64(ft[0]) }

func readCPU() (total, idle uint64, ok bool) {
	var i, k, u [2]uint32
	r, _, _ := pGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, false
	}
	// Kernelzeit enthält die Leerlaufzeit
	return ft64(k) + ft64(u), ft64(i), true
}

// regString liest einen Zeichenketten-Wert aus HKLM.
func regString(path, name string) (string, bool) {
	var hk uintptr
	if r, _, _ := pRegOpenKeyExW.Call(hkeyLocalMachine, uintptr(unsafe.Pointer(u16(path))), 0, 0x20019|0x0100, uintptr(unsafe.Pointer(&hk))); r != 0 {
		return "", false
	}
	defer pRegCloseKey.Call(hk)
	var typ, size uint32
	if r, _, _ := pRegQueryValueExW.Call(hk, uintptr(unsafe.Pointer(u16(name))), 0, uintptr(unsafe.Pointer(&typ)), 0, uintptr(unsafe.Pointer(&size))); r != 0 {
		return "", false
	}
	if size == 0 {
		return "", true
	}
	buf := make([]uint16, size/2+1)
	if r, _, _ := pRegQueryValueExW.Call(hk, uintptr(unsafe.Pointer(u16(name))), 0, uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

func regKeyExists(path string) bool {
	var hk uintptr
	if r, _, _ := pRegOpenKeyExW.Call(hkeyLocalMachine, uintptr(unsafe.Pointer(u16(path))), 0, 0x20019|0x0100, uintptr(unsafe.Pointer(&hk))); r != 0 {
		return false
	}
	pRegCloseKey.Call(hk)
	return true
}

func osMachineID() string {
	s, _ := regString(`SOFTWARE\Microsoft\Cryptography`, "MachineGuid")
	return s
}

var winOS string

func collectFast(c *sysCollector, r *sysReport) {
	if winOS == "" {
		const k = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
		p, _ := regString(k, "ProductName")
		d, _ := regString(k, "DisplayVersion")
		if d == "" {
			d, _ = regString(k, "ReleaseId")
		}
		b, _ := regString(k, "CurrentBuild")
		winOS = winOSName(p, d, b)
	}
	r.OS = winOS
	if t, _, _ := pGetTickCount64.Call(); t > 0 {
		r.Uptime = int64(t / 1000)
	}
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
	var ms struct {
		Length, MemoryLoad                  uint32
		TotalPhys, AvailPhys                uint64
		TotalPageFile, AvailPageFile        uint64
		TotalVirtual, AvailVirtual, AvailEx uint64
	}
	ms.Length = uint32(unsafe.Sizeof(ms))
	if rr, _, _ := pGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); rr != 0 && ms.TotalPhys > 0 {
		r.MemTotal = ms.TotalPhys
		r.MemUsed = ms.TotalPhys - min(ms.AvailPhys, ms.TotalPhys)
	}
	mask, _, _ := pGetLogicalDrives.Call()
	for d := 0; d < 26; d++ {
		if mask&(1<<d) == 0 {
			continue
		}
		root := string(rune('A'+d)) + `:\`
		if t, _, _ := pGetDriveTypeW.Call(uintptr(unsafe.Pointer(u16(root)))); t != 3 { // DRIVE_FIXED
			continue
		}
		var freeAvail, total, totalFree uint64
		if rr, _, _ := pGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(u16(root))), uintptr(unsafe.Pointer(&freeAvail)),
			uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree))); rr != 0 && total > 0 {
			r.Disks = append(r.Disks, sysDisk{Mount: root[:2], Total: total, Free: freeAvail})
		}
	}
}

// serviceState fragt den Dienststeuerungs-Manager (Dienstname oder Anzeigename).
func serviceState(name string) string {
	scm, _, _ := pOpenSCManagerW.Call(0, 0, 0x0001) // SC_MANAGER_CONNECT
	if scm == 0 {
		return "unknown"
	}
	defer pCloseServiceHandle.Call(scm)
	h, _, err := pOpenServiceW.Call(scm, uintptr(unsafe.Pointer(u16(name))), 0x0004) // SERVICE_QUERY_STATUS
	if h == 0 {
		if e, ok := err.(syscall.Errno); ok && e == 1060 { // ERROR_SERVICE_DOES_NOT_EXIST → Anzeigename?
			buf := make([]uint16, 257)
			size := uint32(len(buf))
			if r, _, _ := pGetServiceKeyNameW.Call(scm, uintptr(unsafe.Pointer(u16(name))), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
				h, _, _ = pOpenServiceW.Call(scm, uintptr(unsafe.Pointer(&buf[0])), 0x0004)
			}
			if h == 0 {
				return "missing"
			}
		} else {
			return "unknown"
		}
	}
	defer pCloseServiceHandle.Call(h)
	var st [7]uint32
	if r, _, _ := pQueryServiceStatus.Call(h, uintptr(unsafe.Pointer(&st[0]))); r == 0 {
		return "unknown"
	}
	switch st[1] { // dwCurrentState
	case 1: // SERVICE_STOPPED
		return "stopped"
	case 3, 6, 7: // stop pending, pause pending, paused
		return "stopped"
	}
	return "running" // running, start/continue pending
}

const psEvents = `$e=Get-WinEvent -FilterHashtable @{LogName='System','Application';Level=1,2;StartTime=(Get-Date).AddHours(-1)} -ErrorAction SilentlyContinue; if($e){$e|Group-Object Level|%{"$($_.Name)=$($_.Count)"}}else{"0=0"}`

func eventCounts() (int, int, bool) {
	b, err := runPS(60*time.Second, psEvents)
	if err != nil && len(b) == 0 {
		return -1, -1, false
	}
	return parseEventCounts(b)
}

func rebootPending() int {
	if regKeyExists(`SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired`) ||
		regKeyExists(`SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`) {
		return 1
	}
	if v, ok := regString(`SYSTEM\CurrentControlSet\Control\Session Manager`, "PendingFileRenameOperations"); ok && strings.TrimSpace(v) != "" {
		return 1
	}
	return 0
}

const psAV = `$o=@{}; try{$s=Get-MpComputerStatus -ErrorAction Stop; $o.def=@{on=[bool]$s.AntivirusEnabled; rt=[bool]$s.RealTimeProtectionEnabled; age=[int]$s.AntivirusSignatureAge; am=[string]$s.AMRunningMode}}catch{}; try{$o.sc=@(Get-CimInstance -Namespace root/SecurityCenter2 -ClassName AntiVirusProduct -ErrorAction Stop | %{@{n=[string]$_.displayName; st=[int]$_.productState}})}catch{}; $o|ConvertTo-Json -Depth 4 -Compress`

func antivirus() *sysAV {
	b, err := runPS(60*time.Second, psAV)
	if err != nil && len(b) == 0 {
		return nil
	}
	return parseAVJSON(b)
}

const psUpdates = `try{$s=New-Object -ComObject Microsoft.Update.Session; $r=$s.CreateUpdateSearcher().Search("IsInstalled=0 and IsHidden=0 and Type='Software'"); $r.Updates.Count}catch{-1}`

func pendingUpdates() (int, bool) {
	b, err := runPS(5*time.Minute, psUpdates)
	if err != nil && len(b) == 0 {
		return -1, false
	}
	n := -1
	for _, line := range strings.Split(string(b), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			if x, e := atoiSafe(v); e {
				n = x
			}
		}
	}
	return n, n >= 0
}

func atoiSafe(s string) (int, bool) {
	n := 0
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

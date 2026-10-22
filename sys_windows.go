//go:build windows

package main

import (
	"errors"
	"fmt"
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

// psUpdates: alle nicht installierten, nicht ausgeblendeten Updates wie in „Windows Update“
// (online gesucht, inkl. Defender-Signaturen, .NET und Treiber) – Anzahl, davon Treiber, Titel.
// psSearchFn: sucht beim Standarddienst (ggf. WSUS) und bei jedem registrierten Update-Dienst
// (Microsoft Update, Windows Update, DCat …, ohne Store) und führt die Ergebnisse zusammen –
// so wie die Einstellungen-App mehrere Quellen abfragt (z. B. Defender-Signaturen, .NET 8/10).
const psSearchFn = `function PWServices(){ $ids=@(''); try{ $sm=New-Object -ComObject Microsoft.Update.ServiceManager; foreach($v in $sm.Services){ $id=([string]$v.ServiceID).ToLower(); if($id -ne '855e8a7c-ecb4-4ca3-b045-1dfa50104289'){ $ids+=$id } } }catch{}; if($ids -notcontains '9482f4b4-e343-43b6-b170-9a65bc822c77'){ $ids+='ws' }; return ,$ids }
function PWSearchIn($s,$sid,$crit){ $q=$s.CreateUpdateSearcher(); $q.Online=$true; if($sid -eq 'ws'){ $q.ServerSelection=2 } elseif($sid){ $q.ServerSelection=3; $q.ServiceID=$sid }; return ,$q.Search($crit).Updates }
function PWSearch($crit){ $s=New-Object -ComObject Microsoft.Update.Session; $s.ClientApplicationID='PingWacht'; $seen=@{}; $list=New-Object System.Collections.ArrayList; $ok=$false
 foreach($sid in (PWServices)){ try{ $us=PWSearchIn $s $sid $crit; $ok=$true; foreach($u in $us){ $id=[string]$u.Identity.UpdateID; if(-not $seen.ContainsKey($id)){ $seen[$id]=1; [void]$list.Add($u) } } }catch{} }
 if(-not $ok){ throw 'Windows Update nicht erreichbar' }; return ,$list }
`

const psUpdates = psSearchFn + `try{ $l=PWSearch "IsInstalled=0 and IsHidden=0"; $d=0; $t=@(); foreach($u in $l){ if($u.Type -eq 2){$d++}; $t+=[string]$u.Title }; ConvertTo-Json -Compress @{n=$l.Count; d=$d; t=$t}}catch{'{"n":-1}'}`

func pendingUpdates() (int, int, []string, bool) {
	b, err := runPS(5*time.Minute, psUpdates)
	if err != nil && len(b) == 0 {
		return -1, 0, nil, false
	}
	return parseWinUpdates(string(b))
}

var (
	pStartServiceW = advapi32.NewProc("StartServiceW")
)

// restartService startet einen gestoppten Windows-Dienst (nur freigegebene Dienste, siehe selfHealer).
// Benötigt Administratorrechte des Agenten.
func restartService(name string) error {
	if !validServiceName(name) {
		return errors.New("ungültiger Dienstname")
	}
	scm, _, err := pOpenSCManagerW.Call(0, 0, 0x0001)
	if scm == 0 {
		return fmt.Errorf("Dienststeuerung nicht erreichbar: %v", err)
	}
	defer pCloseServiceHandle.Call(scm)
	const access = 0x0010 | 0x0004 // SERVICE_START | SERVICE_QUERY_STATUS
	h, _, err := pOpenServiceW.Call(scm, uintptr(unsafe.Pointer(u16(name))), access)
	if h == 0 {
		if e, ok := err.(syscall.Errno); ok && e == 1060 {
			buf := make([]uint16, 257)
			size := uint32(len(buf))
			if r, _, _ := pGetServiceKeyNameW.Call(scm, uintptr(unsafe.Pointer(u16(name))), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
				h, _, err = pOpenServiceW.Call(scm, uintptr(unsafe.Pointer(&buf[0])), access)
			}
		}
		if h == 0 {
			if e, ok := err.(syscall.Errno); ok && e == 5 {
				return errors.New("Zugriff verweigert – der Agent braucht Administratorrechte, um Dienste zu starten")
			}
			return fmt.Errorf("Dienst nicht zu öffnen: %v", err)
		}
	}
	defer pCloseServiceHandle.Call(h)
	if r, _, err := pStartServiceW.Call(h, 0, 0); r == 0 {
		if e, ok := err.(syscall.Errno); ok && e == 1056 { // läuft bereits
			return nil
		}
		if e, ok := err.(syscall.Errno); ok && e == 5 {
			return errors.New("Zugriff verweigert – der Agent braucht Administratorrechte, um Dienste zu starten")
		}
		return fmt.Errorf("Start fehlgeschlagen: %v", err)
	}
	return nil
}

// Inventur über CIM/Registry – nur lesend, keine Produktschlüssel.
const psInventory = `$ErrorActionPreference='SilentlyContinue'
$cs=Get-CimInstance Win32_ComputerSystem; $bios=Get-CimInstance Win32_BIOS; $os=Get-CimInstance Win32_OperatingSystem
$cpu=@(Get-CimInstance Win32_Processor)
$ep=[datetime]'1970-01-01'
function ms($d){ if($d){ [int64](($d.ToUniversalTime()-$ep).TotalMilliseconds) } else { 0 } }
$disks=@(Get-CimInstance Win32_DiskDrive | ForEach-Object { @{m=[string]$_.Model; s=[uint64]$_.Size} })
$sw=@()
foreach($p in 'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*','HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*','HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*'){
  $sw+=@(Get-ItemProperty $p | Where-Object { $_.DisplayName -and -not $_.SystemComponent -and -not $_.ParentKeyName } | ForEach-Object { @{n=[string]$_.DisplayName; v=[string]$_.DisplayVersion; p=[string]$_.Publisher; d=[string]$_.InstallDate} })
}
$lic=Get-CimInstance SoftwareLicensingProduct -Filter "ApplicationID='55c92734-d682-4d71-983e-d6ec3f16059f' AND PartialProductKey IS NOT NULL" -Property LicenseStatus | Select-Object -First 1
$o=@{hw=@{vendor=[string]$cs.Manufacturer; model=[string]$cs.Model; serial=[string]$bios.SerialNumber; bios=[string]$bios.SMBIOSBIOSVersion; cpu=[string]$cpu[0].Name.Trim(); cores=[int]($cpu | Measure-Object NumberOfCores -Sum).Sum; threads=[int]($cpu | Measure-Object NumberOfLogicalProcessors -Sum).Sum; ram=[uint64]$cs.TotalPhysicalMemory; disks=$disks};
 os=@{name=[string]$os.Caption; version=[string]$os.Version; build=[string]$os.BuildNumber; installed=(ms $os.InstallDate); boot=(ms $os.LastBootUpTime); domain=[string]$cs.Domain; partOfDomain=[bool]$cs.PartOfDomain; user=[string]$cs.UserName; lic=$(if($lic){[int]$lic.LicenseStatus}else{-1})};
 sw=$sw}
$o | ConvertTo-Json -Depth 5 -Compress`

func collectInventory() *sysInventory {
	b, err := runPS(4*time.Minute, psInventory)
	inv, perr := parseWinInventory(b)
	if perr != nil || inv == nil {
		inv = &sysInventory{}
		if err != nil {
			inv.Err = "Inventur über PowerShell fehlgeschlagen: " + err.Error()
		} else if perr != nil {
			inv.Err = "Inventur nicht lesbar: " + perr.Error()
		}
	}
	inv.HW.NICs = netInventory()
	return inv
}

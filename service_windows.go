//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Windows-Dienst „PingWacht Agent“: startet beim Hochfahren ohne Anmeldung und läuft als SYSTEM
// (nötig für Updates, Neustarts und Scripts). Installation: pingwatch-agent.exe service install
// (als Administrator, meist über den PowerShell-Einzeiler install.ps1).

const (
	svcName    = "PingWachtAgent"
	svcDisplay = "PingWacht Agent"
)

var (
	pStartServiceCtrlDispatcherW = advapi32.NewProc("StartServiceCtrlDispatcherW")
	pRegisterServiceCtrlHandlerW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	pSetServiceStatus            = advapi32.NewProc("SetServiceStatus")
	pFindWindowW                 = user32.NewProc("FindWindowW")
)

// serviceMode: läuft als Dienst (keine Oberfläche, eigener Datenordner, Updates per Neustart durch den Dienst-Manager).
var serviceMode bool

func serviceDataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "PingWacht")
}

func serviceExeDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "PingWacht")
}

type serviceStatus struct {
	ServiceType, CurrentState, ControlsAccepted, Win32ExitCode, ServiceSpecificExitCode, CheckPoint, WaitHint uint32
}

type serviceTableEntry struct {
	name *uint16
	proc uintptr
}

var svc = struct {
	sync.Mutex
	handle uintptr
	stop   chan struct{}
	done   chan struct{}
	args   []string
}{}

func setSvcState(state uint32) {
	st := serviceStatus{ServiceType: 0x10, CurrentState: state}
	if state == 4 {
		st.ControlsAccepted = 1 | 4 // Stop, Herunterfahren
	}
	if state == 2 || state == 3 {
		st.WaitHint = 15000
	}
	pSetServiceStatus.Call(svc.handle, uintptr(unsafe.Pointer(&st)))
}

func svcHandler(ctrl, evType, evData, ctx uintptr) uintptr {
	switch ctrl {
	case 1, 5: // Stop, Herunterfahren
		setSvcState(3)
		svc.Lock()
		select {
		case <-svc.stop:
		default:
			close(svc.stop)
		}
		svc.Unlock()
	case 4: // Abfrage
	}
	return 0
}

func svcMain(argc, argv uintptr) uintptr {
	h, _, _ := pRegisterServiceCtrlHandlerW.Call(uintptr(unsafe.Pointer(u16(svcName))), syscall.NewCallback(svcHandler), 0)
	svc.handle = h
	setSvcState(2)
	go func() {
		runAgent(svc.args)
		close(svc.done)
	}()
	setSvcState(4)
	<-svc.done
	setSvcState(1)
	return 0
}

// serviceWait ersetzt im Dienst die Tray-Oberfläche: wartet auf „Dienst beenden“.
func serviceWait(a *Agent) {
	a.notify = func(string, string, bool) {}
	a.requestExit = func() {
		svc.Lock()
		select {
		case <-svc.stop:
		default:
			close(svc.stop)
		}
		svc.Unlock()
	}
	<-svc.stop
}

func runService(args []string) {
	if len(args) == 0 {
		showError("Aufruf: pingwatch-agent.exe service install|uninstall|run")
		return
	}
	switch args[0] {
	case "run":
		serviceMode = true
		svc.stop, svc.done, svc.args = make(chan struct{}), make(chan struct{}), args[1:]
		table := []serviceTableEntry{{u16(svcName), syscall.NewCallback(svcMain)}, {nil, 0}}
		if r, _, err := pStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0]))); r == 0 {
			showError("Der Dienst-Modus startet nur über die Windows-Dienstverwaltung: " + err.Error())
		}
	case "install":
		if err := serviceInstall(args[1:]); err != nil {
			serviceLog("FEHLER: " + err.Error())
			os.Exit(1)
		}
	case "uninstall":
		serviceUninstall()
	default:
		showError("Unbekannt: " + args[0])
	}
}

// serviceLog: Ausgabe der Installation (die exe hat kein Konsolenfenster) – Datei im Datenordner.
func serviceLog(msg string) {
	os.MkdirAll(serviceDataDir(), 0o755)
	if f, err := os.OpenFile(filepath.Join(serviceDataDir(), "install.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s %s\r\n", time.Now().Format("2006-01-02 15:04:05"), msg)
		f.Close()
	}
}

func sc(args ...string) (string, error) {
	cmd := exec.Command("sc.exe", args...)
	hideWindow(cmd)
	b, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(b)), err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".neu"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	out.Close()
	os.Remove(dst + ".old")
	os.Rename(dst, dst+".old")
	return os.Rename(tmp, dst)
}

func serviceInstall(args []string) error {
	cfg := ""
	sysOnly := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-config", "--config":
			if i+1 < len(args) {
				cfg = args[i+1]
				i++
			}
		case "-sysonly", "--sysonly":
			sysOnly = true
		}
	}
	// agent.json finden: Angabe, neben der exe oder aus dem bisherigen Benutzer-Datenordner
	cands := []string{cfg, filepath.Join(exeDir(), "agent.json")}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		cands = append(cands, filepath.Join(d, "PingWatch", "agent.json"))
	}
	af, used, err := loadAgentFile(cands...)
	if err != nil {
		return errors.New("agent.json nicht gefunden: " + err.Error())
	}
	if sysOnly {
		af.Mode = "sysonly"
	}
	dataDir, binDir := serviceDataDir(), serviceExeDir()
	os.MkdirAll(dataDir, 0o755)
	os.MkdirAll(binDir, 0o755)
	// Datenordner nur für SYSTEM und Administratoren (enthält den Standort-Token)
	exec.Command("icacls", dataDir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F").Run()
	b, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(filepath.Join(dataDir, "agent.json"), b, 0o600); err != nil {
		return err
	}
	self, _ := os.Executable()
	dst := filepath.Join(binDir, "pingwatch-agent.exe")
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(dst)) {
		sc("stop", svcName)
		time.Sleep(2 * time.Second)
		if err := copyFile(self, dst); err != nil {
			return errors.New("Programm konnte nicht nach " + binDir + " kopiert werden: " + err.Error())
		}
	}
	bin := `"` + dst + `" service run -data "` + dataDir + `"`
	if out, err := sc("query", svcName); err == nil && strings.Contains(out, svcName) {
		if out, err := sc("config", svcName, "binPath=", bin, "start=", "auto", "obj=", "LocalSystem", "DisplayName=", svcDisplay); err != nil {
			return errors.New("Dienst ändern: " + out)
		}
	} else if out, err := sc("create", svcName, "binPath=", bin, "start=", "auto", "obj=", "LocalSystem", "DisplayName=", svcDisplay); err != nil {
		return errors.New("Dienst anlegen: " + out)
	}
	sc("description", svcName, "PingWacht – Überwachung von Netzwerk und Rechner, Updates und Scripts nur auf Anforderung (app.pingwacht.de)")
	sc("failure", svcName, "reset=", "86400", "actions=", "restart/5000/restart/10000/restart/60000")
	// Tray-Agent des Benutzers beenden und dessen Autostart aus (sonst liefen zwei Agenten)
	setAutostart(false)
	if w, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(u16("PingWatchAgentWnd"))), 0); w != 0 {
		pPostMessageW.Call(w, wmClose, 0, 0)
		time.Sleep(time.Second)
	}
	if out, err := sc("start", svcName); err != nil && !strings.Contains(out, "1056") {
		return errors.New("Dienst starten: " + out)
	}
	serviceLog("Dienst installiert und gestartet (agent.json aus " + used + ")")
	return nil
}

func serviceUninstall() {
	sc("stop", svcName)
	time.Sleep(2 * time.Second)
	out, _ := sc("delete", svcName)
	serviceLog("Dienst entfernt: " + out)
}

// serviceInstalled: gibt es den Dienst? Dann startet der Tray-Agent keinen zweiten Agenten.
func serviceInstalled() bool {
	var hk uintptr
	r, _, _ := pRegOpenKeyExW.Call(0x80000002, uintptr(unsafe.Pointer(u16(`SYSTEM\CurrentControlSet\Services\`+svcName))), 0, 0x20019, uintptr(unsafe.Pointer(&hk)))
	if r != 0 {
		return false
	}
	pRegCloseKey.Call(hk)
	return true
}

// serviceRestartForUpdate: neue exe liegt an ihrem Platz – Prozess beenden, der Dienst-Manager startet neu.
func serviceRestartForUpdate(a *Agent) {
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.shutdown()
		log.Printf("Update eingespielt – Dienst startet neu")
		os.Exit(3) // Fehler-Code → Wiederherstellungsaktion „Neu starten“ greift
	}()
}

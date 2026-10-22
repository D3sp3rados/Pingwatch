//go:build windows

package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const defaultMode = "agent"

//go:embed trayicons/*.ico
var trayIcons embed.FS

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	pRegisterClassExW       = user32.NewProc("RegisterClassExW")
	pCreateWindowExW        = user32.NewProc("CreateWindowExW")
	pDefWindowProcW         = user32.NewProc("DefWindowProcW")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pGetMessageW            = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessageW       = user32.NewProc("DispatchMessageW")
	pPostQuitMessage        = user32.NewProc("PostQuitMessage")
	pPostMessageW           = user32.NewProc("PostMessageW")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenuW            = user32.NewProc("AppendMenuW")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pLoadImageW             = user32.NewProc("LoadImageW")
	pMessageBoxW            = user32.NewProc("MessageBoxW")
	pRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	pSetTimer               = user32.NewProc("SetTimer")
	pGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	pShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW          = shell32.NewProc("ShellExecuteW")
	pCreateMutexW           = kernel32.NewProc("CreateMutexW")
	pGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")
	pRegOpenKeyExW          = advapi32.NewProc("RegOpenKeyExW")
	pRegSetValueExW         = advapi32.NewProc("RegSetValueExW")
	pRegDeleteValueW        = advapi32.NewProc("RegDeleteValueW")
	pRegQueryValueExW       = advapi32.NewProc("RegQueryValueExW")
	pRegCloseKey            = advapi32.NewProc("RegCloseKey")
)

const (
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmEndSession    = 0x0016
	wmCommand       = 0x0111
	wmTimer         = 0x0113
	wmLButtonUp     = 0x0202
	wmLButtonDbl    = 0x0203
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	wmTray          = wmApp + 1
	wmRefresh       = wmApp + 2
	wmBalloon       = wmApp + 3
	nimAdd          = 0
	nimModify       = 1
	nimDelete       = 2
	nifMessage      = 0x01
	nifIcon         = 0x02
	nifTip          = 0x04
	nifInfo         = 0x10
	niifInfo        = 0x01
	niifWarning     = 0x02
	mfString        = 0x0000
	mfGrayed        = 0x0001
	mfChecked       = 0x0008
	mfSeparator     = 0x0800
	tpmReturnCmd    = 0x0100
	tpmRightButton  = 0x0002
	tpmBottomAlign  = 0x0020
	hkeyCurrentUser = 0x80000001
	keyAllAccess    = 0xF003F
	regSZ           = 1
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
	private uint32
}

type winGUID struct {
	d1 uint32
	d2 uint16
	d3 uint16
	d4 [8]byte
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         winGUID
	hBalloonIcon     uintptr
}

func u16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func copyU16(dst []uint16, s string) {
	u := syscall.StringToUTF16(s)
	if len(u) > len(dst) {
		u = u[:len(dst)-1]
		u = append(u, 0)
	}
	copy(dst, u)
}

func agentDataDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "PingWatch")
	}
	return filepath.Join(exeDir(), "PingWatchData")
}

func singleInstance() bool {
	_, _, err := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(u16("Local\\PingWatchAgent"))))
	if e, ok := err.(syscall.Errno); ok && e == 183 { // ERROR_ALREADY_EXISTS
		return false
	}
	return true
}

func showError(msg string) {
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(msg))), uintptr(unsafe.Pointer(u16("PingWacht-Agent"))), 0x30)
}

func openURL(u string) {
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(u))), 0, 0, 1)
}

// ---- Autostart über HKCU\...\Run ----

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runName = "PingWatchAgent"

func autostartEnabled() bool {
	var hk uintptr
	if r, _, _ := pRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(u16(runKey))), 0, 0x20019, uintptr(unsafe.Pointer(&hk))); r != 0 {
		return false
	}
	defer pRegCloseKey.Call(hk)
	r, _, _ := pRegQueryValueExW.Call(hk, uintptr(unsafe.Pointer(u16(runName))), 0, 0, 0, 0)
	return r == 0
}

func setAutostart(on bool) {
	var hk uintptr
	if r, _, _ := pRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(u16(runKey))), 0, keyAllAccess, uintptr(unsafe.Pointer(&hk))); r != 0 {
		return
	}
	defer pRegCloseKey.Call(hk)
	if on {
		exe, _ := os.Executable()
		val := syscall.StringToUTF16(`"` + exe + `"`)
		pRegSetValueExW.Call(hk, uintptr(unsafe.Pointer(u16(runName))), 0, regSZ, uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2))
	} else {
		pRegDeleteValueW.Call(hk, uintptr(unsafe.Pointer(u16(runName))))
	}
}

// ---- Tray ----

type uiPrefs struct {
	Notify      bool `json:"notify"`
	FirstRunSet bool `json:"firstRunDone"`
}

type tray struct {
	a          *Agent
	hwnd       uintptr
	icons      map[string]uintptr
	nid        notifyIconData
	taskbarMsg uint32
	prefs      uiPrefs
	prefsPath  string
	mu         sync.Mutex
	balloons   []balloon
	curIcon    string
}

type balloon struct {
	title, msg string
	warn       bool
}

var theTray *tray

func agentUI(a *Agent) {
	runtime.LockOSThread()
	t := &tray{a: a, icons: map[string]uintptr{}, prefsPath: filepath.Join(a.dataDir, "ui.json")}
	theTray = t
	t.prefs = uiPrefs{Notify: true}
	if b, err := os.ReadFile(t.prefsPath); err == nil {
		json.Unmarshal(b, &t.prefs)
	}
	if !t.prefs.FirstRunSet {
		setAutostart(true) // im Hintergrund mit Windows starten (abschaltbar im Menü)
		t.prefs.FirstRunSet = true
		t.savePrefs()
	} else if autostartEnabled() {
		setAutostart(true) // Pfad aktualisieren, falls die exe verschoben wurde
	}

	iconDir := filepath.Join(a.dataDir, "icons")
	os.MkdirAll(iconDir, 0o755)
	sm, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
	if sm == 0 {
		sm = 16
	}
	for _, name := range []string{"green", "orange", "red", "grey"} {
		b, err := trayIcons.ReadFile("trayicons/" + name + ".ico")
		if err != nil {
			continue
		}
		p := filepath.Join(iconDir, name+".ico")
		os.WriteFile(p, b, 0o644)
		h, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(u16(p))), 1, sm, sm, 0x10)
		t.icons[name] = h
	}

	hInst, _, _ := pGetModuleHandleW.Call(0)
	cls := u16("PingWatchAgentWnd")
	wc := wndClassEx{lpfnWndProc: syscall.NewCallback(wndProc), hInstance: hInst, lpszClassName: cls}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(u16("PingWacht"))), 0, 0, 0, 0, 0, 0, 0, hInst, 0)
	t.hwnd = hwnd
	tm, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	t.taskbarMsg = uint32(tm)
	t.addIcon()
	pSetTimer.Call(hwnd, 1, 5000, 0)

	a.changed = func() { pPostMessageW.Call(t.hwnd, wmRefresh, 0, 0) }
	a.notify = func(title, msg string, warn bool) {
		t.mu.Lock()
		on := t.prefs.Notify
		if on {
			t.balloons = append(t.balloons, balloon{title, msg, warn})
		}
		t.mu.Unlock()
		if on {
			pPostMessageW.Call(t.hwnd, wmBalloon, 0, 0)
		}
	}

	var m winMsg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (t *tray) savePrefs() {
	b, _ := json.Marshal(t.prefs)
	os.WriteFile(t.prefsPath, b, 0o644)
}

func (t *tray) addIcon() {
	t.nid = notifyIconData{}
	t.nid.cbSize = uint32(unsafe.Sizeof(t.nid))
	t.nid.hWnd = t.hwnd
	t.nid.uID = 1
	t.nid.uFlags = nifMessage | nifIcon | nifTip
	t.nid.uCallbackMessage = wmTray
	t.nid.hIcon = t.icons["grey"]
	t.curIcon = "grey"
	copyU16(t.nid.szTip[:], "PingWacht-Agent – verbinde …")
	pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.nid)))
	t.refresh()
}

func (t *tray) refresh() {
	st := t.a.status()
	icon := "green"
	var tip string
	site := st.Site
	if site == "" {
		site = "PingWacht"
	}
	switch {
	case !st.ServerOK:
		icon = "grey"
		tip = fmt.Sprintf("%s – Server nicht erreichbar", site)
		if st.Pending > 0 {
			tip += fmt.Sprintf(" (%d Messwerte gepuffert)", st.Pending)
		}
	case len(st.Down) > 0:
		icon = "red"
		tip = fmt.Sprintf("%s – keine Antwort: %s", site, strings.Join(st.Down, ", "))
	case st.Loss > 0:
		icon = "orange"
		tip = fmt.Sprintf("%s – %d/%d erreichbar, Paketverlust", site, st.Total-st.Loss, st.Total)
	default:
		tip = fmt.Sprintf("%s – %d/%d erreichbar", site, st.Total, st.Total)
	}
	if !st.ServerOK && len(st.Down) > 0 {
		icon = "red"
	}
	t.nid.uFlags = nifIcon | nifTip
	if h := t.icons[icon]; h != 0 {
		t.nid.hIcon = h
	}
	t.curIcon = icon
	for i := range t.nid.szTip {
		t.nid.szTip[i] = 0
	}
	copyU16(t.nid.szTip[:], tip)
	pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&t.nid)))
}

func (t *tray) showBalloons() {
	t.mu.Lock()
	bs := t.balloons
	t.balloons = nil
	t.mu.Unlock()
	for _, b := range bs {
		n := t.nid
		n.uFlags = nifInfo
		copyU16(n.szInfoTitle[:], b.title)
		copyU16(n.szInfo[:], b.msg)
		n.dwInfoFlags = niifInfo
		if b.warn {
			n.dwInfoFlags = niifWarning
		}
		pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
	}
}

func (t *tray) menu() {
	st := t.a.status()
	hm, _, _ := pCreatePopupMenu.Call()
	add := func(id uintptr, flags uintptr, text string) {
		pAppendMenuW.Call(hm, flags, id, uintptr(unsafe.Pointer(u16(text))))
	}
	site := st.Site
	if site == "" {
		site = "(unbekannt)"
	}
	add(100, mfString|mfGrayed, "Standort: "+site)
	switch {
	case !st.ServerOK:
		msg := "Server nicht erreichbar"
		if st.Pending > 0 {
			msg += fmt.Sprintf(" – %d Messwerte gepuffert", st.Pending)
		}
		add(101, mfString|mfGrayed, msg)
	case len(st.Down) > 0:
		add(101, mfString|mfGrayed, "Keine Antwort: "+strings.Join(st.Down, ", "))
	default:
		add(101, mfString|mfGrayed, fmt.Sprintf("%d Hosts werden überwacht", st.Total))
	}
	add(0, mfSeparator, "")
	add(1, mfString, "Dashboard öffnen")
	add(0, mfSeparator, "")
	var f uintptr
	if autostartEnabled() {
		f = mfChecked
	}
	add(2, mfString|f, "Mit Windows starten")
	f = 0
	if t.prefs.Notify {
		f = mfChecked
	}
	add(3, mfString|f, "Benachrichtigungen bei Ausfall")
	add(4, mfString, "Log-Ordner öffnen")
	add(0, mfSeparator, "")
	add(9, mfString, "Beenden")

	var pt struct{ x, y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(hm, tpmReturnCmd|tpmRightButton|tpmBottomAlign, uintptr(pt.x), uintptr(pt.y), 0, t.hwnd, 0)
	pPostMessageW.Call(t.hwnd, 0, 0, 0)
	pDestroyMenu.Call(hm)
	switch cmd {
	case 1:
		openURL(t.a.file.Server)
	case 2:
		setAutostart(!autostartEnabled())
	case 3:
		t.mu.Lock()
		t.prefs.Notify = !t.prefs.Notify
		t.mu.Unlock()
		t.savePrefs()
	case 4:
		openURL(t.a.dataDir)
	case 9:
		pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
		pDestroyWindow.Call(t.hwnd)
	}
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	t := theTray
	switch uint32(msg) {
	case wmTray:
		switch uint32(lParam) {
		case wmLButtonUp, wmLButtonDbl:
			if t != nil {
				openURL(t.a.file.Server)
			}
		case wmRButtonUp:
			if t != nil {
				t.menu()
			}
		}
		return 0
	case wmRefresh, wmTimer:
		if t != nil {
			t.refresh()
		}
		return 0
	case wmBalloon:
		if t != nil {
			t.showBalloons()
		}
		return 0
	case wmEndSession:
		if wParam != 0 && t != nil {
			t.a.savePending()
		}
		return 0
	case wmClose:
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	if t != nil && t.taskbarMsg != 0 && uint32(msg) == t.taskbarMsg {
		t.addIcon() // Explorer neu gestartet
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// Größenprüfung zur Compile-Zeit (Win64-ABI)
var _ [976 - unsafe.Sizeof(notifyIconData{})]byte
var _ [unsafe.Sizeof(notifyIconData{}) - 976]byte
var _ [80 - unsafe.Sizeof(wndClassEx{})]byte
var _ [unsafe.Sizeof(wndClassEx{}) - 80]byte
var _ [48 - unsafe.Sizeof(winMsg{})]byte
var _ [unsafe.Sizeof(winMsg{}) - 48]byte
var _ [40 - unsafe.Sizeof(icmpEchoReply{})]byte
var _ [unsafe.Sizeof(icmpEchoReply{}) - 40]byte

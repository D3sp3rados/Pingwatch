package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Selbst-Aktualisierung des Agenten: holt die neueste Version vom eigenen Server
// (/api/agent/binary/<name>), prüft die SHA-256-Prüfsumme, tauscht die Datei und startet neu.

var upd struct {
	sync.Mutex
	running bool
	lastTry time.Time
	lastMsg string
}

// newerVersion: a neuer als b? (Format 1.2.3, fehlende Teile = 0)
func newerVersion(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func agentBinaryName() string {
	switch runtime.GOOS {
	case "windows":
		return "pingwatch-agent.exe"
	case "darwin":
		return "pingwatch-agent-darwin-" + runtime.GOARCH
	}
	return "pingwatch-agent-linux-" + runtime.GOARCH
}

// updateStatus für Tray/Log.
func updateStatus() string {
	upd.Lock()
	defer upd.Unlock()
	return upd.lastMsg
}

// selfUpdate aktualisiert auf die Server-Version latest. force = sofort (Knopf), sonst höchstens alle 30 min ein Versuch.
func (a *Agent) selfUpdate(latest string, force bool) (string, error) {
	if latest == "" || !newerVersion(latest, version) {
		return "Agent ist aktuell (" + version + ")", nil
	}
	upd.Lock()
	if upd.running || (!force && time.Since(upd.lastTry) < 30*time.Minute) {
		upd.Unlock()
		return "Update läuft bereits", nil
	}
	upd.running, upd.lastTry = true, time.Now()
	upd.Unlock()
	msg, err := a.doUpdate(latest)
	upd.Lock()
	upd.running = false
	if err != nil {
		upd.lastMsg = "Update fehlgeschlagen: " + err.Error()
	} else {
		upd.lastMsg = msg
	}
	upd.Unlock()
	if err != nil {
		log.Printf("Update auf %s fehlgeschlagen: %v", latest, err)
	}
	return msg, err
}

func (a *Agent) doUpdate(latest string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	log.Printf("Update: lade Version %s (%s) …", latest, agentBinaryName())
	resp, err := a.req("GET", "/api/agent/binary/"+agentBinaryName(), nil, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Server: %s", resp.Status)
	}
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", fmt.Errorf("kein Schreibrecht im Programmordner (%v)", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 200<<20))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if n < 1<<20 {
		os.Remove(tmp)
		return "", errors.New("Download unvollständig")
	}
	if want := resp.Header.Get("X-Sha256"); want != "" && !strings.EqualFold(want, hex.EncodeToString(h.Sum(nil))) {
		os.Remove(tmp)
		return "", errors.New("Prüfsumme stimmt nicht – Update verworfen")
	}
	log.Printf("Update: Version %s geladen (%d KB), starte neu …", latest, n/1024)
	if err := a.applyUpdate(exe, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return "Aktualisiert auf " + latest + " – Agent startet neu", nil
}

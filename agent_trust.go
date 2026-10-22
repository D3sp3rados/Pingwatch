package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// trustStore (Agent): Signaturschlüssel des Kontos, beim ersten Kontakt gemerkt (Datei
// trusted-keys.json im Datenordner). Weitere Schlüssel nur, wenn ein gemerkter sie bestätigt hat.
// Sobald ein Schlüssel gemerkt ist, nimmt der Agent nur noch unterschriebene Aufträge an.
// Zurücksetzen: Datei trusted-keys.json löschen und Agent neu starten.
type trustStore struct {
	mu     sync.Mutex
	loaded bool
	Keys   []SignKey        `json:"keys"`
	Seen   map[string]int64 `json:"seen"` // Einmal-Kennungen sofortiger Aufträge (gegen Wiederholung)
}

func trustFile(dir string) string { return filepath.Join(dir, "trusted-keys.json") }

func (t *trustStore) loadLocked(dir string) {
	if t.loaded {
		return
	}
	t.loaded = true
	if b, err := os.ReadFile(trustFile(dir)); err == nil {
		json.Unmarshal(b, t)
	}
	if t.Seen == nil {
		t.Seen = map[string]int64{}
	}
}

func (t *trustStore) saveLocked(dir string) {
	if dir == "" {
		return
	}
	b, _ := json.MarshalIndent(t, "", "  ")
	os.WriteFile(trustFile(dir), b, 0o600)
}

// learn übernimmt Schlüssel aus der Server-Konfiguration nach den Regeln oben.
func (t *trustStore) learn(dir, org string, list []SignKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked(dir)
	has := func(id string) bool {
		for _, k := range t.Keys {
			if k.ID == id {
				return true
			}
		}
		return false
	}
	changed := false
	if len(t.Keys) == 0 {
		for _, k := range list {
			if k.Endorse == "" && spkiID(k.Pub) == k.ID {
				t.Keys = append(t.Keys, SignKey{ID: k.ID, Pub: k.Pub, Name: k.Name})
				changed = true
			}
		}
	}
	for more := true; more; {
		more = false
		for _, k := range list {
			if has(k.ID) || k.Endorse == "" || spkiID(k.Pub) != k.ID {
				continue
			}
			kid, sig, _ := strings.Cut(k.Endorse, ":")
			if verifyWithKeys(t.Keys, kid, endorseMsg(org, k.Pub), sig) {
				t.Keys = append(t.Keys, SignKey{ID: k.ID, Pub: k.Pub, Name: k.Name})
				changed, more = true, true
			}
		}
	}
	if changed {
		t.saveLocked(dir)
	}
}

// verifyJob prüft die Unterschrift eines Aktions-Auftrags.
func (a *Agent) verifyJob(j agentJob) error {
	t := &a.trust
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked(a.dataDir)
	if len(t.Keys) == 0 {
		return nil // Konto ohne Signaturschlüssel: wie bisher
	}
	sp, sig, kid := j.Params["sp"], j.Params["sig"], j.Params["kid"]
	if sp == "" || !verifyWithKeys(t.Keys, kid, sp, sig) {
		return errors.New("Auftrag nicht gültig unterschrieben – abgelehnt")
	}
	f := strings.Split(sp, "|")
	var org, kind, scope, id, cat, reboot, shash string
	switch {
	case len(f) == 11 && f[0] == "pw-act" && f[1] == "v1":
		org, kind, scope, id, cat, reboot, shash = f[2], f[3], f[4], f[5], f[6], f[7], f[8]
		ts, _ := strconv.ParseInt(f[9], 10, 64)
		if time.Since(time.UnixMilli(ts)) > 24*time.Hour || time.UnixMilli(ts).After(time.Now().Add(10*time.Minute)) {
			return errors.New("Auftrag abgelaufen – abgelehnt")
		}
		nonce := f[10]
		if _, dup := t.Seen[nonce]; dup {
			return errors.New("Auftrag wurde schon ausgeführt – Wiederholung abgelehnt")
		}
		now := time.Now().UnixMilli()
		for k, v := range t.Seen {
			if now-v > 48*3600*1000 {
				delete(t.Seen, k)
			}
		}
		t.Seen[nonce] = now
		defer t.saveLocked(a.dataDir)
	case len(f) == 12 && f[0] == "pw-plan" && f[1] == "v1":
		org, kind, scope, id, cat, reboot, shash = f[2], f[3], f[4], f[5], f[6], f[7], f[8]
	default:
		return errors.New("Auftrag unbekannten Formats – abgelehnt")
	}
	want := ""
	if j.Kind == "script" {
		want = scriptHash(j.Params["script"])
	}
	a.mu.Lock()
	siteID, siteOrg, siteCust, mach := a.siteID, a.siteOrg, a.siteCust, a.machine
	a.mu.Unlock()
	switch {
	case org != siteOrg || kind != j.Kind || shash != want:
		return errors.New("Auftrag passt nicht zur Unterschrift – abgelehnt")
	case cat != j.Params["cat"] || reboot != j.Params["reboot"]:
		return errors.New("Optionen passen nicht zur Unterschrift – abgelehnt")
	case scope == "machine" && id != siteID+":"+mach,
		scope == "site" && id != siteID,
		scope == "customer" && (id == "" || id != siteCust),
		scope != "machine" && scope != "site" && scope != "customer":
		return errors.New("Auftrag gilt laut Unterschrift nicht für diesen Rechner – abgelehnt")
	}
	return nil
}

package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signierte Aktionen: Aufträge (Updates, Neustarts, Scripts) und Pläne werden im Browser des
// Admins mit einem Schlüssel unterschrieben, der den Browser nie verlässt (WebCrypto, ECDSA P-256).
// Der Agent merkt sich die Schlüssel seines Kontos beim ersten Kontakt und nimmt danach nur
// unterschriebene Aufträge an. Weitere Schlüssel muss ein bereits bekannter Schlüssel bestätigen.
// Ein übernommener Server allein kann so nichts auf den Rechnern ausführen.

type SignKey struct {
	ID      string `json:"id"`                // erste 16 Hex-Zeichen von SHA-256(Schlüssel)
	Pub     string `json:"pub"`               // öffentlicher Schlüssel (SPKI, Base64)
	Name    string `json:"name"`              // z. B. „Raphi – Chrome am Mac“
	By      string `json:"by"`                // wer ihn angelegt hat
	At      int64  `json:"at"`                //
	Endorse string `json:"endorse,omitempty"` // Bestätigung durch einen vorhandenen Schlüssel: "<kid>:<sig>"
}

func spkiID(pub string) string {
	b, _ := base64.StdEncoding.DecodeString(pub)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func parseECPub(pub string) (*ecdsa.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKIXPublicKey(b)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("kein ECDSA-Schlüssel")
	}
	return ek, nil
}

// verifyP1363: WebCrypto liefert die Signatur als r||s (je 32 Byte, Base64).
func verifyP1363(pub *ecdsa.PublicKey, msg, sigB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != 64 {
		return false
	}
	h := sha256.Sum256([]byte(msg))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, h[:], r, s)
}

func endorseMsg(org, pub string) string { return "pw-key|v1|" + org + "|" + pub }

func scriptHash(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// actPayload: unterschriebener Text eines sofortigen Auftrags.
func actPayload(org, kind, scope, id, cat, reboot, shash string, ts int64, nonce string) string {
	return strings.Join([]string{"pw-act", "v1", org, kind, scope, id, cat, reboot, shash, strconv.FormatInt(ts, 10), nonce}, "|")
}

// planPayload: unterschriebener Text eines Plans.
func planPayload(org string, x UpdatePlan, shash string) string {
	days := make([]string, len(x.Weekdays))
	for i, d := range x.Weekdays {
		days[i] = strconv.Itoa(d)
	}
	return strings.Join([]string{"pw-plan", "v1", org, x.Kind, x.Scope, x.Target, x.Cat, x.Reboot, shash,
		strconv.Itoa(x.Every), strings.Join(days, ","), x.At}, "|")
}

// verifyWithKeys prüft eine Signatur gegen eine Schlüsselliste.
func verifyWithKeys(keys []SignKey, kid, msg, sig string) bool {
	for _, k := range keys {
		if k.ID != kid {
			continue
		}
		if pub, err := parseECPub(k.Pub); err == nil && verifyP1363(pub, msg, sig) {
			return true
		}
	}
	return false
}

func (m *Monitor) orgSignKeys(org string) []SignKey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, o := range m.cfg.Orgs {
		if o.ID == org {
			return append([]SignKey(nil), o.SignKeys...)
		}
	}
	return nil
}

// GET /api/signkeys · POST {pub, name, endorse, code} · DELETE ?id=&code=
func (s *Server) handleSignKeys(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m := s.m
	keys := m.orgSignKeys(p.Org)
	if r.Method == http.MethodGet {
		if keys == nil {
			keys = []SignKey{}
		}
		writeJSON(w, keys)
		return
	}
	if !p.canEdit() {
		http.Error(w, "Nur Inhaber und Admins", 403)
		return
	}
	var req struct{ Pub, Name, Endorse, Code string }
	if r.Method == http.MethodDelete {
		req.Code = r.URL.Query().Get("code")
	} else if !readJSON(w, r, &req) {
		return
	}
	if code, err := m.secondFactor(p, req.Code); err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	by := "Betreiber"
	if p.User != nil {
		by = p.User.Email
	}
	if r.Method == http.MethodDelete {
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		for i := range m.cfg.Orgs {
			if m.cfg.Orgs[i].ID != p.Org {
				continue
			}
			out := m.cfg.Orgs[i].SignKeys[:0]
			for _, k := range m.cfg.Orgs[i].SignKeys {
				if k.ID != id {
					out = append(out, k)
				}
			}
			m.cfg.Orgs[i].SignKeys = out
		}
		m.saveConfigLocked()
		m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if _, err := parseECPub(req.Pub); err != nil {
		http.Error(w, "Schlüssel ungültig", 400)
		return
	}
	k := SignKey{ID: spkiID(req.Pub), Pub: req.Pub, Name: strings.TrimSpace(req.Name), By: by, At: time.Now().UnixMilli()}
	if len(k.Name) > 80 {
		k.Name = k.Name[:80]
	}
	if len(keys) > 0 {
		// weiterer Schlüssel: muss von einem vorhandenen bestätigt sein
		kid, sig, _ := strings.Cut(req.Endorse, ":")
		if !verifyWithKeys(keys, kid, endorseMsg(p.Org, req.Pub), sig) {
			http.Error(w, "Ein weiterer Schlüssel muss in einem Browser bestätigt werden, der schon einen Schlüssel dieses Kontos hat", 400)
			return
		}
		k.Endorse = req.Endorse
	}
	m.mu.Lock()
	for i := range m.cfg.Orgs {
		if m.cfg.Orgs[i].ID == p.Org {
			for _, x := range m.cfg.Orgs[i].SignKeys {
				if x.ID == k.ID {
					m.mu.Unlock()
					writeJSON(w, k)
					return
				}
			}
			m.cfg.Orgs[i].SignKeys = append(m.cfg.Orgs[i].SignKeys, k)
		}
	}
	m.saveConfigLocked()
	m.mu.Unlock()
	writeJSON(w, k)
}

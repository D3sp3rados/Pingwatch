package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
)

func TestParseP8Variants(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	full := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	body := base64.StdEncoding.EncodeToString(der)
	for name, v := range map[string]string{
		"datei":        full,
		"ohne umbruch": strings.ReplaceAll(full, "\n", ""),
		"leerzeichen":  strings.ReplaceAll(full, "\n", " "),
		"nur inhalt":   body,
		"base64 datei": base64.StdEncoding.EncodeToString([]byte(full)),
		"escaped":      strings.ReplaceAll(full, "\n", `\n`),
		"anführung":    `"` + full + `"`,
	} {
		if _, err := parseP8(v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parseP8(full[:120]); err == nil || !strings.Contains(err.Error(), "unvollständig") && !strings.Contains(err.Error(), "Base64") {
		t.Fatal("abgeschnitten muss Fehler geben", err)
	}
}

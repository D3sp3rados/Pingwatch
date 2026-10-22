package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicInstall(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pingwatch-agent-linux-amd64"), []byte("BIN"), 0o755)
	s := &Server{agentDir: dir}
	rec := httptest.NewRecorder()
	s.handlePublicBinary(rec, httptest.NewRequest("GET", "/dl/pingwatch-agent-linux-amd64", nil))
	if rec.Code != 200 || rec.Body.String() != "BIN" || rec.Header().Get("X-Sha256") == "" {
		t.Fatal("dl", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handlePublicBinary(rec, httptest.NewRequest("GET", "/dl/agent.json", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatal("nur Agent-Programme", rec.Code)
	}
	t.Setenv("PUBLIC_URL", "https://app.example.de")
	rec = httptest.NewRecorder()
	s.handleInstallScript(rec, httptest.NewRequest("GET", "/install.sh", nil))
	b := rec.Body.String()
	if !strings.HasPrefix(b, "#!/bin/sh") || !strings.Contains(b, `SERVER="https://app.example.de"`) || strings.Contains(b, "@@SERVER@@") {
		t.Fatal(b[:200])
	}
}

func TestInstallPS(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleInstallPS(rec, httptest.NewRequest("GET", "/install.ps1", nil))
	b := rec.Body.String()
	if !strings.Contains(b, "service', 'install'") || strings.Contains(b, "@@SERVER@@") || !strings.Contains(b, "param(") {
		t.Fatal(b[:200])
	}
}

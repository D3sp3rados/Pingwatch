package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// runScript führt ein Script aus der Bibliothek aus (Agent). Ausgabe höchstens 64 KB.
func (a *Agent) runScript(lang, body string, timeoutSec int) (actionResult, error) {
	if timeoutSec <= 0 || timeoutSec > 3600 {
		timeoutSec = 300
	}
	win := runtime.GOOS == "windows"
	var ext string
	switch lang {
	case "powershell":
		ext = ".ps1"
		if !win {
			if _, err := exec.LookPath("pwsh"); err != nil {
				return actionResult{}, errors.New("PowerShell-Scripts laufen nur unter Windows (oder mit installiertem pwsh)")
			}
		}
	case "cmd":
		if !win {
			return actionResult{}, errors.New("cmd-Scripts laufen nur unter Windows")
		}
		ext = ".cmd"
	case "bash", "sh":
		if win {
			return actionResult{}, errors.New(lang + "-Scripts laufen nur unter Linux/macOS")
		}
		ext = ".sh"
	default:
		return actionResult{}, errors.New("Script-Sprache unbekannt")
	}
	dir := filepath.Join(a.dataDir, "scripts")
	os.MkdirAll(dir, 0o700)
	f := filepath.Join(dir, "run-"+randHex(6)+ext)
	content := body
	if win {
		content = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
		if lang == "powershell" {
			content = "\uFEFF" + content // UTF-8 mit BOM, sonst liest Windows PowerShell Umlaute falsch
		}
	}
	if err := os.WriteFile(f, []byte(content), 0o700); err != nil {
		return actionResult{}, err
	}
	defer os.Remove(f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch lang {
	case "powershell":
		if win {
			cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", f)
		} else {
			cmd = exec.CommandContext(ctx, "pwsh", "-NoProfile", "-NonInteractive", "-File", f)
		}
	case "cmd":
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", f)
	case "bash":
		cmd = exec.CommandContext(ctx, "bash", f)
	default:
		cmd = exec.CommandContext(ctx, "sh", f)
	}
	cmd.Dir = dir
	hideWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitWriter{w: &out, n: 64 << 10}, &limitWriter{w: &out, n: 64 << 10}
	start := time.Now()
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			return actionResult{Output: toUTF8(out.Bytes())}, errors.New("Zeitlimit überschritten (" + strconv.Itoa(timeoutSec) + " s)")
		case errors.As(err, &ee):
			code = ee.ExitCode()
		default:
			return actionResult{}, errors.New("Script konnte nicht gestartet werden: " + err.Error())
		}
	}
	o := toUTF8(out.Bytes())
	msg := "Exit " + strconv.Itoa(code) + " · " + time.Since(start).Round(100*time.Millisecond).String()
	if first := firstLine(o); first != "" {
		msg += " · " + first
	}
	return actionResult{Message: msg, Exit: &code, Output: o}, nil
}

type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

// toUTF8: Windows-Konsolen geben oft CP850/1252 aus – ungültiges UTF-8 als Latin-1 lesen.
func toUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len([]rune(l)) > 120 {
				l = string([]rune(l)[:120]) + "…"
			}
			return l
		}
	}
	return ""
}

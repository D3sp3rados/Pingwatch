//go:build !windows

package main

import (
	"os"
	"syscall"
	"time"
)

// applyUpdate (Linux/macOS/Pi): Datei atomar ersetzen und den Prozess mit der neuen Version neu starten.
func (a *Agent) applyUpdate(exe, tmp string) error {
	if err := os.Rename(tmp, exe); err != nil {
		return err
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.shutdown()
		env := append(os.Environ(), "PINGWACHT_UPDATED=1")
		if err := syscall.Exec(exe, os.Args, env); err != nil {
			os.Exit(0) // Dienst/Container startet neu
		}
	}()
	return nil
}

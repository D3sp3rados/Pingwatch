package main

import (
	"os"
	"os/exec"
	"time"
)

// applyUpdate (Windows): laufende exe umbenennen, neue an ihre Stelle, neuen Prozess starten, alten beenden.
func (a *Agent) applyUpdate(exe, tmp string) error {
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), "PINGWACHT_UPDATED=1")
	if err := cmd.Start(); err != nil {
		os.Remove(exe)
		os.Rename(old, exe)
		return err
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		if a.requestExit != nil {
			a.requestExit()
		} else {
			a.shutdown()
			os.Exit(0)
		}
	}()
	return nil
}

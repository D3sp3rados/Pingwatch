//go:build !windows

package main

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const defaultMode = "server"

func agentDataDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".pingwatch")
	}
	return "./pingwatch-agent"
}

func singleInstance() bool { return true }

func showError(msg string) { log.Println(msg) }

// agentUI ohne Oberfläche (Linux/Pi): Status ins Log, beenden per Signal.
func agentUI(a *Agent) {
	a.notify = func(title, msg string, warn bool) {}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ch:
			log.Printf("Beende Agent …")
			return
		case <-t.C:
			st := a.status()
			log.Printf("Status: Standort %q, %d Hosts, %d nicht erreichbar, Server ok=%v, Puffer %d", st.Site, st.Total, len(st.Down), st.ServerOK, st.Pending)
		}
	}
}

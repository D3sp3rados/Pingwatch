package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Ausgehende öffentliche IP des Servers (für Freigaben in Firewalls der Kunden).
var egressIP atomic.Value

func egressLoop() {
	cl := &http.Client{Timeout: 10 * time.Second}
	for {
		for _, u := range []string{"https://api.ipify.org", "https://ifconfig.me/ip", "https://icanhazip.com"} {
			resp, err := cl.Get(u)
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
			resp.Body.Close()
			if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil && resp.StatusCode == 200 {
				egressIP.Store(ip.String())
				break
			}
		}
		time.Sleep(time.Hour)
	}
}

func currentEgressIP() string {
	s, _ := egressIP.Load().(string)
	return s
}

package main

import (
	"os"
	"strings"
	"testing"
)

func TestPiUserData(t *testing.T) {
	o := piOpts{Server: "https://app.pingwacht.de", Token: "tok123", Site: "Büro", Host: "pingwacht-pi", Pass: piPassword(), IP: "192.168.215.20/24", GW: "192.168.215.1"}
	u := piUserData(o)
	if !strings.HasPrefix(u, "#cloud-config\n") || !strings.Contains(u, "install.sh") || !strings.Contains(u, "tok123") {
		t.Fatal(u)
	}
	if d := os.Getenv("PI_OUT"); d != "" {
		os.WriteFile(d+"/user-data", []byte(u), 0o644)
		os.WriteFile(d+"/network-config", []byte(piNetworkConfig(o)), 0o644)
	}
}

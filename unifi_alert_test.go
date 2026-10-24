package main

import "testing"

func TestUniFiAlerts(t *testing.T) {
	var nilc *UniFiCfg
	if !nilc.alerts("n:x", "ap") {
		t.Fatal("nil cfg meldet alles")
	}
	c := &UniFiCfg{}
	if c.alerts("n:ap1", "ap") || !c.alerts("n:gw", "gateway") || !c.alerts("console", "console") || c.alerts("p:camera:1", "camera") || !c.alerts("p:nvr:1", "nvr") {
		t.Fatal("Standard: nur Gateway/Konsole")
	}
	c.AlertOn = []string{"n:ap1"}
	c.AlertOff = []string{"n:gw"}
	if !c.alerts("n:ap1", "ap") || c.alerts("n:gw", "gateway") {
		t.Fatal("Ausnahmen")
	}
	c = &UniFiCfg{Alert: "all"}
	if !c.alerts("n:ap1", "ap") {
		t.Fatal("all")
	}
	c = &UniFiCfg{Alert: "none"}
	if c.alerts("n:gw", "gateway") {
		t.Fatal("none")
	}
}

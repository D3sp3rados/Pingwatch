package main

import "testing"

func TestDuplicateHost(t *testing.T) {
	sv, _ := newTestServer(t)
	m := sv.m
	m.UpsertHost(HostCfg{Name: "Server", Addr: "https://www.rst.de/", Method: "http", Site: "cloud", Org: "o1"})
	if d := m.duplicateHost(HostCfg{Name: "x", Addr: "www.rst.de", Method: "http", Site: "cloud", Org: "o1"}); d != "Server" {
		t.Fatalf("doppelt nicht erkannt: %q", d)
	}
	if d := m.duplicateHost(HostCfg{Name: "x", Addr: "www.rst.de", Method: "http", Site: "cloud", Org: "o2"}); d != "" {
		t.Fatal("anderes Konto")
	}
	if d := m.duplicateHost(HostCfg{Name: "x", Addr: "www.rst.de", Method: "icmp", Site: "cloud", Org: "o1"}); d != "" {
		t.Fatal("andere Methode")
	}
}

func TestUniFiPathOK(t *testing.T) {
	ok := [][2]string{{"GET", unifiIntAPI + "/sites"}, {"POST", unifiIntAPI + "/sites/x/devices/y/actions"}, {"GET", protectAPI + "/cameras/c/snapshot"}, {"GET", "/proxy/network/api/s/default/stat/device/aa"}}
	bad := [][2]string{{"POST", unifiIntAPI + "/sites/x/devices"}, {"DELETE", unifiIntAPI + "/sites"}, {"GET", "/api/users"}, {"GET", unifiIntAPI + "/../../api"}, {"POST", "/proxy/network/api/s/default/cmd/devmgr"}}
	for _, x := range ok {
		if !unifiPathOK(x[0], x[1]) {
			t.Fatal("sollte erlaubt sein:", x)
		}
	}
	for _, x := range bad {
		if unifiPathOK(x[0], x[1]) {
			t.Fatal("sollte gesperrt sein:", x)
		}
	}
}

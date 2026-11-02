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

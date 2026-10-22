package main

import (
	"reflect"
	"testing"
)

func TestFindAndNormScanNets(t *testing.T) {
	v := map[string]any{"data": []any{
		map[string]any{"name": "LAN", "ipSubnet": "192.168.3.1/24"},
		map[string]any{"name": "IoT", "ipv4Configuration": map[string]any{"hostIpAddress": "192.168.20.1", "prefixLength": float64(24)}},
		map[string]any{"name": "Groß", "subnet": "10.0.0.1/8"},
		map[string]any{"name": "WAN", "subnet": "85.10.1.1/29"},
	}}
	got := normScanNets(append(findCIDRs(v), "192.168.3.0/24"))
	want := []string{"10.0.0.0/24", "192.168.3.0/24", "192.168.20.0/24"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestScanNetworkMulti(t *testing.T) {
	old := simMode
	simMode = true
	defer func() { simMode = old }()
	r, err := scanNetwork("192.168.3.0/24, 192.168.20.0/24,192.168.3.0/24", false)
	if err != nil || len(r.Nets) != 2 {
		t.Fatal(r, err)
	}
	if _, err := scanNetwork("kaputt", false); err == nil {
		t.Fatal("ungültiges Netz akzeptiert")
	}
	r = &scanResult{Hits: []scanHit{{IP: "192.168.3.22"}, {IP: "192.168.3.1", Name: "h33"}}}
	nameScanHits(r, map[string]string{"192.168.3.22": "Drucker", "192.168.3.1": "x"})
	if r.Hits[0].Name != "Drucker" || r.Hits[1].Name != "h33" {
		t.Fatal(r.Hits)
	}
}

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

func TestLANsAndWANIP(t *testing.T) {
	l, ok := lanFromMap(map[string]any{"name": "Gäste", "vlan": "20", "ip_subnet": "192.168.20.1/24"})
	if !ok || l.VLAN != 20 || l.IP != "192.168.20.1/24" {
		t.Fatal(l)
	}
	l, ok = lanFromMap(map[string]any{"name": "IoT", "vlanId": float64(30), "ipv4Configuration": map[string]any{"hostIpAddress": "10.30.0.1", "prefixLength": float64(24)}})
	if !ok || l.VLAN != 30 || l.IP != "10.30.0.1/24" {
		t.Fatal(l)
	}
	det := map[string]any{"interfaces": map[string]any{"ports": []any{map[string]any{"name": "WAN", "ipAddress": "192.168.178.20"}}}, "ipAddress": "192.168.3.254"}
	if ip := findWANIP(det); ip != "192.168.178.20" {
		t.Fatal(ip)
	}
	if ip := findWANIP(map[string]any{"wan1": map[string]any{"ip": "85.1.2.3"}}); ip != "85.1.2.3" {
		t.Fatal(ip)
	}
	if ip := findWANIP(map[string]any{"ipAddress": "192.168.3.254"}); ip != "" {
		t.Fatal("LAN-IP als WAN erkannt:", ip)
	}
}

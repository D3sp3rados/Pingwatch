package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestParseGeoAddr(t *testing.T) {
	cases := map[string]string{
		"185.51.186.243 Kuhnert-Laufenburgerstrasse11": "Laufenburgerstraße 11",
		"185.51.186.245 Schmid-Im Schaechle4":          "Im Schaechle 4",
		"185.207.100.203 Leber Ruettehof33":            "Ruettehof 33",
		"Glueck Dogern Hattelstr. 13 192.168.250.231":  "Hattelstraße 13",
		"10.12.0.137 60GHZ Sektor":                     "",
		"Barr Joseph-Tröndlin Weg 19(glas)":            "Joseph-Tröndlin Weg 19",
		"R. Schaeuble Joseph-Tröndlin-Weg 14":          "Joseph-Tröndlin-Weg 14",
		"Adriano-Waldshuter Strasse2":                  "Waldshuter Straße 2",
	}
	for in, want := range cases {
		a, ok := parseGeoAddr(in, "")
		if want == "" {
			if ok {
				t.Errorf("%q: %q", in, a.Street)
			}
			continue
		}
		if a.Street != want {
			t.Errorf("%q: %q, erwartet %q", in, a.Street, want)
		}
	}
	// Abdeckung auf echten Namen (nur lokal vorhanden)
	f, err := os.Open("/tmp/claude-0/dup/map.txt")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n, ok := 0, 0
	var ex []string
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) < 4 || strings.HasPrefix(p[0], "d") == false {
			continue
		}
		n++
		if a, good := parseGeoAddr(p[1], p[3]); good {
			ok++
			if len(ex) < 60 && n%25 == 0 {
				ex = append(ex, p[1]+"  →  "+a.Street+" | "+a.Town)
			}
		}
	}
	t.Logf("%d von %d Namen mit Adresse", ok, n)
	for _, e := range ex {
		t.Log(e)
	}
}

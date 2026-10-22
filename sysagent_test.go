package main

import "testing"

func TestParseWinUpdates(t *testing.T) {
	n, d, l, ok := parseWinUpdates("\ufeff{\"n\":6,\"d\":0,\"t\":[\"KB2267602 Defender\",\"2026-10 .NET\"]}\r\n")
	if !ok || n != 6 || d != 0 || len(l) != 2 || l[0] != "KB2267602 Defender" {
		t.Fatal(n, d, l, ok)
	}
	if n, _, l, ok := parseWinUpdates(`{"n":1,"d":1,"t":"Intel Treiber"}`); !ok || n != 1 || len(l) != 1 {
		t.Fatal("einzelner Titel", n, l)
	}
	if n, _, _, ok := parseWinUpdates("4\r\n"); !ok || n != 4 {
		t.Fatal("alt", n)
	}
	for _, bad := range []string{"", `{"n":-1}`, "-1", "Fehler"} {
		if _, _, _, ok := parseWinUpdates(bad); ok {
			t.Fatal("ungültig akzeptiert:", bad)
		}
	}
}

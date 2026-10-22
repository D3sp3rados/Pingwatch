package main

import "testing"

func TestParseWinUpdates(t *testing.T) {
	for in, want := range map[string][3]int{"6;2\r\n": {6, 2, 1}, "4": {4, 0, 1}, "-1": {-1, 0, 0}, "": {-1, 0, 0}, "3;9": {3, 3, 1}} {
		n, d, ok := parseWinUpdates(in)
		if n != want[0] || d != want[1] || ok != (want[2] == 1) {
			t.Errorf("%q → %d %d %v", in, n, d, ok)
		}
	}
}

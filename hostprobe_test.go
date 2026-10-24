package main

import (
	"strings"
	"testing"
)

func TestHostProbe(t *testing.T) {
	_, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	code, body := c.do("POST", "/api/hosts", map[string]any{"name": "X", "addr": "1.2.3.4", "site": "cloud", "paused": true, "method": "icmp"})
	if code != 200 {
		t.Fatal(code, body)
	}
	var h struct{ ID string }
	mustJSON(t, body, &h)
	code, body = c.do("POST", "/api/hosts/probe", map[string]any{"ids": []string{h.ID}})
	if code != 200 || !strings.Contains(body, h.ID) {
		t.Fatal(code, body)
	}
	t.Log(body)
}

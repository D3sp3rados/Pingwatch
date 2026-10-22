package main

import (
	"net/http"
	"testing"
)

func TestAppWebSession(t *testing.T) {
	s, ts := newTestServer(t)
	tok := s.makeToken("admin", s.adminRef())
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", ts.URL+"/api/app/websession", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/#/" {
		t.Fatal(resp.Status, resp.Header.Get("Location"))
	}
	ok := false
	for _, c := range resp.Cookies() {
		if c.Name == "pw_session" && c.Value == tok {
			ok = true
		}
	}
	if !ok {
		t.Fatal("kein Sitzungs-Cookie")
	}
	req, _ = http.NewRequest("POST", ts.URL+"/api/app/websession", nil)
	req.Header.Set("Authorization", "Bearer falsch")
	resp, _ = cl.Do(req)
	resp.Body.Close()
	if resp.Header.Get("Location") != "/login" || len(resp.Cookies()) != 0 {
		t.Fatal("ungültiger Token:", resp.Header.Get("Location"))
	}
}

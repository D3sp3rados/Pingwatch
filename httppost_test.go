package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPCheckPost(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("accept") != "application/json" || string(b) != `{"label":"x"}` {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"address":"mn_shield-addr1abc"}`))
	}))
	defer ts.Close()
	h := HostCfg{ID: "t", Method: "http", Addr: ts.URL, HTTPVerb: "POST", HTTPBody: `{"label":"x"}`, HTTPHeaders: "accept: application/json", Expect: "mn_shield-addr"}
	if _, _, e := httpCheck(h, 3*time.Second); e != "" {
		t.Fatal(e)
	}
	h.HTTPVerb = ""
	if _, _, e := httpCheck(h, 3*time.Second); e == "" {
		t.Fatal("GET hätte fehlschlagen müssen")
	}
}

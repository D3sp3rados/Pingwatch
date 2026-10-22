package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtectFetchAndNotify(t *testing.T) {
	camState := "CONNECTED"
	bat := 80
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "k" {
			w.WriteHeader(401)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, protectAPI) {
		case "/meta/info":
			fmt.Fprint(w, `{"applicationVersion":"5.1.200"}`)
		case "/nvrs":
			fmt.Fprint(w, `{"id":"n1","name":"UNVR","modelKey":"nvr"}`)
		case "/cameras":
			fmt.Fprintf(w, `[{"id":"c1","name":"Eingang","modelKey":"camera","state":"%s"}]`, camState)
		case "/sensors":
			fmt.Fprintf(w, `[{"id":"s1","name":"Serverraum","state":"CONNECTED","batteryStatus":{"percentage":%d,"isLow":false},"stats":{"temperature":{"value":23.5},"humidity":{"value":41}},"isOpened":false}]`, bat)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	u := newUniFiAPI(srv.URL, "k")
	p, err := u.protect()
	if err != nil || p == nil {
		t.Fatal(p, err)
	}
	if p.Version != "5.1.200" || len(p.Devs) != 3 {
		t.Fatalf("%+v", p)
	}
	var sens protectDev
	for _, d := range p.Devs {
		if d.Kind == "sensor" {
			sens = d
		}
	}
	if sens.Bat != 80 || sens.Temp == nil || *sens.Temp != 23.5 || sens.Open == nil || *sens.Open {
		t.Fatalf("Sensor: %+v", sens)
	}
	// ohne Protect: 404 → Hinweis ohne Geräte
	if p, err := newUniFiAPI(srv.URL+"/x", "k").protect(); err != nil || p == nil || len(p.Devs) != 0 || !strings.Contains(p.Err, "404") {
		t.Fatal(p, err)
	}

	m := NewMonitor(NewFileStore(t.TempDir()))
	m.notices = make(chan notice, 20)
	site := SiteCfg{ID: "s1", Name: "Büro", Org: "o1"}
	m.ingestProtect(site, p) // erster Stand: keine Meldung
	camState, bat = "DISCONNECTED", 10
	p2, _ := u.protect()
	m.ingestProtect(site, p2)
	var got []string
	for len(m.notices) > 0 {
		got = append(got, (<-m.notices).title)
	}
	if len(got) != 2 || !strings.Contains(strings.Join(got, "|"), "Kamera Eingang offline") || !strings.Contains(strings.Join(got, "|"), "Akku schwach") {
		t.Fatalf("Meldungen: %v", got)
	}
	v := protectSiteView("s1")
	if v.Total != 3 || v.Offline != 1 || v.BatLow != 1 || v.Devs[0].Name != "Eingang" {
		t.Fatalf("Ansicht: %+v", v)
	}
	camState = "CONNECTED"
	p3, _ := u.protect()
	m.ingestProtect(site, p3)
	if n := <-m.notices; !strings.Contains(n.title, "wieder online") {
		t.Fatal(n.title)
	}
}

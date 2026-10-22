package main

import (
	"encoding/json"
	"testing"
)

func TestCustomers(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	code, body := c.do("POST", "/api/customers", map[string]string{"name": "Kindergärten", "number": "4711"})
	if code != 200 {
		t.Fatal(code, body)
	}
	var cu Customer
	json.Unmarshal([]byte(body), &cu)
	if code, _ := c.do("POST", "/api/customers", map[string]string{"name": "kindergärten"}); code != 409 {
		t.Fatal("doppelt erlaubt", code)
	}
	_, body = c.do("POST", "/api/sites", map[string]any{"name": "St. Martin Görwihl", "setCustomer": cu.ID, "setNumber": " 11848 "})
	var sc SiteCfg
	json.Unmarshal([]byte(body), &sc)
	if sc.Customer != cu.ID || sc.Number != "11848" {
		t.Fatalf("Zuordnung: %+v", sc)
	}
	if code, _ := c.do("POST", "/api/sites", map[string]any{"id": sc.ID, "setCustomer": "gibtsnicht"}); code != 400 {
		t.Fatal("fremder Kunde erlaubt", code)
	}
	// Umbenennen ändert die Zuordnung nicht
	c.do("POST", "/api/sites", map[string]any{"id": sc.ID, "name": "St. Martin"})
	if x, _ := s.m.siteByID(sc.ID); x.Customer != cu.ID || x.Number != "11848" || x.Name != "St. Martin" {
		t.Fatalf("nach Umbenennen: %+v", x)
	}
	var st struct {
		Customers []Customer `json:"customers"`
		Sites     []SiteCfg  `json:"sites"`
	}
	_, body = c.do("GET", "/api/state", nil)
	json.Unmarshal([]byte(body), &st)
	ok := false
	for _, x := range st.Sites {
		ok = ok || (x.ID == sc.ID && x.Customer == cu.ID && x.Number == "11848")
	}
	if len(st.Customers) != 1 || !ok {
		t.Fatalf("state: %s", body[:min(400, len(body))])
	}
	// Kunde löschen → Standort bleibt, ohne Kunde
	if code, _ := c.do("DELETE", "/api/customers?id="+cu.ID, nil); code != 200 {
		t.Fatal("löschen", code)
	}
	if x, _ := s.m.siteByID(sc.ID); x.Customer != "" {
		t.Fatal("Zuordnung nicht entfernt")
	}
}

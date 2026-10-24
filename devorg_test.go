package main

import "testing"

func TestDeviceForOrg(t *testing.T) {
	users := map[string]User{"u1": {ID: "u1", Org: "raphi", Memberships: []Membership{{Org: "rst"}}}}
	d := Device{Org: "raphi", User: "u1"}
	if !deviceForOrg(d, users, "raphi") || !deviceForOrg(d, users, "rst") {
		t.Fatal("Mitglied soll Meldungen beider Konten bekommen")
	}
	if deviceForOrg(d, users, "fremd") {
		t.Fatal("fremdes Konto")
	}
	if deviceForOrg(Device{Org: "raphi"}, users, "rst") {
		t.Fatal("Gerät ohne Benutzer nur eigenes Konto")
	}
	if !deviceForOrg(Device{Org: "x", AllOrgs: true}, users, "rst") {
		t.Fatal("Betreiber alle")
	}
}

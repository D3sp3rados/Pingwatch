package main

import "testing"

func TestDSNHostOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@172.16.1.115:5432/pingwatch?sslmode=disable")
	t.Setenv("DB_HOST", "153.94.132.115")
	t.Setenv("DB_PORT", "")
	d, _ := databaseDSN()
	if d != "postgres://u:p@153.94.132.115:5432/pingwatch?sslmode=disable" {
		t.Fatal(d)
	}
	t.Setenv("DB_HOST", "")
	d, _ = databaseDSN()
	if d != "postgres://u:p@172.16.1.115:5432/pingwatch?sslmode=disable" {
		t.Fatal(d)
	}
}

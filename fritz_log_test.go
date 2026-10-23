package main

import (
	"testing"
	"time"
)

func TestFritzLogSurvivesReboot(t *testing.T) {
	dir := t.TempDir()
	m := NewMonitor(NewFileStore(dir))
	hc := HostCfg{ID: "fbx1", Name: "FB"}
	fh := &fritzHost{}
	m.fritzExtraIngest(hc, fh, &fritzExtra{Log: []string{"b 10:01 DSL synchron", "a 10:00 Start"}}, true)
	// Box neu gestartet: altes Log weg, nur noch neue Zeile
	m.fritzExtraIngest(hc, fh, &fritzExtra{Log: []string{"c 10:05 Neustart"}}, true)
	if len(fh.logHist) != 3 || fh.logHist[0] != "c 10:05 Neustart" {
		t.Fatalf("log: %v", fh.logHist)
	}
	m.store.Flush()
	waitBlob := func() bool { b, _ := m.store.(blobStore).LoadBlob(fritzLogKey("fbx1")); return len(b) > 0 }
	for i := 0; i < 100 && !waitBlob(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	// PingWacht-Neustart
	fh2 := &fritzHost{}
	m.fritzExtraIngest(hc, fh2, &fritzExtra{Log: []string{"d 10:10 neu"}}, true)
	if len(fh2.logHist) < 2 || fh2.logHist[0] != "d 10:10 neu" {
		t.Fatalf("nach Neustart: %v", fh2.logHist)
	}
}

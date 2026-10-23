package main

import (
	"testing"
	"time"
)

func TestProtectIgnoredDevice(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	defer m.Shutdown()
	uc := &UniFiCfg{On: true, Ignore: []string{"p:camera:c1"}}
	site := SiteCfg{ID: "sig", Name: "Haus", Org: "o1", UniFi: uc}
	now := time.Now().UnixMilli()
	snap := &protectSnap{At: now, Devs: []protectDev{
		{ID: "c1", Kind: "camera", Name: "Wifi", State: "DISCONNECTED", Bat: -1},
		{ID: "c2", Kind: "camera", Name: "Hof", State: "CONNECTED", Bat: -1}}}
	m.ingestProtect(site, snap)
	m.ingestProtect(site, snap)
	protectState.Lock()
	open := len(protectState.bySite["sig"].open)
	protectState.Unlock()
	if open != 0 {
		t.Fatalf("pausiertes Gerät hat Ereignis: %d", open)
	}
	v := protectSiteView("sig", uc)
	if v.Offline != 0 || v.Total != 2 || !v.Devs[len(v.Devs)-1].Ign {
		t.Fatalf("view: %+v", v)
	}
}

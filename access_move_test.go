package main

import "testing"

func TestMoveSite(t *testing.T) {
	m := NewMonitor(NewFileStore(t.TempDir()))
	defer m.Shutdown()
	m.mu.Lock()
	m.cfg.Orgs = append(m.cfg.Orgs, Org{ID: "o2", Name: "Ziel"})
	m.cfg.Sites = append(m.cfg.Sites, SiteCfg{ID: "s9", Name: "Büro", Org: defaultOrgID, Customer: "c1"})
	m.cfg.Groups = append(m.cfg.Groups, Group{ID: "g1", Org: defaultOrgID, Name: "Intern"})
	m.cfg.Machines = append(m.cfg.Machines, MachineCfg{ID: "s9:pc", Site: "s9", Org: defaultOrgID, Group: "g1"})
	m.mu.Unlock()
	m.UpsertHost(HostCfg{Name: "drucker", Addr: "10.0.0.5", Site: "s9", Org: defaultOrgID, Group: "g1", Paused: true})
	h, mc, err := m.moveSite(defaultOrgID, "o2", "s9")
	if err != nil || h != 1 || mc != 1 {
		t.Fatalf("%d %d %v", h, mc, err)
	}
	sc, _ := m.siteByID("s9")
	mcfg, _ := m.machineByID("s9:pc")
	if sc.Org != "o2" || sc.Customer != "" || mcfg.Org != "o2" || mcfg.Group == "g1" || mcfg.Group == "" {
		t.Fatalf("site %+v machine %+v", sc, mcfg)
	}
}

package main

import (
	"math"
	"math/rand"
	"time"
)

// simMachines liefert im Simulationsmodus (PINGWATCH_SIM=1) drei Beispiel-Rechner für jeden
// Standort mit eingeschalteter Rechner-Überwachung.
func (m *Monitor) simMachines() {
	type simM struct {
		id, host, os string
		mem          uint64
		disks        []sysDisk
		svcs         []sysSvc
		av           *sysAV
		upd, reboot  int
		cpu          float64
	}
	const g = 1 << 30
	list := []simM{
		{"sim-dc01", "SRV-DC01", "Windows Server 2022 Standard 21H2 (Build 20348)", 16 * g,
			[]sysDisk{{"C:", 120 * g, 9 * g}, {"D:", 500 * g, 210 * g}}, []sysSvc{{"Spooler", "running"}, {"MSSQLSERVER", "running"}},
			&sysAV{Name: "Microsoft Defender", Enabled: true, RealTime: true, SigDays: 0}, 4, 0, 18},
		{"sim-pc07", "BUCHHALTUNG-PC", "Windows 11 Pro 23H2 (Build 22631)", 8 * g,
			[]sysDisk{{"C:", 250 * g, 140 * g}}, []sysSvc{{"Spooler", "stopped"}, {"Datev Sync", "running"}},
			&sysAV{Name: "Microsoft Defender", Enabled: true, RealTime: true, SigDays: 1}, 12, 1, 7},
		{"sim-nas", "nas-backup", "Debian GNU/Linux 12 (bookworm) · Kernel 6.1.0-21-amd64", 4 * g,
			[]sysDisk{{"/", 30 * g, 21 * g}, {"/srv/backup", 4000 * g, 900 * g}}, []sysSvc{{"smbd", "running"}}, nil, 0, 0, 3},
	}
	start := time.Now()
	for {
		m.mu.RLock()
		var sites []SiteCfg
		for _, s := range m.cfg.Sites {
			if s.Sys != nil && s.Sys.On {
				sites = append(sites, s)
			}
		}
		m.mu.RUnlock()
		now := time.Now()
		for _, s := range sites {
			for i, x := range list {
				mc, err := m.registerMachine(s, x.id, x.host, i > 0)
				if err != nil {
					continue
				}
				wave := math.Sin(float64(now.Sub(start).Seconds())/300 + float64(i))
				r := &sysReport{Host: x.host, OS: x.os, Uptime: int64(now.Sub(start).Seconds()) + 86400*int64(3+i*9),
					CPU: math.Max(0, x.cpu+wave*8+rand.Float64()*6), MemTotal: x.mem, MemUsed: uint64(float64(x.mem) * (0.55 + 0.1*wave)),
					Disks: x.disks, Svcs: x.svcs, EvCrit: 0, EvErr: 2 + i, Updates: x.upd, UpdAt: now.UnixMilli(), AV: x.av, Reboot: x.reboot, Ver: version}
				if x.os[0] == 'D' {
					r.EvCrit, r.EvErr, r.Reboot = -1, -1, 0
				}
				m.machineSeen(mc)
				m.ingestSys(mc, s.Sys.eff(), []sysSample{{T: now.UnixMilli(), R: r}})
			}
		}
		time.Sleep(20 * time.Second)
	}
}

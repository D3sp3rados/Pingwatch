package main

import (
	"net/http"
	"sync"
	"time"
)

// POST /api/hosts/probe {ids:[…]} – pausierte (oder beliebige) Cloud-Hosts einmalig prüfen,
// ohne sie zu aktivieren: keine Ereignisse, keine Meldungen, nichts wird gespeichert.
func (s *Server) handleHostProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	var req struct{ IDs []string }
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) > 300 {
		req.IDs = req.IDs[:300]
	}
	type res struct {
		ID   string  `json:"id"`
		OK   bool    `json:"ok"`
		RTT  float64 `json:"rtt"` // ms, Mittel der Antworten
		Got  int     `json:"got"` // Antworten von 3
		Err  string  `json:"err,omitempty"`
		Skip string  `json:"skip,omitempty"`
	}
	var cfgs []HostCfg
	out := []res{}
	for _, id := range req.IDs {
		h := s.hostFor(r, id)
		if h == nil {
			continue
		}
		s.m.mu.RLock()
		c := h.cfg
		s.m.mu.RUnlock()
		if c.Site != "" && c.Site != "cloud" {
			out = append(out, res{ID: id, Skip: "Agent-Standort – nur Cloud-Hosts testbar"})
			continue
		}
		if c.Addr == "" {
			out = append(out, res{ID: id, Skip: "keine Adresse"})
			continue
		}
		cfgs = append(cfgs, c)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 24)
	for _, c := range cfgs {
		wg.Add(1)
		go func(c HostCfg) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rs := &resolver{addr: c.Addr}
			x := res{ID: c.ID}
			var sum float64
			for i := 0; i < 3; i++ {
				v, _, e := probe(c, rs)
				if v >= 0 {
					x.Got++
					sum += float64(v) / 10
				} else if e != "" {
					x.Err = e
				}
				time.Sleep(300 * time.Millisecond)
			}
			x.OK = x.Got > 0
			if x.Got > 0 {
				x.RTT, x.Err = sum/float64(x.Got), ""
			}
			mu.Lock()
			out = append(out, x)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	writeJSON(w, out)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// runTrace führt eine Routenverfolgung mit den Bordmitteln des Systems aus
// (Windows: tracert, Linux/macOS: traceroute oder tracepath) und liefert die Ausgabe.
func runTrace(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" || strings.ContainsAny(addr, " ;&|`$<>\"'") || strings.HasPrefix(addr, "-") {
		return "Adresse ungültig"
	}
	if simMode {
		return "Routenverfolgung zu " + addr + " (Simulation)\n 1  192.168.1.1  1 ms\n 2  10.0.0.1  8 ms\n 3  * * *\n 4  * * *"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	var cands [][]string
	if runtime.GOOS == "windows" {
		cands = [][]string{{"tracert", "-d", "-h", "20", "-w", "800", addr}}
	} else {
		cands = [][]string{
			{"traceroute", "-n", "-q", "1", "-w", "1", "-m", "20", addr},
			{"tracepath", "-n", "-m", "20", addr},
		}
	}
	var lastErr error
	for _, c := range cands {
		if _, err := exec.LookPath(c[0]); err != nil {
			lastErr = err
			continue
		}
		cmd := exec.CommandContext(ctx, c[0], c[1:]...)
		hideWindow(cmd)
		out, err := cmd.CombinedOutput()
		s := strings.TrimSpace(decodeConsole(out))
		if s != "" && !(err != nil && len(s) < 80) {
			if len(s) > 4000 {
				s = s[:4000] + "\n…"
			}
			return s
		}
		if err != nil {
			lastErr = errors.New(strings.TrimSpace(s + " " + err.Error()))
		}
	}
	if lastErr == nil {
		lastErr = errors.New("kein traceroute verfügbar")
	}
	return "Routenverfolgung nicht möglich: " + lastErr.Error()
}

// decodeConsole wandelt die OEM-Ausgabe von tracert (CP850) in UTF-8, wenn sie kein gültiges UTF-8 ist.
func decodeConsole(b []byte) string {
	s := string(b)
	if runtime.GOOS != "windows" || isUTF8(s) {
		return strings.ReplaceAll(s, "\r\n", "\n")
	}
	cp850 := map[byte]rune{0x81: 'ü', 0x84: 'ä', 0x94: 'ö', 0x8e: 'Ä', 0x99: 'Ö', 0x9a: 'Ü', 0xe1: 'ß', 0x82: 'é', 0xff: ' '}
	var sb strings.Builder
	for _, c := range b {
		if c < 128 {
			sb.WriteByte(c)
		} else if r, ok := cp850[c]; ok {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('?')
		}
	}
	return strings.ReplaceAll(sb.String(), "\r\n", "\n")
}

func isUTF8(s string) bool { return strings.ToValidUTF8(s, "\x00") == s }

// ---------- Server: Anstoßen und Speichern ----------

type traceReq struct {
	ID   int64  `json:"id"`
	Addr string `json:"addr"`
}

var traceQueue = struct {
	sync.Mutex
	bySite map[string][]traceReq
}{bySite: map[string][]traceReq{}}

// requestTrace startet nach einem Ausfall die Routenverfolgung – in der Cloud direkt, sonst über den Agenten.
func (m *Monitor) requestTrace(e Event) {
	o := m.orgByID(e.Org)
	if o == nil || !planHas(o.Plan, "traceroute") || e.HostID == "" {
		return
	}
	h := m.host(e.HostID)
	if h == nil {
		return
	}
	h.mu.Lock()
	addr, site := h.cfg.Addr, h.cfg.Site
	h.mu.Unlock()
	m.setTrace(e.ID, "läuft …")
	if site == "cloud" {
		go func() { m.setTrace(e.ID, runTrace(addr)) }()
		return
	}
	traceQueue.Lock()
	q := append(traceQueue.bySite[site], traceReq{e.ID, addr})
	if len(q) > 20 {
		q = q[len(q)-20:]
	}
	traceQueue.bySite[site] = q
	traceQueue.Unlock()
}

func takeTraces(site string) []traceReq {
	traceQueue.Lock()
	defer traceQueue.Unlock()
	q := traceQueue.bySite[site]
	delete(traceQueue.bySite, site)
	return q
}

func (m *Monitor) setTrace(id int64, text string) {
	m.evMu.Lock()
	var cp *Event
	for i := len(m.events) - 1; i >= 0; i-- {
		if m.events[i].ID == id {
			m.events[i].Trace = text
			if m.events[i].End > 0 {
				c := *m.events[i]
				cp = &c
			}
			break
		}
	}
	m.evMu.Unlock()
	if cp != nil {
		m.store.AppendEvent(*cp)
	}
}

// POST /api/agent/trace {id, text}
func (s *Server) handleAgentTrace(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	var req struct {
		ID   int64  `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	if req.ID < 0 {
		deliverManualTrace(req.ID, req.Text)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	s.m.evMu.Lock()
	okEv := false
	for i := len(s.m.events) - 1; i >= 0; i-- {
		if e := s.m.events[i]; e.ID == req.ID {
			okEv = e.Site == site.ID
			break
		}
	}
	s.m.evMu.Unlock()
	if !okEv {
		http.Error(w, "Ereignis unbekannt", 404)
		return
	}
	if len(req.Text) > 4100 {
		req.Text = req.Text[:4100]
	}
	s.m.setTrace(req.ID, req.Text)
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/trace {host} – Routenverfolgung von Hand (nur Cloud-Hosts sofort, Agent-Hosts beim nächsten Kontakt).
func (s *Server) handleTraceNow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host string `json:"host"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	h := s.hostFor(r, req.Host)
	if h == nil {
		http.Error(w, "unbekannter Host", 404)
		return
	}
	p := principal(r)
	o := s.m.orgByID(p.Org)
	if o == nil || !planHas(o.Plan, "traceroute") {
		http.Error(w, "Traceroute ist im Paket Pro enthalten", 402)
		return
	}
	h.mu.Lock()
	addr, site := h.cfg.Addr, h.cfg.Site
	h.mu.Unlock()
	if site == "cloud" {
		writeJSON(w, map[string]string{"text": runTrace(addr)})
		return
	}
	// Agent: als Pseudo-Ereignis einreihen und auf die Antwort warten
	id := -time.Now().UnixNano()
	ch := make(chan string, 1)
	manualTraces.Lock()
	manualTraces.m[id] = ch
	manualTraces.Unlock()
	defer func() { manualTraces.Lock(); delete(manualTraces.m, id); manualTraces.Unlock() }()
	traceQueue.Lock()
	traceQueue.bySite[site] = append(traceQueue.bySite[site], traceReq{id, addr})
	traceQueue.Unlock()
	select {
	case t := <-ch:
		writeJSON(w, map[string]string{"text": t})
	case <-time.After(100 * time.Second):
		http.Error(w, "Der Agent hat nicht geantwortet (offline oder zu alte Version?)", 504)
	}
}

var manualTraces = struct {
	sync.Mutex
	m map[int64]chan string
}{m: map[int64]chan string{}}

func deliverManualTrace(id int64, text string) bool {
	manualTraces.Lock()
	ch, ok := manualTraces.m[id]
	manualTraces.Unlock()
	if ok {
		select {
		case ch <- text:
		default:
		}
	}
	return ok
}

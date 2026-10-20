package main

import (
	"net/http"
	"strings"
	"time"
)

// POST /api/events/ack {id, note, undo}: Ereignis quittieren („ich kümmere mich“).
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   int64  `json:"id"`
		Note string `json:"note"`
		Undo bool   `json:"undo"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p := principal(r)
	who := "Betreiber"
	if p.User != nil {
		who = p.User.Name
		if who == "" {
			who = p.User.Email
		}
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 300 {
		note = note[:300]
	}
	m := s.m
	m.evMu.Lock()
	var cp *Event
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if e.ID != req.ID || e.Org != p.Org {
			continue
		}
		if req.Undo {
			e.AckBy, e.AckAt, e.AckNote = "", 0, ""
		} else {
			e.AckBy, e.AckAt, e.AckNote = who, time.Now().UnixMilli(), note
		}
		c := *e
		cp = &c
		break
	}
	m.evMu.Unlock()
	if cp == nil {
		http.Error(w, "Ereignis unbekannt", 404)
		return
	}
	if cp.End > 0 {
		m.store.AppendEvent(*cp)
	}
	writeJSON(w, cp)
}

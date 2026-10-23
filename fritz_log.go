package main

import (
	"encoding/json"
	"strings"
)

// Fritzbox-Ereignisprotokoll dauerhaft sammeln: Die Box löscht ihr Log beim Neustart,
// PingWacht behält es (bis fritzLogMax Zeilen, neueste zuerst) und sichert es in der Datenbank.

const fritzLogMax = 2000

type fritzLogSnap struct {
	Log   []string `json:"log"`
	Known []string `json:"known,omitempty"`
}

func fritzLogKey(hostID string) string { return "fritzlog:" + hostID }

// mergeFritzLog fügt neue Zeilen (neueste zuerst) vorne an; liefert true bei Änderung.
func mergeFritzLog(old, cur []string) ([]string, bool) {
	have := make(map[string]bool, len(old))
	for _, l := range old {
		have[l] = true
	}
	var add []string
	for _, l := range cur {
		l = strings.TrimSpace(l)
		if l != "" && !have[l] {
			have[l] = true
			add = append(add, l)
		}
	}
	if len(add) == 0 {
		return old, false
	}
	out := append(add, old...)
	if len(out) > fritzLogMax {
		out = out[:fritzLogMax]
	}
	return out, true
}

// fritzLogLoad lädt beim ersten Kontakt nach einem Neustart das gesicherte Log.
func (m *Monitor) fritzLogLoad(hostID string, fh *fritzHost) {
	if fh.logLoaded {
		return
	}
	fh.logLoaded = true
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	b, err := bs.LoadBlob(fritzLogKey(hostID))
	if err != nil || len(b) == 0 {
		return
	}
	var s fritzLogSnap
	if json.Unmarshal(b, &s) != nil {
		return
	}
	fh.logHist = append(fh.logHist, s.Log...)
	if len(s.Known) > 0 {
		if fh.known == nil {
			fh.known = map[string]bool{}
		}
		for _, k := range s.Known {
			fh.known[k] = true
		}
	}
}

func (m *Monitor) fritzLogSave(hostID string, fh *fritzHost) {
	bs, ok := m.store.(blobStore)
	if !ok {
		return
	}
	s := fritzLogSnap{Log: append([]string(nil), fh.logHist...)}
	for k := range fh.known {
		s.Known = append(s.Known, k)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	bs.SaveBlob(fritzLogKey(hostID), b)
}

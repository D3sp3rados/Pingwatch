package main

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

var monthNames = []string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}

type hostReport struct {
	Name, Addr, Site string
	Days             []DayStat // ältester zuerst
	Total            Agg
	Outages          int
	Downtime         int64
	DataDays         int
	Losses, Spikes   int
}

func (hr hostReport) avail() float64 {
	if hr.DataDays == 0 {
		return -1
	}
	a := 1 - float64(hr.Downtime)/float64(int64(hr.DataDays)*86400000)
	if a < 0 {
		a = 0
	}
	return a
}

func dayAvail(d DayStat) float64 {
	if d.Total.N == 0 {
		return -1
	}
	a := 1 - float64(d.Downtime)/86400000
	if a < 0 {
		a = 0
	}
	return a
}

func fmtPct(v float64, dec int) string {
	if v < 0 {
		return "–"
	}
	return strings.Replace(fmt.Sprintf("%.*f %%", dec, v*100), ".", ",", 1)
}

func fmtDE(v float64, dec int) string {
	return strings.Replace(fmt.Sprintf("%.*f", dec, v), ".", ",", 1)
}

// parseMonth: "2026-09" → erster Tag; leer = Vormonat.
func parseMonth(s string) (time.Time, error) {
	if s == "" {
		n := time.Now()
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.Local).AddDate(0, -1, 0), nil
	}
	return time.ParseInLocation("2006-01", s, time.Local)
}

// buildReport erzeugt den Monatsbericht einer Organisation als PDF.
func (m *Monitor) buildReport(org string, month time.Time) ([]byte, error) {
	o := m.orgByID(org)
	if o == nil {
		return nil, fmt.Errorf("Kunde unbekannt")
	}
	first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.Local)
	last := first.AddDate(0, 1, -1)
	now := time.Now()
	if first.After(now) {
		return nil, fmt.Errorf("Monat liegt in der Zukunft")
	}
	days := last.Day()
	if last.After(now) {
		last = now
		days = now.Day()
	}
	var hrs []hostReport
	for _, h := range m.hostsOf(org) {
		h.mu.Lock()
		cfg := h.cfg
		h.mu.Unlock()
		ds := m.dayStatsAt(h, last, days)
		hr := hostReport{Name: cfg.Name, Addr: cfg.Addr, Site: m.siteNameOf(cfg.Site)}
		for i := len(ds) - 1; i >= 0; i-- {
			d := ds[i]
			hr.Days = append(hr.Days, d)
			hr.Total.Merge(d.Total)
			hr.Outages += d.Outages
			hr.Losses += d.Losses
			hr.Spikes += d.Spikes
			if d.Total.N > 0 {
				hr.DataDays++
				hr.Downtime += d.Downtime // Ausfallzeit nur an Tagen mit Messwerten zählen
			}
		}
		if hr.Name == "" {
			hr.Name = cfg.Addr
		}
		hrs = append(hrs, hr)
	}
	sort.Slice(hrs, func(i, j int) bool { return strings.ToLower(hrs[i].Name) < strings.ToLower(hrs[j].Name) })
	evs := m.queryEvents(org, "", "", map[string]bool{"outage": true, "offline": true}, first.UnixMilli(), last.AddDate(0, 0, 1).UnixMilli(), 5000)
	kept := evs[:0]
	for _, e := range evs {
		if !e.Maint {
			kept = append(kept, e)
		}
	}
	evs = kept
	sort.Slice(evs, func(i, j int) bool { return evDur(evs[i]) > evDur(evs[j]) })

	title := fmt.Sprintf("Monatsbericht %s %d", monthNames[first.Month()-1], first.Year())
	p := newPDF()
	ink, mut, line := hexColor("#0f172a"), hexColor("#64748b"), hexColor("#e2e8f0")
	acc := hexColor("#0f9d76")
	const L, R = 40.0, 555.0
	prov := providerInfo()["name"]

	footer := func() {
		p.Line(L, 812, R, 812, 0.5, line)
		p.Text(L, 825, 8, false, mut, 'l', fmt.Sprintf("PingWatch · %s · erstellt am %s", prov, now.Format("02.01.2006 15:04")))
		p.Text(R, 825, 8, false, mut, 'r', fmt.Sprintf("Seite %d", p.PageNo()))
	}
	y := 0.0
	header := func(sub bool) {
		p.Rect(0, 0, p.W, 6, acc)
		if sub {
			p.Text(L, 34, 9, true, mut, 'l', o.Name+" · "+title)
			y = 56
			return
		}
		p.Text(L, 52, 22, true, ink, 'l', title)
		p.Text(L, 72, 12, false, mut, 'l', o.Name)
		p.Text(R, 52, 10, true, acc, 'r', "PingWatch")
		p.Text(R, 66, 8, false, mut, 'r', fmt.Sprintf("%s – %s", first.Format("02.01.2006"), last.Format("02.01.2006")))
		y = 100
	}
	need := func(h float64) {
		if y+h > 800 {
			footer()
			p.AddPage()
			header(true)
		}
	}
	header(false)

	// Kennzahlen
	var tot Agg
	var down int64
	var dataDays, outs int
	for _, hr := range hrs {
		tot.Merge(hr.Total)
		down += hr.Downtime
		dataDays += hr.DataDays
		outs += hr.Outages
	}
	overall := -1.0
	if dataDays > 0 {
		overall = 1 - float64(down)/float64(int64(dataDays)*86400000)
	}
	kpis := [][2]string{{"Verfügbarkeit gesamt", fmtPct(overall, 2)}, {"Ausfälle", fmt.Sprint(outs)}, {"Ausfallzeit gesamt", fmtDurShortZ(down)},
		{"Paketverlust", lossPct(tot)}, {"Überwachte Hosts", fmt.Sprint(len(hrs))}}
	kw := (R - L) / float64(len(kpis))
	p.Rect(L, y, R-L, 52, hexColor("#f1f5f9"))
	for i, k := range kpis {
		x := L + float64(i)*kw + 12
		p.Text(x, y+20, 8, false, mut, 'l', k[0])
		p.Text(x, y+40, 15, true, ink, 'l', k[1])
	}
	y += 76

	// Tabelle je Host
	cols := []struct {
		t     string
		x     float64
		align byte
	}{{"Host", L, 'l'}, {"Verfügbar", 268, 'r'}, {"Ausfälle", 312, 'r'}, {"Ausfallzeit", 368, 'r'}, {"Verlust", 410, 'r'}, {"Ø ms", 446, 'r'}, {"Max ms", 488, 'r'}, {"Spitzen", R, 'r'}}
	thead := func() {
		p.Text(L, y, 13, true, ink, 'l', "Übersicht je Host")
		y += 18
		for _, c := range cols {
			p.Text(c.x, y, 8, true, mut, c.align, c.t)
		}
		y += 5
		p.Line(L, y, R, y, 0.6, line)
		y += 13
	}
	thead()
	if len(hrs) == 0 {
		p.Text(L, y, 10, false, mut, 'l', "Keine Hosts vorhanden.")
		y += 16
	}
	for _, hr := range hrs {
		if y+22 > 800 {
			footer()
			p.AddPage()
			header(true)
			thead()
		}
		av := hr.avail()
		col := ink
		if av >= 0 && av < 0.99 {
			col = hexColor("#dc2626")
		} else if av >= 0 && av < 0.999 {
			col = hexColor("#c2410c")
		}
		p.Text(L, y, 9, true, ink, 'l', fitText(hr.Name, 9, true, 175))
		p.Text(L, y+10, 7, false, mut, 'l', fitText(hr.Addr+" · "+hr.Site, 7, false, 175))
		p.Text(268, y, 9, true, col, 'r', fmtPct(av, 2))
		p.Text(312, y, 9, false, ink, 'r', fmt.Sprint(hr.Outages))
		p.Text(368, y, 9, false, ink, 'r', fmtDurShortZ(hr.Downtime))
		p.Text(410, y, 9, false, ink, 'r', lossPct(hr.Total))
		avg, mx := "–", "–"
		if g := hr.Total.N - hr.Total.Lost; g > 0 {
			avg = fmtDE(float64(hr.Total.Sum)/float64(g), 1)
			mx = fmt.Sprint(hr.Total.Max)
		}
		p.Text(446, y, 9, false, ink, 'r', avg)
		p.Text(488, y, 9, false, ink, 'r', mx)
		p.Text(R, y, 9, false, ink, 'r', fmt.Sprint(hr.Spikes))
		y += 16
		p.Line(L, y, R, y, 0.3, line)
		y += 11
	}
	y += 10

	// Verfügbarkeit je Tag (Kacheln)
	if len(hrs) > 0 {
		y += 6
		need(60)
		p.Text(L, y, 13, true, ink, 'l', "Verfügbarkeit je Tag")
		y += 16
		gx := 200.0
		cw := (R - gx) / float64(days)
		legend := [][2]string{{"#16a34a", "ab 99,9 %"}, {"#84cc16", "ab 99 %"}, {"#f59e0b", "ab 95 %"}, {"#dc2626", "unter 95 %"}, {"#cbd5e1", "keine Daten"}}
		lx := L
		for _, lg := range legend {
			p.Rect(lx, y-7, 8, 8, hexColor(lg[0]))
			p.Text(lx+11, y, 7.5, false, mut, 'l', lg[1])
			lx += 70
		}
		y += 12
		dayHead := func() {
			for i := 0; i < days; i++ {
				if i%2 == 0 || days <= 16 {
					p.Text(gx+float64(i)*cw+cw/2, y, 6.5, false, mut, 'c', fmt.Sprint(i+1))
				}
			}
			y += 6
		}
		dayHead()
		for _, hr := range hrs {
			if y+14 > 800 {
				footer()
				p.AddPage()
				header(true)
				dayHead()
			}
			p.Text(L, y+9, 8, false, ink, 'l', fitText(hr.Name, 8, false, gx-L-8))
			for i, d := range hr.Days {
				a := dayAvail(d)
				c := "#cbd5e1"
				switch {
				case a < 0:
				case a >= 0.999:
					c = "#16a34a"
				case a >= 0.99:
					c = "#84cc16"
				case a >= 0.95:
					c = "#f59e0b"
				default:
					c = "#dc2626"
				}
				p.Rect(gx+float64(i)*cw+0.6, y, cw-1.2, 11, hexColor(c))
			}
			y += 14
		}
		y += 14
	}

	// Längste Ausfälle
	y += 6
	need(60)
	p.Text(L, y, 13, true, ink, 'l', "Längste Ausfälle")
	y += 18
	if len(evs) == 0 {
		p.Text(L, y, 10, false, mut, 'l', "Keine Ausfälle in diesem Monat.")
		y += 16
	}
	for i, e := range evs {
		if i >= 25 {
			p.Text(L, y, 8, false, mut, 'l', fmt.Sprintf("… und %d weitere", len(evs)-25))
			y += 14
			break
		}
		need(16)
		end := "läuft noch"
		if e.End > 0 {
			end = time.UnixMilli(e.End).Format("02.01. 15:04")
		}
		typ := "Ausfall"
		if e.Type == "offline" {
			typ = "Standort offline"
		}
		p.Text(L, y, 9, false, ink, 'l', time.UnixMilli(e.Start).Format("02.01. 15:04")+" – "+end)
		p.Text(170, y, 9, true, ink, 'l', fitText(e.HostName, 9, true, 200))
		p.Text(380, y, 9, false, mut, 'l', typ)
		p.Text(R, y, 9, true, ink, 'r', fmtDurShortZ(evDur(e)))
		y += 13
	}
	y += 10
	need(40)
	p.Text(L, y, 7.5, false, mut, 'l', "Verfügbarkeit = Anteil der Zeit ohne Ausfall an den Tagen mit Messwerten. Ausfall = mehrere Pings in Folge ohne Antwort (je Host einstellbar).")
	footer()
	return p.Bytes(o.Name + " – " + title), nil
}

func evDur(e Event) int64 {
	end := e.End
	if end == 0 {
		end = time.Now().UnixMilli()
	}
	return end - e.Start
}

func fmtDurShortZ(ms int64) string {
	if ms <= 0 {
		return "0"
	}
	return fmtDurShort(ms)
}

func lossPct(a Agg) string {
	if a.N == 0 {
		return "–"
	}
	v := float64(a.Lost) / float64(a.N)
	dec := 2
	if v >= 0.01 {
		dec = 1
	}
	return fmtPct(v, dec)
}

// GET /api/report.pdf?month=2026-09
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	o := s.m.orgByID(p.Org)
	if o == nil {
		http.Error(w, "Kunde unbekannt", 404)
		return
	}
	if !planHas(o.Plan, "report") {
		http.Error(w, "Der Monatsbericht ist im Paket Pro und Partner enthalten", 402)
		return
	}
	month, err := parseMonth(r.URL.Query().Get("month"))
	if err != nil {
		http.Error(w, "Monat ungültig", 400)
		return
	}
	pdf, err := s.m.buildReport(o.ID, month)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	disp := "inline"
	if r.URL.Query().Get("dl") == "1" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="PingWatch-Bericht-%s.pdf"`, disp, month.Format("2006-01")))
	w.Write(pdf)
}

// POST /api/report/send {month} – an alle Berichts-Empfänger der Org senden.
func (s *Server) handleReportSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Month string `json:"month"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p := principal(r)
	o := s.m.orgByID(p.Org)
	if o == nil || !planHas(o.Plan, "report") {
		http.Error(w, "Der Monatsbericht ist im Paket Pro und Partner enthalten", 402)
		return
	}
	month, err := parseMonth(req.Month)
	if err != nil {
		http.Error(w, "Monat ungültig", 400)
		return
	}
	n, err := s.m.sendReport(o.ID, month)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "sent": n})
}

func (m *Monitor) reportRecipients(org string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]bool{}
	var to []string
	for _, c := range m.cfg.Channels {
		if c.Org == org && c.Type == "email" && c.Report && !c.Disabled {
			for _, a := range splitAddrs(c.Target) {
				if !seen[a] {
					seen[a] = true
					to = append(to, a)
				}
			}
		}
	}
	return to
}

func (m *Monitor) sendReport(org string, month time.Time) (int, error) {
	to := m.reportRecipients(org)
	if len(to) == 0 {
		return 0, fmt.Errorf("Kein E-Mail-Alarmweg mit „Monatsbericht“ eingerichtet")
	}
	o := m.orgByID(org)
	pdf, err := m.buildReport(org, month)
	if err != nil {
		return 0, err
	}
	mn := fmt.Sprintf("%s %d", monthNames[month.Month()-1], month.Year())
	txt := fmt.Sprintf("Guten Tag,\n\nim Anhang finden Sie den PingWatch-Monatsbericht %s für %s.\n\n%s\n", mn, o.Name, dashboardURL())
	htm := fmt.Sprintf(`<div style="font-family:-apple-system,Segoe UI,Arial,sans-serif">Guten Tag,<br><br>im Anhang finden Sie den PingWatch-Monatsbericht <b>%s</b> für %s.%s</div>`,
		mn, o.Name, strings.Replace(linkHTML(dashboardURL()), " · ", "<br><br>", 1))
	err = sendMail(to, "PingWatch-Monatsbericht "+mn+" – "+o.Name, txt, htm,
		&attachment{Name: "PingWatch-Bericht-" + month.Format("2006-01") + ".pdf", Type: "application/pdf", Data: pdf})
	return len(to), err
}

// reportLoop verschickt am 1. eines Monats ab 7 Uhr den Bericht des Vormonats.
func (m *Monitor) reportLoop() {
	t := time.NewTicker(10 * time.Minute)
	for range t.C {
		now := time.Now()
		if now.Day() != 1 || now.Hour() < 7 {
			continue
		}
		prev, _ := parseMonth("")
		key := prev.Format("2006-01")
		m.mu.RLock()
		var todo []string
		for _, o := range m.cfg.Orgs {
			if !o.Disabled && o.ReportSent != key && planHas(o.Plan, "report") {
				todo = append(todo, o.ID)
			}
		}
		m.mu.RUnlock()
		for _, id := range todo {
			if len(m.reportRecipients(id)) == 0 {
				continue
			}
			n, err := m.sendReport(id, prev)
			if err != nil {
				log.Printf("Monatsbericht %s: %v", id, err)
				continue // nächster Versuch in 10 Minuten
			}
			log.Printf("Monatsbericht %s an %d Empfänger gesendet", id, n)
			m.mu.Lock()
			for i := range m.cfg.Orgs {
				if m.cfg.Orgs[i].ID == id {
					m.cfg.Orgs[i].ReportSent = key
				}
			}
			m.saveConfigLocked()
			m.mu.Unlock()
		}
	}
}

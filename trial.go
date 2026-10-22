package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Testphase für Selbst-Registrierungen.
//
// Einschalten in rstcloud (Umgebungsvariablen der App):
//   ALLOW_SIGNUP=1   Registrierung unter /signup erlauben (sonst 404)
//   TRIAL_DAYS=14    Länge der Testphase in Tagen (Standard 14)
//
// Neue Kunden bekommen Paket Pro mit 1 Standort bis TrialUntil. Danach werden sie automatisch
// auf Free zurückgestuft: Hosts über dem Free-Limit werden pausiert (nicht gelöscht), Standorte
// und Daten bleiben. Der Betreiber kann im Kunden-Dialog „Test bis“ verlängern oder ein Paket
// zuweisen (dann endet die Testphase).

func trialDays() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TRIAL_DAYS"))); err == nil && n > 0 && n <= 365 {
		return n
	}
	return 14
}

// operatorEmails: Empfänger für Betreiber-Hinweise (neue Registrierung, Testende).
func operatorEmails() []string {
	var out []string
	seen := map[string]bool{}
	for _, src := range []string{os.Getenv("PROVIDER_EMAIL"), os.Getenv("AI_REQUEST_TO")} {
		for _, e := range strings.Split(src, ",") {
			e = strings.TrimSpace(e)
			if validEmail(e) && !seen[strings.ToLower(e)] {
				seen[strings.ToLower(e)] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// mailer ist austauschbar (Tests).
var trialMail = func(to []string, subject, text string) error {
	if !smtpConfigured() || len(to) == 0 {
		return nil
	}
	return sendMail(to, subject, text, "", nil)
}

func (m *Monitor) ownerEmails(org string) []string {
	var out []string
	for _, u := range m.cfg.Users {
		if u.Org == org && u.Role == "owner" && validEmail(u.Email) {
			out = append(out, u.Email)
		}
	}
	return out
}

// trialCheck: Erinnerung 3 Tage vor Ende, Rückstufung auf Free nach Ablauf.
func (m *Monitor) trialCheck(now time.Time) {
	nowMs := now.UnixMilli()
	type job struct {
		org, name string
		to        []string
		until     int64
		end       bool
	}
	var jobs []job
	var pause []HostCfg
	m.mu.Lock()
	changed := false
	for i := range m.cfg.Orgs {
		o := &m.cfg.Orgs[i]
		if o.TrialUntil <= 0 || o.TrialEnded {
			continue
		}
		switch {
		case nowMs >= o.TrialUntil:
			o.TrialEnded = true
			o.Plan, o.Sites = "free", 0
			changed = true
			jobs = append(jobs, job{o.ID, o.Name, m.ownerEmails(o.ID), o.TrialUntil, true})
			// Hosts über dem Free-Limit pausieren (die zuerst angelegten bleiben aktiv)
			lim := o.hostLimit()
			var hs []HostCfg
			for _, h := range m.cfg.Hosts {
				if h.Org == o.ID && !h.Paused {
					hs = append(hs, h)
				}
			}
			if lim > 0 && len(hs) > lim {
				for _, h := range hs[lim:] {
					h.Paused, h.PauseUntil = true, 0
					pause = append(pause, h)
				}
			}
		case !o.TrialWarned && nowMs >= o.TrialUntil-3*24*3600*1000:
			o.TrialWarned = true
			changed = true
			jobs = append(jobs, job{o.ID, o.Name, m.ownerEmails(o.ID), o.TrialUntil, false})
		}
	}
	if changed {
		m.saveConfigLocked()
	}
	m.mu.Unlock()
	for _, h := range pause {
		m.UpsertHost(h)
	}
	link := dashboardURL()
	if link == "" {
		link = defaultPublicURL
	}
	for _, j := range jobs {
		day := time.UnixMilli(j.until).Format("02.01.2006")
		if j.end {
			log.Printf("Testphase beendet: %s → Free", j.name)
			text := fmt.Sprintf("Guten Tag,\n\ndie Testphase von PingWacht für „%s“ ist am %s abgelaufen. Ihr Konto läuft jetzt im kostenlosen Paket Free weiter (1 Standort, 5 Hosts). Weitere Hosts wurden pausiert – es wurde nichts gelöscht.\n\nSo geht es weiter: %s/#/billing\n\nFragen? Antworten Sie einfach auf diese E-Mail.\n", j.name, day, link)
			if err := trialMail(j.to, "PingWacht: Testphase beendet", text); err != nil {
				log.Printf("Testende-Mail an %v: %v", j.to, err)
			}
			trialMail(operatorEmails(), "PingWacht: Testphase beendet – "+j.name, fmt.Sprintf("Die Testphase von „%s“ ist abgelaufen, das Konto ist jetzt auf Free.\n\n%s/#/admin\n", j.name, link))
		} else {
			text := fmt.Sprintf("Guten Tag,\n\nIhre Testphase von PingWacht für „%s“ endet am %s. Damit die Überwachung danach im vollen Umfang weiterläuft, wählen Sie bitte ein Paket: %s/#/billing\n\nOhne Paket läuft Ihr Konto automatisch im kostenlosen Paket Free weiter (1 Standort, 5 Hosts).\n", j.name, day, link)
			if err := trialMail(j.to, "PingWacht: Ihre Testphase endet am "+day, text); err != nil {
				log.Printf("Test-Erinnerung an %v: %v", j.to, err)
			}
		}
	}
}

// ---------- Missbrauchsschutz für /signup ----------

var signupRL = struct {
	sync.Mutex
	hits map[string][]time.Time
}{hits: map[string][]time.Time{}}

const signupMaxPerHour = 3

// signupAllowed zählt einen Versuch und meldet, ob er noch erlaubt ist.
func signupAllowed(ip string, now time.Time) bool {
	signupRL.Lock()
	defer signupRL.Unlock()
	var keep []time.Time
	for _, t := range signupRL.hits[ip] {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	if len(keep) >= signupMaxPerHour {
		signupRL.hits[ip] = keep
		return false
	}
	signupRL.hits[ip] = append(keep, now)
	return true
}

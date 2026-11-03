package main

import (
	"crypto/hmac"
	"encoding/base64"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// „Passwort vergessen“: E-Mail angeben → (nur wenn es das Konto gibt) Mail mit Link → neues Passwort
// setzen; ist Zwei-Faktor eingerichtet, muss zusätzlich der Code eingegeben werden.
// Der Link ist signiert, 60 Minuten gültig und nur einmal verwendbar (er hängt am alten Passwort-Hash).
// Die Antwort verrät nie, ob eine Adresse registriert ist.

const resetValid = 60 * time.Minute

var resetLimit = struct {
	sync.Mutex
	byEmail map[string]time.Time
	byIP    map[string][]time.Time
}{byEmail: map[string]time.Time{}, byIP: map[string][]time.Time{}}

// resetAllowed: höchstens eine Mail je Adresse in 2 Minuten und 10 Anfragen je IP in 15 Minuten.
func resetAllowed(email, ip string, now time.Time) bool {
	resetLimit.Lock()
	defer resetLimit.Unlock()
	var keep []time.Time
	for _, t := range resetLimit.byIP[ip] {
		if now.Sub(t) < 15*time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 10 {
		resetLimit.byIP[ip] = keep
		return false
	}
	resetLimit.byIP[ip] = append(keep, now)
	if t, ok := resetLimit.byEmail[email]; ok && now.Sub(t) < 2*time.Minute {
		return false
	}
	resetLimit.byEmail[email] = now
	return true
}

func (s *Server) resetToken(u *User, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte("reset|" + u.ID + "|" + strconv.FormatInt(exp.Unix(), 10)))
	return payload + "." + s.sign(payload+"|"+userRef(u))
}

// resetUser prüft den Link und liefert den Benutzer (nil = ungültig, abgelaufen oder schon benutzt).
func (s *Server) resetUser(tok string) *User {
	i := strings.IndexByte(tok, '.')
	if i <= 0 {
		return nil
	}
	payload, sig := tok[:i], tok[i+1:]
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	p := strings.Split(string(raw), "|")
	if len(p) != 3 || p[0] != "reset" {
		return nil
	}
	exp, _ := strconv.ParseInt(p[2], 10, 64)
	if time.Now().Unix() > exp {
		return nil
	}
	u := s.m.userByID(p[1])
	if u == nil || !hmac.Equal([]byte(sig), []byte(s.sign(payload+"|"+userRef(u)))) {
		return nil
	}
	return u
}

const resetPage = `<!doctype html><html lang="de"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>PingWacht – %s</title><link rel="icon" href="/favicon.ico">
<style>:root{--bg:#f3f5f9;--card:#fff;--fg:#0f172a;--mut:#64748b;--line:#e2e8f0;--acc:#059669}
@media (prefers-color-scheme:dark){:root{--bg:#0b1120;--card:#111a2e;--fg:#e2e8f0;--mut:#8b9ab4;--line:#1e2a44;--acc:#34d399}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:15px/1.45 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
form,.box{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:28px;width:100%%;max-width:360px}
.brand{display:flex;align-items:center;gap:10px;margin-bottom:18px;font-weight:700;font-size:20px}img{width:36px;height:36px}
h1{font-size:17px;margin:0 0 10px}p{color:var(--mut);font-size:14px;margin:0 0 16px}label{font-size:13px;color:var(--mut)}
input{width:100%%;margin:6px 0 16px;padding:11px 12px;border-radius:10px;border:1px solid var(--line);background:var(--bg);color:var(--fg);font-size:16px}
button{width:100%%;padding:11px;border:0;border-radius:10px;background:var(--acc);color:#04130c;font-weight:600;font-size:15px;cursor:pointer}
.err{color:#ef4444;font-size:13px;margin:-6px 0 12px}a{color:var(--acc)}.back{display:block;text-align:center;font-size:13px;margin-top:14px}</style></head><body>
%s</body></html>`

const brandHTML = `<div class="brand"><img src="/icon.svg" alt=""><span>Ping<span style="color:var(--acc)">Wacht</span></span></div>`

func writeResetPage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer") // Link-Token nicht weitergeben
	fmt.Fprintf(w, resetPage, html.EscapeString(title), body)
}

// GET/POST /forgot
func (s *Server) handleForgot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()
		email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
		if email != "" && resetAllowed(email, clientIP(r), time.Now()) {
			if u := s.m.userByEmail(email); u != nil && (u.Expires == 0 || u.Expires > time.Now().UnixMilli()) {
				link := publicBase(r) + "/reset?t=" + url.QueryEscape(s.resetToken(u, time.Now().Add(resetValid)))
				go s.sendResetMail(u, link)
			}
		} else if email != "" {
			time.Sleep(500 * time.Millisecond)
		}
		writeResetPage(w, "Passwort vergessen", `<div class="box">`+brandHTML+`<h1>E-Mail ist unterwegs</h1>
<p>Wenn es zu <b>`+html.EscapeString(email)+`</b> einen PingWacht-Zugang gibt, bekommst du in wenigen Minuten eine E-Mail mit einem Link zum Ändern des Passworts. Der Link gilt 60 Minuten.</p>
<p>Keine E-Mail? Auch im Spam-Ordner nachsehen.</p><a class="back" href="/login">← Zur Anmeldung</a></div>`)
		return
	}
	writeResetPage(w, "Passwort vergessen", `<form method="post" action="/forgot">`+brandHTML+`<h1>Passwort vergessen</h1>
<p>Gib die E-Mail-Adresse deines Zugangs ein. Du bekommst einen Link, mit dem du ein neues Passwort festlegst.</p>
<p style="background:rgba(245,158,11,.12);border:1px solid rgba(245,158,11,.45);color:var(--fg);border-radius:10px;padding:10px 12px;font-size:13px">⚠️ <b>Achtung:</b> Wenn du das Passwort zurücksetzt, wirst du <b>überall abgemeldet</b> – auf allen Geräten, im Browser und in der App. Danach meldest du dich überall mit dem neuen Passwort an.</p>
<label for="em">E-Mail</label><input id="em" name="email" type="email" autocomplete="username" autocapitalize="none" required autofocus placeholder="name@firma.de">
<button type="submit">Link senden</button><a class="back" href="/login">← Zur Anmeldung</a></form>`)
}

func (s *Server) sendResetMail(u *User, link string) {
	name := nameSuffix(u.Name)
	txt := fmt.Sprintf("Guten Tag%s,\n\nfür Ihren PingWacht-Zugang (%s) wurde ein neues Passwort angefordert.\n\nPasswort jetzt ändern:\n%s\n\nDer Link ist 60 Minuten gültig und kann nur einmal verwendet werden.\nWenn Sie das nicht angefordert haben, ignorieren Sie diese E-Mail – Ihr Passwort bleibt unverändert.\n", name, u.Email, link)
	htm := fmt.Sprintf(`<div style="font-family:-apple-system,Segoe UI,Arial,sans-serif;max-width:560px">Guten Tag%s,<br><br>für Ihren PingWacht-Zugang (%s) wurde ein neues Passwort angefordert.<br><br>
<a href="%s" style="display:inline-block;background:#059669;color:#fff;text-decoration:none;font-weight:600;padding:11px 20px;border-radius:8px">Passwort jetzt ändern</a><br><br>
<span style="color:#64748b;font-size:13px">Der Link ist 60 Minuten gültig und kann nur einmal verwendet werden. Wenn Sie das nicht angefordert haben, ignorieren Sie diese E-Mail – Ihr Passwort bleibt unverändert.</span></div>`,
		html.EscapeString(name), html.EscapeString(u.Email), html.EscapeString(link))
	if err := sendMail([]string{u.Email}, "PingWacht: Passwort ändern", txt, htm, nil); err != nil {
		log.Printf("Passwort-Link an %s nicht gesendet: %v", u.Email, err)
	}
}

// GET/POST /reset?t=…
func (s *Server) handlePassReset(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	tok := r.FormValue("t")
	u := s.resetUser(tok)
	if u == nil {
		writeResetPage(w, "Link ungültig", `<div class="box">`+brandHTML+`<h1>Link ungültig oder abgelaufen</h1>
<p>Der Link ist nur 60 Minuten gültig und kann nur einmal verwendet werden.</p><a class="back" href="/forgot">Neuen Link anfordern</a></div>`)
		return
	}
	need2FA := u.TOTP != ""
	form := func(msg string) {
		errHTML := ""
		if msg != "" {
			errHTML = `<div class="err">` + html.EscapeString(msg) + `</div>`
		}
		tfa := ""
		if need2FA {
			tfa = `<label for="code">Zwei-Faktor-Code</label><input id="code" name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" required placeholder="6-stelliger Code aus der Authenticator-App">`
		}
		writeResetPage(w, "Neues Passwort", `<form method="post" action="/reset">`+brandHTML+`<h1>Neues Passwort</h1>
<p>Für <b>`+html.EscapeString(u.Email)+`</b>. Mindestens 8 Zeichen.</p>
<input type="hidden" name="t" value="`+html.EscapeString(tok)+`">
<label for="p1">Neues Passwort</label><input id="p1" name="p1" type="password" autocomplete="new-password" minlength="8" required autofocus>
<label for="p2">Wiederholen</label><input id="p2" name="p2" type="password" autocomplete="new-password" minlength="8" required>
`+tfa+errHTML+`<button type="submit">Passwort speichern</button><a class="back" href="/login">Abbrechen</a></form>`)
	}
	if r.Method != http.MethodPost {
		form("")
		return
	}
	p1, p2 := r.FormValue("p1"), r.FormValue("p2")
	switch {
	case len(p1) < 8:
		form("Das Passwort braucht mindestens 8 Zeichen.")
		return
	case p1 != p2:
		form("Die beiden Passwörter stimmen nicht überein.")
		return
	}
	if need2FA {
		if err := totpCheck(u.ID, u.TOTP, strings.TrimSpace(r.FormValue("code")), time.Now()); err != nil {
			time.Sleep(800 * time.Millisecond)
			form("Zwei-Faktor-Code: " + err.Error())
			return
		}
	}
	h := hashPassword(p1)
	s.m.mu.Lock()
	for i := range s.m.cfg.Users {
		if s.m.cfg.Users[i].ID == u.ID {
			s.m.cfg.Users[i].PassHash = h // ändert userRef: alte Anmeldungen und dieser Link werden ungültig
			s.m.cfg.Users[i].TempPW = false
		}
	}
	s.m.auditLocked(u.Org, u.Email, "geändert", "Passwort", u.Email, "über „Passwort vergessen“ · "+clientIP(r))
	s.m.saveConfigLocked()
	s.m.mu.Unlock()
	log.Printf("Passwort über Link zurückgesetzt: %s", u.Email)
	writeResetPage(w, "Passwort geändert", `<div class="box">`+brandHTML+`<h1>Passwort geändert ✓</h1>
<p>Du kannst dich jetzt mit dem neuen Passwort anmelden. Alle bisherigen Anmeldungen (auch in der App) sind abgemeldet.</p>
<a class="back" href="/login" style="font-size:15px">Zur Anmeldung</a></div>`)
}

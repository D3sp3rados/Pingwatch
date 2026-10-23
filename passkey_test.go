package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// fakeAuthenticator simuliert einen Passkey (ES256).
type fakeAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	count  uint32
}

func cborHead(major byte, n int) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	default:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
}
func cborBytes(b []byte) []byte { return append(cborHead(2, len(b)), b...) }
func cborText(s string) []byte  { return append(cborHead(3, len(s)), s...) }
func cborInt(i int) []byte {
	if i >= 0 {
		return cborHead(0, i)
	}
	return cborHead(1, -1-i)
}

func (f *fakeAuthenticator) cose() []byte {
	x, y := f.key.X.FillBytes(make([]byte, 32)), f.key.Y.FillBytes(make([]byte, 32))
	b := cborHead(5, 5)
	b = append(b, cborInt(1)...)
	b = append(b, cborInt(2)...)
	b = append(b, cborInt(3)...)
	b = append(b, cborInt(-7)...)
	b = append(b, cborInt(-1)...)
	b = append(b, cborInt(1)...)
	b = append(b, cborInt(-2)...)
	b = append(b, cborBytes(x)...)
	b = append(b, cborInt(-3)...)
	b = append(b, cborBytes(y)...)
	return b
}

func (f *fakeAuthenticator) authData(rp string, attested bool) []byte {
	h := sha256.Sum256([]byte(rp))
	flags := byte(0x05)
	if attested {
		flags |= 0x40
	}
	f.count++
	b := append(h[:], flags, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[33:], f.count)
	if attested {
		b = append(b, make([]byte, 16)...)
		b = append(b, byte(len(f.credID)>>8), byte(len(f.credID)))
		b = append(b, f.credID...)
		b = append(b, f.cose()...)
	}
	return b
}

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": challenge, "origin": origin})
	return b
}

func (f *fakeAuthenticator) create(rp, challenge, origin string) map[string]string {
	ad := f.authData(rp, true)
	att := cborHead(5, 3)
	att = append(att, cborText("fmt")...)
	att = append(att, cborText("none")...)
	att = append(att, cborText("attStmt")...)
	att = append(att, cborHead(5, 0)...)
	att = append(att, cborText("authData")...)
	att = append(att, cborBytes(ad)...)
	return map[string]string{"clientDataJSON": b64u.EncodeToString(clientData("webauthn.create", challenge, origin)), "attestationObject": b64u.EncodeToString(att), "name": "Testgerät"}
}

func (f *fakeAuthenticator) get(rp, challenge, origin string) map[string]any {
	ad := f.authData(rp, false)
	cd := clientData("webauthn.get", challenge, origin)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte(nil), ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, f.key, digest[:])
	return map[string]any{"id": b64u.EncodeToString(f.credID), "clientDataJSON": b64u.EncodeToString(cd), "authenticatorData": b64u.EncodeToString(ad), "signature": b64u.EncodeToString(sig)}
}

func TestPasskeyFlow(t *testing.T) {
	srv, ts := newTestServer(t)
	u, _ := url.Parse(ts.URL)
	rp, origin := u.Hostname(), "http://"+u.Hostname()
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	fa := &fakeAuthenticator{key: key, credID: []byte("cred-123456789")}
	// Registrierung
	_, body := c.do("POST", "/api/passkey/options", map[string]string{"purpose": "register"})
	var opt struct {
		Challenge string              `json:"challenge"`
		RP        struct{ ID string } `json:"rp"`
	}
	json.Unmarshal([]byte(body), &opt)
	if opt.RP.ID != rp || opt.Challenge == "" {
		t.Fatal(body)
	}
	if code, b := c.do("POST", "/api/passkey/register", fa.create(rp, opt.Challenge, origin)); code != 200 {
		t.Fatal(code, b)
	}
	// dieselbe Challenge ein zweites Mal → abgelehnt
	if code, _ := c.do("POST", "/api/passkey/register", fa.create(rp, opt.Challenge, origin)); code == 200 {
		t.Fatal("Challenge doppelt verwendet")
	}
	_, list := c.do("GET", "/api/passkey", nil)
	if !strings.Contains(list, "Testgerät") {
		t.Fatal(list)
	}
	// Anmeldung ohne Passwort
	post := func(path string, v any) (*http.Response, string) {
		b, _ := json.Marshal(v)
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b2, _ := io.ReadAll(resp.Body)
		return resp, string(b2)
	}
	_, ob := post("/passkey/login/options", map[string]any{})
	json.Unmarshal([]byte(ob), &opt)
	as := fa.get(rp, opt.Challenge, origin)
	as["app"] = true
	resp, lb := post("/passkey/login", as)
	if resp.StatusCode != 200 || !strings.Contains(lb, "token") {
		t.Fatal(resp.StatusCode, lb)
	}
	var tk struct{ Token string }
	json.Unmarshal([]byte(lb), &tk)
	req, _ := http.NewRequest("GET", ts.URL+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+tk.Token)
	r2, _ := http.DefaultClient.Do(req)
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatal("Token aus Passkey-Anmeldung ungültig", r2.StatusCode)
	}
	// falsche Herkunft
	_, ob = post("/passkey/login/options", map[string]any{})
	json.Unmarshal([]byte(ob), &opt)
	if resp, _ := post("/passkey/login", fa.get(rp, opt.Challenge, "https://boese.example")); resp.StatusCode == 200 {
		t.Fatal("falsche Herkunft angenommen")
	}
	// Bestätigung (Step-up) statt Code
	_, sb := c.do("POST", "/api/passkey/options", map[string]string{"purpose": "stepup"})
	json.Unmarshal([]byte(sb), &opt)
	if code, b := c.do("POST", "/api/passkey/stepup", fa.get(rp, opt.Challenge, origin)); code != 200 {
		t.Fatal(code, b)
	}
	p := &Principal{Super: true, Role: "owner"}
	if _, err := srv.m.secondFactor(p, "passkey"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.m.secondFactor(p, "passkey"); err == nil {
		t.Fatal("Bestätigung zweimal verwendet")
	}
	// Apple-Zuordnung für die App
	ar, _ := http.Get(ts.URL + "/.well-known/apple-app-site-association")
	ab, _ := io.ReadAll(ar.Body)
	ar.Body.Close()
	aasa := string(ab)
	if !strings.Contains(aasa, "de.rst.pingwatch") {
		t.Fatal(aasa)
	}
}

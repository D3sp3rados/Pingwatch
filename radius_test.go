package main

import (
	"crypto/md5"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func radRequest(id byte, user, pw, secret string) ([]byte, []byte) {
	auth := make([]byte, 16)
	for i := range auth {
		auth[i] = byte(i * 7)
	}
	p := []byte(pw)
	for len(p)%16 != 0 || len(p) == 0 {
		p = append(p, 0)
	}
	enc := make([]byte, len(p))
	prev := auth
	for i := 0; i < len(p); i += 16 {
		h := md5.Sum(append([]byte(secret), prev...))
		for j := 0; j < 16; j++ {
			enc[i+j] = p[i+j] ^ h[j]
		}
		prev = enc[i : i+16]
	}
	attrs := append([]byte{attrUserName, byte(2 + len(user))}, user...)
	attrs = append(attrs, attrUserPassword, byte(2+len(enc)))
	attrs = append(attrs, enc...)
	pkt := make([]byte, 20+len(attrs))
	pkt[0], pkt[1] = radAccessRequest, id
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	copy(pkt[4:20], auth)
	copy(pkt[20:], attrs)
	return pkt, auth
}

func TestRadiusServer(t *testing.T) {
	u := newRadiusUser("raphi", "geheim123", false)
	g := newRadiusUser("weg", "geheim123", false)
	g.Disabled = true
	c := &radiusAgentCfg{Secret: "s3cr3t"}
	for _, x := range []RadiusUser{u, g} {
		c.Users = append(c.Users, struct {
			Name     string `json:"n"`
			Salt     string `json:"s"`
			Hash     string `json:"h"`
			Disabled bool   `json:"d,omitempty"`
		}{x.Name, x.Salt, x.Hash, x.Disabled})
	}
	a := &Agent{}
	a.radiusApply(c)
	time.Sleep(200 * time.Millisecond)
	conn, err := net.Dial("udp4", "127.0.0.1:1812")
	if err != nil {
		t.Skip(err)
	}
	defer conn.Close()
	try := func(id byte, user, pw string) byte {
		pkt, auth := radRequest(id, user, pw, "s3cr3t")
		conn.Write(pkt)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 4096)
		n, err := conn.Read(b)
		if err != nil {
			t.Fatalf("keine Antwort: %v", err)
		}
		// Response Authenticator prüfen
		cp := append([]byte(nil), b[:n]...)
		got := append([]byte(nil), cp[4:20]...)
		copy(cp[4:20], auth)
		h := md5.New()
		h.Write(cp)
		h.Write([]byte("s3cr3t"))
		if string(h.Sum(nil)) != string(got) {
			t.Fatal("Response Authenticator falsch")
		}
		return b[0]
	}
	if try(1, "raphi", "geheim123") != radAccessAccept {
		t.Fatal("erwartet Accept")
	}
	if try(2, "raphi", "falsch") != radAccessReject {
		t.Fatal("erwartet Reject")
	}
	if try(3, "weg", "geheim123") != radAccessReject {
		t.Fatal("gesperrt erwartet Reject")
	}
	radiusAgent.Lock()
	n := len(radiusAgent.pending)
	radiusAgent.Unlock()
	if n != 3 {
		t.Fatalf("Protokoll: %d", n)
	}
}

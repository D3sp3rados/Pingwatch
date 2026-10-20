package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Erweiterte Prüfungen (Paket Pro): Webseite/HTTPS inkl. Zertifikat und DNS-Auflösung.

// certExp merkt sich je Host das Ablaufdatum des TLS-Zertifikats (Unix-ms).
var certExp sync.Map

func certExpiry(hostID string) int64 {
	if v, ok := certExp.Load(hostID); ok {
		return v.(int64)
	}
	return 0
}

var httpCheckClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("zu viele Weiterleitungen")
		}
		return nil
	},
}

// normalizeURL ergänzt https:// bei Eingaben wie „www.rst.de“.
func normalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

func httpCheck(h HostCfg, timeout time.Duration) (int32, string, string) {
	u, err := url.Parse(normalizeURL(h.Addr))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return -1, "", "URL ungültig"
	}
	if timeout < 3*time.Second {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", "PingWatch/"+version+" (Verfügbarkeitsprüfung)")
	start := time.Now()
	resp, err := httpCheckClient.Do(req)
	if err != nil {
		var ce *tls.CertificateVerificationError
		switch {
		case errors.As(err, &ce):
			return -1, "", "Zertifikat ungültig: " + shortErr(ce.Err)
		case errors.Is(err, context.DeadlineExceeded):
			return -1, "", errTimeout.Error()
		}
		return -1, "", shortErr(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	rtt := tenths(time.Since(start))
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		exp := resp.TLS.PeerCertificates[0].NotAfter
		certExp.Store(h.ID, exp.UnixMilli())
	}
	exp := strings.TrimSpace(h.Expect)
	if code, err := strconv.Atoi(exp); err == nil && len(exp) == 3 {
		if resp.StatusCode != code {
			return -1, "", fmt.Sprintf("HTTP %d statt %d", resp.StatusCode, code)
		}
	} else if resp.StatusCode >= 400 {
		return -1, "", fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	} else if exp != "" && !strings.Contains(string(body), exp) {
		return -1, "", "Text „" + exp + "“ nicht gefunden"
	}
	return rtt, u.Host, ""
}

func dnsCheck(h HostCfg, timeout time.Duration) (int32, string, string) {
	name := strings.TrimSpace(h.Addr)
	if name == "" {
		return -1, "", "Name fehlt"
	}
	r := net.DefaultResolver
	srv := strings.TrimSpace(h.DNSServer)
	if srv != "" {
		if _, _, err := net.SplitHostPort(srv); err != nil {
			srv = net.JoinHostPort(srv, "53")
		}
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, srv)
		}}
	}
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	addrs, err := r.LookupHost(ctx, name)
	rtt := tenths(time.Since(start))
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) {
			if de.IsNotFound {
				return -1, "", "DNS: Name nicht gefunden"
			}
			if de.IsTimeout {
				return -1, "", "DNS: " + errTimeout.Error()
			}
		}
		return -1, "", "DNS: " + shortErr(err)
	}
	if exp := strings.TrimSpace(h.Expect); exp != "" {
		found := false
		for _, a := range addrs {
			if a == exp {
				found = true
			}
		}
		if !found {
			return -1, strings.Join(addrs, ", "), "DNS: " + exp + " fehlt in der Antwort (" + strings.Join(addrs, ", ") + ")"
		}
	}
	return rtt, strings.Join(addrs, ", "), ""
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && len(s) > 90 {
		s = s[i+2:]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func isCheckMethod(m string) bool { return m == "http" || m == "dns" || m == "snmp" }

// ---------- Zertifikats-Warnung ----------

var certWarned sync.Map // hostID → Tag der letzten Warnung

// checkCerts meldet einmal täglich Zertifikate, die in weniger als 14 Tagen ablaufen.
func (m *Monitor) checkCerts() {
	now := time.Now()
	day := now.Format("2006-01-02")
	for _, h := range m.hostList() {
		h.mu.Lock()
		cfg := h.cfg
		h.mu.Unlock()
		if cfg.Method != "http" || cfg.Paused {
			continue
		}
		exp := certExpiry(cfg.ID)
		if exp == 0 {
			continue
		}
		left := time.Until(time.UnixMilli(exp))
		if left > 14*24*time.Hour {
			continue
		}
		if v, ok := certWarned.Load(cfg.ID); ok && v.(string) == day {
			continue
		}
		certWarned.Store(cfg.ID, day)
		days := int(left.Hours() / 24)
		body := fmt.Sprintf("Das TLS-Zertifikat läuft in %d Tagen ab (%s).", days, time.UnixMilli(exp).Format("02.01.2006"))
		if left <= 0 {
			body = "Das TLS-Zertifikat ist abgelaufen (" + time.UnixMilli(exp).Format("02.01.2006") + ")."
		}
		if m.inMaint(cfg.Org, cfg.ID, now) {
			continue
		}
		m.notify(notice{"cert", cfg.ID, "🔐 Zertifikat " + cfg.Name, body, cfg.Org})
	}
}

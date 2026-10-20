package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Rundlaufzeiten werden intern in Zehntel-Millisekunden gespeichert, -1 = keine Antwort.

var simMode = os.Getenv("PINGWATCH_SIM") == "1"

var errTimeout = errors.New("Zeitüberschreitung")

type resolver struct {
	addr string
	ip   net.IP
	at   time.Time
}

func (r *resolver) get() (net.IP, error) {
	if ip := net.ParseIP(r.addr); ip != nil {
		return ip, nil
	}
	if r.ip != nil && time.Since(r.at) < 60*time.Second {
		return r.ip, nil
	}
	ips, err := net.LookupIP(r.addr)
	if err != nil || len(ips) == 0 {
		if r.ip != nil && time.Since(r.at) < 10*time.Minute {
			return r.ip, nil
		}
		return nil, fmt.Errorf("DNS: %s nicht auflösbar", r.addr)
	}
	pick := ips[0]
	for _, ip := range ips {
		if ip.To4() != nil {
			pick = ip
			break
		}
	}
	r.ip, r.at = pick, time.Now()
	return pick, nil
}

// probe führt eine einzelne Messung aus.
func probe(h HostCfg, res *resolver) (int32, string, string) {
	timeout := time.Duration(h.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Second
	}
	if h.Method == "http" && !simMode {
		return httpCheck(h, timeout)
	}
	if h.Method == "dns" && !simMode {
		return dnsCheck(h, timeout)
	}
	if h.Method == "snmp" && !simMode {
		return snmpProbe(h, timeout)
	}
	if simMode {
		if h.Method == "http" && strings.HasPrefix(normalizeURL(h.Addr), "https://") {
			certExp.Store(h.ID, time.Now().Add(10*24*time.Hour).UnixMilli())
		}
		r := simPing(h.Addr)
		if r < 0 {
			time.Sleep(timeout / 10)
			return -1, "sim", errTimeout.Error()
		}
		return r, "sim", ""
	}
	ip, err := res.get()
	if err != nil {
		return -1, "", err.Error()
	}
	var r int32
	if h.Method == "tcp" {
		r, err = tcpPing(ip, h.Port, timeout)
	} else {
		r, err = icmpPing(ip, timeout)
	}
	if err != nil {
		return -1, ip.String(), err.Error()
	}
	return r, ip.String(), ""
}

func tcpPing(ip net.IP, port int, timeout time.Duration) (int32, error) {
	if port <= 0 {
		port = 443
	}
	start := time.Now()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)), timeout)
	rtt := time.Since(start)
	if err != nil {
		// RST zurück = Gerät antwortet, Port nur geschlossen
		if errors.Is(err, syscall.ECONNREFUSED) {
			return tenths(rtt), nil
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return -1, errTimeout
		}
		return -1, fmt.Errorf("TCP: %v", err)
	}
	c.Close()
	return tenths(rtt), nil
}

func tenths(d time.Duration) int32 {
	v := int32(math.Round(float64(d.Microseconds()) / 100))
	if v < 0 {
		v = 0
	}
	return v
}

// ---- Simulation zum Testen ohne echtes Netz ----

var simMu sync.Mutex
var simState = map[string]*simHost{}

type simHost struct {
	base     float64
	lossLeft int
	spkLeft  int
	rnd      *rand.Rand
}

func simPing(addr string) int32 {
	simMu.Lock()
	defer simMu.Unlock()
	s := simState[addr]
	if s == nil {
		h := fnv.New32a()
		h.Write([]byte(addr))
		seed := int64(h.Sum32())
		r := rand.New(rand.NewSource(seed + time.Now().UnixNano()))
		s = &simHost{base: 2 + float64(seed%40), rnd: r}
		simState[addr] = s
	}
	if s.lossLeft > 0 {
		s.lossLeft--
		return -1
	}
	if s.rnd.Float64() < 0.004 {
		s.lossLeft = s.rnd.Intn(8)
		return -1
	}
	if s.rnd.Float64() < 0.003 {
		s.spkLeft = 2 + s.rnd.Intn(15)
	}
	v := s.base * (0.85 + s.rnd.Float64()*0.4)
	if s.spkLeft > 0 {
		s.spkLeft--
		v = s.base*4 + s.rnd.Float64()*200
	}
	return int32(v * 10)
}

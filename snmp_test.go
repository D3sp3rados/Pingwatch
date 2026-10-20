package main

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSNMP beantwortet GET und GETBULK aus einer kleinen MIB.
type mibVal struct {
	t byte
	v any
}

func oidLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

func encVal(m mibVal) []byte {
	switch m.t {
	case berOctet:
		return berTLV(berOctet, []byte(m.v.(string)))
	case berCnt64:
		n := m.v.(uint64)
		b := []byte{}
		for n > 0 {
			b = append([]byte{byte(n)}, b...)
			n >>= 8
		}
		if len(b) == 0 || b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		return berTLV(berCnt64, b)
	default:
		return berTLV(m.t, berIntBytes(m.v.(int64)))
	}
}

func startFakeSNMP(t *testing.T, mib map[string]mibVal) (string, int) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var keys []string
	for k := range mib {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return oidLess(keys[i], keys[j]) })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req, err := snmpDecode(buf[:n])
			if err != nil || req.Community != "geheim" {
				continue
			}
			var vbs []byte
			add := func(oid string, val []byte) {
				ob, _ := berEncodeOID(oid)
				vbs = append(vbs, berTLV(berSeq, append(berTLV(berOID, ob), val...))...)
			}
			switch req.Type {
			case pduGet:
				for _, v := range req.Vars {
					if m, ok := mib[v.OID]; ok {
						add(v.OID, encVal(m))
					} else {
						add(v.OID, berTLV(berNoInst, nil))
					}
				}
			case pduBulk:
				start := req.Vars[0].OID
				cnt := 0
				for _, k := range keys {
					if oidLess(start, k) && cnt < req.ErrIndex {
						add(k, encVal(mib[k]))
						cnt++
					}
				}
				if cnt == 0 {
					add(start, berTLV(berEndView, nil))
				}
			}
			p := berTLV(berInt, berIntBytes(int64(req.ReqID)))
			p = append(p, berTLV(berInt, []byte{0})...)
			p = append(p, berTLV(berInt, []byte{0})...)
			p = append(p, berTLV(berSeq, vbs)...)
			msg := berTLV(berInt, []byte{1})
			msg = append(msg, berTLV(berOctet, []byte("geheim"))...)
			msg = append(msg, berTLV(pduResp, p)...)
			pc.WriteTo(berTLV(berSeq, msg), addr)
		}
	}()
	_, ps, _ := net.SplitHostPort(pc.LocalAddr().String())
	port, _ := strconv.Atoi(ps)
	return "127.0.0.1", port
}

func TestSNMP(t *testing.T) {
	if b, _ := berEncodeOID("1.3.6.1.2.1.31.1.1.1.6.100000"); berDecodeOID(b) != "1.3.6.1.2.1.31.1.1.1.6.100000" {
		t.Fatal("OID-Kodierung")
	}
	mib := map[string]mibVal{
		oidSysUpTime:          {berTicks, int64(123456)},
		oidIfDescr + ".1":     {berOctet, "eth0"},
		oidIfDescr + ".2":     {berOctet, "eth1"},
		oidIfName + ".1":      {berOctet, "Port 1"},
		oidIfName + ".2":      {berOctet, "Port 2"},
		oidIfAlias + ".1":     {berOctet, "Uplink"},
		oidIfOper + ".1":      {berInt, int64(1)},
		oidIfOper + ".2":      {berInt, int64(2)},
		oidIfHighSpeed + ".1": {berGauge, int64(1000)},
		oidIfHCIn + ".1":      {berCnt64, uint64(5_000_000_000_000)},
		oidIfHCOut + ".1":     {berCnt64, uint64(42)},
		oidIfInErr + ".1":     {berCnt32, int64(3)},
		"1.3.6.1.2.1.99.1":    {berInt, int64(9)}, // außerhalb der Bäume
	}
	host, port := startFakeSNMP(t, mib)
	h := HostCfg{Addr: host, Port: port, Community: "geheim", Method: "snmp"}
	old := simMode
	simMode = false
	defer func() { simMode = old }()
	if r, _, e := snmpProbe(h, time.Second); r < 0 {
		t.Fatal("Probe:", e)
	}
	ifs, err := snmpInterfaces(h)
	if err != nil || len(ifs) != 2 {
		t.Fatalf("ifs: %v %+v", err, ifs)
	}
	p1 := ifs[0]
	if p1.Name != "Port 1" || p1.Alias != "Uplink" || !p1.Up || p1.Speed != 1000 || p1.In != 5_000_000_000_000 || !p1.HC || p1.Err != 3 || ifs[1].Up {
		t.Fatalf("Port 1: %+v / %+v", p1, ifs[1])
	}
	bad := h
	bad.Community = "falsch"
	if r, _, _ := snmpProbe(bad, 300*time.Millisecond); r >= 0 {
		t.Fatal("falsche Community muss scheitern")
	}
}

func TestIfIngestAndPortAlarm(t *testing.T) {
	s, ts := newTestServer(t)
	c := newClient(t, ts)
	c.login("", "admin-pass-123")
	hc := s.m.UpsertHost(HostCfg{Name: "Switch", Addr: "10.0.0.9", Site: "cloud", Org: "default", Method: "snmp", Watch: "2"})
	if hc.Port != 161 || hc.Community != "public" {
		t.Fatalf("normalize: %+v", hc)
	}
	h := s.m.host(hc.ID)
	t0 := time.Now().Add(-2 * time.Minute).UnixMilli()
	s.m.ingestIfs(h, t0, []ifSample{{Idx: 1, Name: "Port 1", Up: true, In: 1000, Out: 0, HC: true}, {Idx: 2, Name: "Port 2", Up: true}}, "")
	s.m.ingestIfs(h, t0+60000, []ifSample{{Idx: 1, Name: "Port 1", Up: true, In: 1000 + 7_500_000, Out: 0, HC: true}, {Idx: 2, Name: "Port 2", Up: false}}, "")
	evs := s.m.queryEvents("default", "", "", map[string]bool{"port": true}, 0, 0, 10)
	if len(evs) != 1 || !strings.Contains(evs[0].HostName, "Port 2") {
		t.Fatalf("Port-Alarm: %+v", evs)
	}
	code, body := c.do("GET", "/api/snmp?host="+hc.ID+"&if=1", nil)
	if code != 200 || !strings.Contains(body, `"in":1000000`) || !strings.Contains(body, `"series":[`) {
		t.Fatalf("snmp api: %d %s", code, body)
	}
	// Port-Überwachung ändern, Hosts-Formular löscht sie nicht
	c.do("POST", "/api/snmp/watch", map[string]any{"host": hc.ID, "watch": []int{1, 2}})
	hc2 := hc
	hc2.Watch = ""
	c.do("POST", "/api/hosts", hc2)
	if w := s.m.host(hc.ID).cfg.Watch; w != "1,2" {
		t.Fatalf("watch: %q", w)
	}
}

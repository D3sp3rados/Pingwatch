package main

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"
)

// Minimaler SNMPv2c-Client (GET und GETBULK-Walk) ohne externe Bibliotheken.

const (
	berInt     = 0x02
	berOctet   = 0x04
	berNull    = 0x05
	berOID     = 0x06
	berSeq     = 0x30
	berIP      = 0x40
	berCnt32   = 0x41
	berGauge   = 0x42
	berTicks   = 0x43
	berCnt64   = 0x46
	berNoObj   = 0x80
	berNoInst  = 0x81
	berEndView = 0x82
	pduGet     = 0xA0
	pduGetNext = 0xA1
	pduResp    = 0xA2
	pduBulk    = 0xA5
)

type snmpVar struct {
	OID  string
	Type byte
	Int  int64  // Integer, Counter, Gauge, TimeTicks
	U64  uint64 // Counter64
	Str  string // OctetString, IpAddress, OID
}

func (v snmpVar) num() uint64 {
	if v.Type == berCnt64 {
		return v.U64
	}
	return uint64(v.Int)
}

// ---- BER-Kodierung ----

func berLen(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n)}, b...)
		n >>= 8
	}
	return append([]byte{0x80 | byte(len(b))}, b...)
}

func berTLV(t byte, v []byte) []byte {
	return append(append([]byte{t}, berLen(len(v))...), v...)
}

func berIntBytes(n int64) []byte {
	b := []byte{byte(n)}
	for n > 127 || n < -128 {
		n >>= 8
		b = append([]byte{byte(n)}, b...)
	}
	return b
}

func berEncodeOID(oid string) ([]byte, error) {
	parts := strings.Split(strings.Trim(oid, "."), ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("OID ungültig: %s", oid)
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("OID ungültig: %s", oid)
		}
		nums[i] = n
	}
	out := []byte{byte(nums[0]*40 + nums[1])}
	for _, n := range nums[2:] {
		var tmp []byte
		tmp = append(tmp, byte(n&0x7f))
		for n >>= 7; n > 0; n >>= 7 {
			tmp = append([]byte{byte(n&0x7f) | 0x80}, tmp...)
		}
		out = append(out, tmp...)
	}
	return out, nil
}

func berDecodeOID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	parts := []string{strconv.Itoa(int(b[0]) / 40), strconv.Itoa(int(b[0]) % 40)}
	var n uint64
	for _, c := range b[1:] {
		n = n<<7 | uint64(c&0x7f)
		if c&0x80 == 0 {
			parts = append(parts, strconv.FormatUint(n, 10))
			n = 0
		}
	}
	return strings.Join(parts, ".")
}

func snmpEncode(community string, pdu byte, reqID int32, a, b int, oids []string) ([]byte, error) {
	var vbs []byte
	for _, o := range oids {
		ob, err := berEncodeOID(o)
		if err != nil {
			return nil, err
		}
		vbs = append(vbs, berTLV(berSeq, append(berTLV(berOID, ob), berTLV(berNull, nil)...))...)
	}
	p := berTLV(berInt, berIntBytes(int64(reqID)))
	p = append(p, berTLV(berInt, berIntBytes(int64(a)))...)
	p = append(p, berTLV(berInt, berIntBytes(int64(b)))...)
	p = append(p, berTLV(berSeq, vbs)...)
	msg := berTLV(berInt, []byte{1}) // Version 2c
	msg = append(msg, berTLV(berOctet, []byte(community))...)
	msg = append(msg, berTLV(pdu, p)...)
	return berTLV(berSeq, msg), nil
}

// ---- BER-Dekodierung ----

type berReader struct {
	b []byte
	i int
}

var errBER = errors.New("SNMP: ungültige Antwort")

func (r *berReader) next() (byte, []byte, error) {
	if r.i+2 > len(r.b) {
		return 0, nil, errBER
	}
	t := r.b[r.i]
	r.i++
	l := int(r.b[r.i])
	r.i++
	if l&0x80 != 0 {
		n := l & 0x7f
		if n == 0 || n > 4 || r.i+n > len(r.b) {
			return 0, nil, errBER
		}
		l = 0
		for k := 0; k < n; k++ {
			l = l<<8 | int(r.b[r.i])
			r.i++
		}
	}
	if l < 0 || r.i+l > len(r.b) {
		return 0, nil, errBER
	}
	v := r.b[r.i : r.i+l]
	r.i += l
	return t, v, nil
}

func berInt64(v []byte) int64 {
	var n int64
	if len(v) > 0 && v[0]&0x80 != 0 {
		n = -1
	}
	for _, c := range v {
		n = n<<8 | int64(c)
	}
	return n
}

func berUint64(v []byte) uint64 {
	var n uint64
	for _, c := range v {
		n = n<<8 | uint64(c)
	}
	return n
}

type snmpPDU struct {
	Type      byte
	ReqID     int32
	ErrStatus int
	ErrIndex  int
	Vars      []snmpVar
	Community string
}

func snmpDecode(b []byte) (*snmpPDU, error) {
	r := &berReader{b: b}
	t, msg, err := r.next()
	if err != nil || t != berSeq {
		return nil, errBER
	}
	mr := &berReader{b: msg}
	if _, _, err := mr.next(); err != nil { // Version
		return nil, err
	}
	_, comm, err := mr.next()
	if err != nil {
		return nil, err
	}
	pt, pdu, err := mr.next()
	if err != nil {
		return nil, err
	}
	out := &snmpPDU{Type: pt, Community: string(comm)}
	pr := &berReader{b: pdu}
	_, v, err := pr.next()
	if err != nil {
		return nil, err
	}
	out.ReqID = int32(berInt64(v))
	_, v, _ = pr.next()
	out.ErrStatus = int(berInt64(v))
	_, v, _ = pr.next()
	out.ErrIndex = int(berInt64(v))
	_, vbs, err := pr.next()
	if err != nil {
		return nil, err
	}
	vr := &berReader{b: vbs}
	for vr.i < len(vbs) {
		_, vb, err := vr.next()
		if err != nil {
			return nil, err
		}
		ir := &berReader{b: vb}
		_, ob, err := ir.next()
		if err != nil {
			return nil, err
		}
		vt, val, err := ir.next()
		if err != nil {
			return nil, err
		}
		sv := snmpVar{OID: berDecodeOID(ob), Type: vt}
		switch vt {
		case berInt:
			sv.Int = berInt64(val)
		case berCnt32, berGauge, berTicks:
			sv.Int = int64(berUint64(val))
		case berCnt64:
			sv.U64 = berUint64(val)
		case berOctet:
			sv.Str = string(val)
		case berIP:
			if len(val) == 4 {
				sv.Str = net.IP(val).String()
			}
		case berOID:
			sv.Str = berDecodeOID(val)
		}
		out.Vars = append(out.Vars, sv)
	}
	return out, nil
}

// ---- Client ----

type snmpClient struct {
	addr      string
	community string
	timeout   time.Duration
	retries   int
}

func newSNMP(host string, port int, community string, timeout time.Duration) *snmpClient {
	if port <= 0 {
		port = 161
	}
	if community == "" {
		community = "public"
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &snmpClient{addr: net.JoinHostPort(host, strconv.Itoa(port)), community: community, timeout: timeout, retries: 1}
}

func (c *snmpClient) do(pdu byte, a, b int, oids []string) (*snmpPDU, error) {
	conn, err := net.Dial("udp", c.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	reqID := rand.Int31()
	msg, err := snmpEncode(c.community, pdu, reqID, a, b, oids)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for try := 0; try <= c.retries; try++ {
		conn.SetDeadline(time.Now().Add(c.timeout))
		if _, err := conn.Write(msg); err != nil {
			return nil, err
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break // Zeitüberschreitung → nächster Versuch
			}
			resp, err := snmpDecode(buf[:n])
			if err != nil || resp.ReqID != reqID {
				continue
			}
			if resp.ErrStatus != 0 {
				return nil, fmt.Errorf("SNMP-Fehler %d", resp.ErrStatus)
			}
			return resp, nil
		}
	}
	return nil, errors.New("SNMP: keine Antwort (Community, Port 161/UDP und SNMP am Gerät prüfen)")
}

func (c *snmpClient) Get(oids ...string) ([]snmpVar, error) {
	r, err := c.do(pduGet, 0, 0, oids)
	if err != nil {
		return nil, err
	}
	return r.Vars, nil
}

// Walk liefert alle Werte unterhalb von root (GETBULK, max. 5000 Einträge).
func (c *snmpClient) Walk(root string) ([]snmpVar, error) {
	var out []snmpVar
	cur := root
	prefix := strings.Trim(root, ".") + "."
	for len(out) < 5000 {
		r, err := c.do(pduBulk, 0, 25, []string{cur})
		if err != nil {
			return out, err
		}
		if len(r.Vars) == 0 {
			break
		}
		done := false
		for _, v := range r.Vars {
			if v.Type == berEndView || !strings.HasPrefix(v.OID, prefix) {
				done = true
				break
			}
			out = append(out, v)
			cur = v.OID
		}
		if done {
			break
		}
	}
	return out, nil
}

// ---- Schnittstellen ----

const (
	oidSysUpTime   = "1.3.6.1.2.1.1.3.0"
	oidSysName     = "1.3.6.1.2.1.1.5.0"
	oidIfDescr     = "1.3.6.1.2.1.2.2.1.2"
	oidIfOper      = "1.3.6.1.2.1.2.2.1.8"
	oidIfInOct     = "1.3.6.1.2.1.2.2.1.10"
	oidIfInErr     = "1.3.6.1.2.1.2.2.1.14"
	oidIfOutOct    = "1.3.6.1.2.1.2.2.1.16"
	oidIfOutErr    = "1.3.6.1.2.1.2.2.1.20"
	oidIfName      = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfHCIn      = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOut     = "1.3.6.1.2.1.31.1.1.1.10"
	oidIfHighSpeed = "1.3.6.1.2.1.31.1.1.1.15"
	oidIfAlias     = "1.3.6.1.2.1.31.1.1.1.18"
)

// ifSample ist der Rohzustand einer Schnittstelle (Zähler).
type ifSample struct {
	Idx   int    `json:"i"`
	Name  string `json:"n"`
	Alias string `json:"a,omitempty"`
	Up    bool   `json:"u"`
	Speed int64  `json:"s"` // Mbit/s
	In    uint64 `json:"in"`
	Out   uint64 `json:"out"`
	Err   uint64 `json:"e"`
	HC    bool   `json:"hc,omitempty"` // 64-Bit-Zähler
}

func lastIdx(oid string) int {
	i := strings.LastIndex(oid, ".")
	n, _ := strconv.Atoi(oid[i+1:])
	return n
}

// snmpInterfaces liest die Schnittstellentabelle eines Geräts.
func snmpInterfaces(h HostCfg) ([]ifSample, error) {
	if simMode {
		return simIfs(h), nil
	}
	c := newSNMP(h.Addr, h.Port, h.Community, 3*time.Second)
	byIdx := map[int]*ifSample{}
	get := func(i int) *ifSample {
		s := byIdx[i]
		if s == nil {
			s = &ifSample{Idx: i}
			byIdx[i] = s
		}
		return s
	}
	descr, err := c.Walk(oidIfDescr)
	if err != nil && len(descr) == 0 {
		return nil, err
	}
	for _, v := range descr {
		get(lastIdx(v.OID)).Name = strings.TrimRight(v.Str, "\x00")
	}
	walk := func(oid string, f func(*ifSample, snmpVar)) {
		vs, _ := c.Walk(oid)
		for _, v := range vs {
			if s := byIdx[lastIdx(v.OID)]; s != nil {
				f(s, v)
			}
		}
	}
	walk(oidIfName, func(s *ifSample, v snmpVar) {
		if n := strings.TrimSpace(v.Str); n != "" {
			s.Name = n
		}
	})
	walk(oidIfAlias, func(s *ifSample, v snmpVar) { s.Alias = strings.TrimSpace(v.Str) })
	walk(oidIfOper, func(s *ifSample, v snmpVar) { s.Up = v.Int == 1 })
	walk(oidIfHighSpeed, func(s *ifSample, v snmpVar) { s.Speed = v.Int })
	walk(oidIfHCIn, func(s *ifSample, v snmpVar) { s.In, s.HC = v.num(), true })
	walk(oidIfHCOut, func(s *ifSample, v snmpVar) { s.Out = v.num() })
	hc := false
	for _, s := range byIdx {
		hc = hc || s.HC
	}
	if !hc {
		walk(oidIfInOct, func(s *ifSample, v snmpVar) { s.In = v.num() })
		walk(oidIfOutOct, func(s *ifSample, v snmpVar) { s.Out = v.num() })
	}
	walk(oidIfInErr, func(s *ifSample, v snmpVar) { s.Err += v.num() })
	walk(oidIfOutErr, func(s *ifSample, v snmpVar) { s.Err += v.num() })
	out := make([]ifSample, 0, len(byIdx))
	for _, s := range byIdx {
		out = append(out, *s)
	}
	sortIfs(out)
	if len(out) > 256 {
		out = out[:256]
	}
	return out, nil
}

func sortIfs(s []ifSample) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Idx < s[j-1].Idx; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// snmpProbe: Erreichbarkeit per SNMP-GET auf sysUpTime.
func snmpProbe(h HostCfg, timeout time.Duration) (int32, string, string) {
	c := newSNMP(h.Addr, h.Port, h.Community, timeout)
	c.retries = 0
	start := time.Now()
	if _, err := c.Get(oidSysUpTime); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return -1, "", errTimeout.Error()
		}
		return -1, "", err.Error()
	}
	return tenths(time.Since(start)), h.Addr, ""
}

// ---- Simulation ----

var simIfCounters = map[string][]ifSample{}

func simIfs(h HostCfg) []ifSample {
	simMu.Lock()
	defer simMu.Unlock()
	cur := simIfCounters[h.ID]
	if cur == nil {
		for i := 1; i <= 8; i++ {
			cur = append(cur, ifSample{Idx: i, Name: fmt.Sprintf("Port %d", i), Up: i != 7, Speed: 1000, HC: true})
		}
		cur[0].Alias = "Uplink"
	}
	for i := range cur {
		if cur[i].Up {
			cur[i].In += uint64(rand.Intn(40_000_000))
			cur[i].Out += uint64(rand.Intn(15_000_000))
			if rand.Intn(50) == 0 {
				cur[i].Err++
			}
		}
	}
	simIfCounters[h.ID] = cur
	return append([]ifSample(nil), cur...)
}

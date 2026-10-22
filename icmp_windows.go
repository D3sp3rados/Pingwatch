//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

var (
	modIphlp         = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreate   = modIphlp.NewProc("IcmpCreateFile")
	procIcmpClose    = modIphlp.NewProc("IcmpCloseHandle")
	procIcmpSendEcho = modIphlp.NewProc("IcmpSendEcho")
)

type icmpEchoReply struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Ttl           uint8
	Tos           uint8
	Flags         uint8
	OptionsSize   uint8
	OptionsData   uintptr
}

var icmpStatusText = map[uint32]string{
	11002: "Zielnetz nicht erreichbar",
	11003: "Ziel nicht erreichbar",
	11004: "Protokoll nicht erreichbar",
	11005: "Port nicht erreichbar",
	11010: "Zeitüberschreitung",
	11013: "TTL abgelaufen",
	11050: "Allgemeiner Fehler",
}

func icmpPing(ip net.IP, timeout time.Duration) (int32, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return -1, errors.New("nur IPv4 unterstützt")
	}
	h, _, _ := procIcmpCreate.Call()
	if h == 0 || h == ^uintptr(0) {
		return -1, errors.New("IcmpCreateFile fehlgeschlagen")
	}
	defer procIcmpClose.Call(h)

	data := []byte("PingWacht-Probe-PingWacht-Probe!")
	reply := make([]byte, int(unsafe.Sizeof(icmpEchoReply{}))+len(data)+8+64)
	addr := binary.LittleEndian.Uint32(ip4)
	ms := uint32(timeout / time.Millisecond)
	if ms == 0 {
		ms = 1000
	}
	start := time.Now()
	r, _, _ := procIcmpSendEcho.Call(h, uintptr(addr),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(ms))
	elapsed := time.Since(start)
	rep := (*icmpEchoReply)(unsafe.Pointer(&reply[0]))
	if r == 0 {
		if rep.Status != 0 {
			if t, ok := icmpStatusText[rep.Status]; ok {
				return -1, errors.New(t)
			}
		}
		return -1, errTimeout
	}
	if rep.Status != 0 {
		if t, ok := icmpStatusText[rep.Status]; ok {
			return -1, errors.New(t)
		}
		return -1, fmt.Errorf("ICMP-Status %d", rep.Status)
	}
	// Windows liefert ganze ms; unter 1 ms die gemessene Zeit nehmen
	if rep.RoundTripTime == 0 {
		v := tenths(elapsed)
		if v > 9 {
			v = 9
		}
		return v, nil
	}
	return int32(rep.RoundTripTime) * 10, nil
}

func openICMP() (int, bool, error) { return 0, false, nil }
func closeFD(fd int)               {}

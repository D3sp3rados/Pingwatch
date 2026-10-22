//go:build !windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"time"
)

var icmpSeq uint32

var errNoICMP = errors.New("ICMP nicht erlaubt – Methode TCP verwenden")

func openICMP() (int, bool, error) {
	if fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP); err == nil {
		return fd, false, nil
	}
	if fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP); err == nil {
		return fd, true, nil
	}
	return -1, false, errNoICMP
}

func icmpChecksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = (s & 0xffff) + (s >> 16)
	}
	return ^uint16(s)
}

func icmpPing(ip net.IP, timeout time.Duration) (int32, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return -1, errors.New("nur IPv4 unterstützt")
	}
	fd, raw, err := openICMP()
	if err != nil {
		return -1, err
	}
	defer syscall.Close(fd)

	seq := uint16(atomic.AddUint32(&icmpSeq, 1))
	nonce := make([]byte, 8)
	rand.Read(nonce)
	pkt := make([]byte, 8+24)
	pkt[0] = 8
	binary.BigEndian.PutUint16(pkt[4:], uint16(syscall.Getpid()&0xffff))
	binary.BigEndian.PutUint16(pkt[6:], seq)
	copy(pkt[8:], nonce)
	copy(pkt[16:], []byte("PingWacht-Probe!"))
	binary.BigEndian.PutUint16(pkt[2:], icmpChecksum(pkt))

	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], ip4)
	start := time.Now()
	deadline := start.Add(timeout)
	if err := syscall.Sendto(fd, pkt, 0, sa); err != nil {
		return -1, err
	}
	buf := make([]byte, 1500)
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			return -1, errTimeout
		}
		tv := syscall.NsecToTimeval(rem.Nanoseconds())
		syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
		n, from, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				return -1, errTimeout
			}
			return -1, err
		}
		b := buf[:n]
		if raw {
			if len(b) < 20 {
				continue
			}
			b = b[int(b[0]&0x0f)*4:]
		}
		if len(b) < 16 {
			continue
		}
		switch b[0] {
		case 0: // Echo Reply
			if binary.BigEndian.Uint16(b[6:]) != seq || !bytes.Equal(b[8:16], nonce) {
				continue
			}
			if f, ok := from.(*syscall.SockaddrInet4); ok && !bytes.Equal(f.Addr[:], ip4) {
				continue
			}
			return tenths(time.Since(start)), nil
		case 3, 11: // Unreachable / TTL exceeded (nur bei Raw-Sockets sichtbar)
			inner := b[8:]
			if len(inner) < 28 {
				continue
			}
			inner = inner[int(inner[0]&0x0f)*4:]
			if len(inner) >= 8 && binary.BigEndian.Uint16(inner[6:]) == seq {
				if b[0] == 3 {
					return -1, errors.New("Ziel nicht erreichbar")
				}
				return -1, errors.New("TTL abgelaufen")
			}
		}
	}
}

func closeFD(fd int) { syscall.Close(fd) }

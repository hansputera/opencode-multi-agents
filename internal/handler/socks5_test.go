package handler

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

// Minimal SOCKS5 (RFC 1928) no-auth proxy server for tests.

func netListen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

func socks5Serve(conn net.Conn) {
	defer conn.Close()

	// Greeting
	greet := make([]byte, 2)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return
	}
	methods := make([]byte, int(greet[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// CONNECT request
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	if hdr[1] != 0x01 {
		return
	}
	var host string
	switch hdr[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 0x03: // domain
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return
		}
		dom := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, dom); err != nil {
			return
		}
		host = string(dom)
	default:
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf)

	target, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		return
	}
	defer target.Close()

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
	}
	go pipe(target, conn)
	go pipe(conn, target)
	wg.Wait()
}

//go:build linux

// Package transparent implements a transparent TCP proxy for Linux.
//
// Mechanism:
//  1. iptables (set up by the platform package) redirects all outbound TCP to
//     our listen port using the REDIRECT target.
//  2. We use the SO_ORIGINAL_DST socket option (via getsockopt) to recover
//     the original destination that the application intended to reach.
//  3. We open a SOCKS5 connection to the proxy server and CONNECT to the
//     original destination.
//  4. Data is relayed bidirectionally.
//
// This is a completely transparent proxy — applications have no knowledge of it.
package transparent

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"
	"unsafe"

	// golang.org/x/net/proxy — SOCKS5 dialer by The Go Authors / Google (https://pkg.go.dev/golang.org/x/net/proxy)
	"golang.org/x/net/proxy"

	"github.com/mole/internal/config"
	"github.com/mole/internal/logger"
)

// Linux kernel constants for SO_ORIGINAL_DST.
const (
	solIP          = 0  // SOL_IP
	soOriginalDst  = 80 // SO_ORIGINAL_DST
)

// Proxy is a Linux transparent TCP proxy.
type Proxy struct {
	ListenAddr string
	Server     config.ServerEntry
	Auth       config.AuthConfig
	Log        *logger.Logger
	DialTimeout time.Duration

	// Callbacks for the connector — all optional.
	OnConnOpen  func() // called when a tunnel connection opens
	OnConnClose func() // called when a tunnel connection closes
	OnDialFail  func() // called when SOCKS5 dial to the upstream server fails

	serverMu sync.RWMutex
	ln       net.Listener
	stopChan chan struct{}
}

// Start begins listening for redirected TCP connections.
func (p *Proxy) Start() error {
	if p.DialTimeout == 0 {
		p.DialTimeout = 30 * time.Second
	}
	p.stopChan = make(chan struct{})

	ln, err := net.Listen("tcp", p.ListenAddr)
	if err != nil {
		return fmt.Errorf("transparent proxy: listen on %s: %w", p.ListenAddr, err)
	}
	p.ln = ln

	p.Log.Info("Transparent TCP proxy listening on %s → SOCKS5 %s",
		p.ListenAddr, p.Server.Socks5Addr())

	go p.serve()
	return nil
}

// Stop shuts down the transparent proxy.
func (p *Proxy) Stop() {
	if p.ln != nil {
		_ = p.ln.Close()
	}
	if p.stopChan != nil {
		close(p.stopChan)
	}
	p.Log.Info("Transparent proxy stopped")
}

// UpdateServer switches to a new upstream proxy server (thread-safe).
func (p *Proxy) UpdateServer(srv config.ServerEntry) {
	p.serverMu.Lock()
	p.Server = srv
	p.serverMu.Unlock()
	p.Log.Info("Transparent proxy switched to server %s (%s)", srv.Name, srv.Socks5Addr())
}

func (p *Proxy) currentServer() config.ServerEntry {
	p.serverMu.RLock()
	defer p.serverMu.RUnlock()
	return p.Server
}

// ─── Accept loop ──────────────────────────────────────────────────────────

func (p *Proxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			select {
			case <-p.stopChan:
				return
			default:
				p.Log.Error("Transparent proxy: accept error: %v", err)
				return
			}
		}
		go p.handleConn(conn.(*net.TCPConn))
	}
}

// ─── Per-connection handler ───────────────────────────────────────────────

func (p *Proxy) handleConn(clientConn *net.TCPConn) {
	if p.OnConnOpen != nil {
		p.OnConnOpen()
	}
	defer func() {
		if p.OnConnClose != nil {
			p.OnConnClose()
		}
		clientConn.Close()
	}()

	// ── Recover original destination via SO_ORIGINAL_DST ─────────────────
	origDst, err := getOriginalDst(clientConn)
	if err != nil {
		p.Log.Error("Transparent proxy: SO_ORIGINAL_DST failed (src=%s): %v",
			clientConn.RemoteAddr(), err)
		return
	}

	src := clientConn.RemoteAddr().String()
	srv := p.currentServer()
	p.Log.Debug("Transparent proxy: intercepted %s → %s", src, origDst)

	// ── Dial through SOCKS5 to original destination ───────────────────────
	var auth *proxy.Auth
	if p.Auth.Username != "" {
		auth = &proxy.Auth{User: p.Auth.Username, Password: p.Auth.Password}
	}

	dialer, err := proxy.SOCKS5("tcp", srv.Socks5Addr(), auth, &net.Dialer{
		Timeout: p.DialTimeout,
	})
	if err != nil {
		p.Log.Error("Transparent proxy: create SOCKS5 dialer: %v", err)
		if p.OnDialFail != nil {
			p.OnDialFail()
		}
		return
	}

	targetConn, err := dialer.Dial("tcp", origDst)
	if err != nil {
		p.Log.Error("Transparent proxy: SOCKS5 dial to %s via %s: %v",
			origDst, srv.Socks5Addr(), err)
		if p.OnDialFail != nil {
			p.OnDialFail()
		}
		return
	}
	defer targetConn.Close()

	p.Log.Debug("Transparent proxy: tunnel open %s → SOCKS5 → %s", src, origDst)

	// ── Relay ─────────────────────────────────────────────────────────────
	start := time.Now()
	sent, recv := relay(clientConn, targetConn.(*net.TCPConn))
	elapsed := time.Since(start).Round(time.Millisecond)

	p.Log.Debug("Transparent proxy: tunnel closed %s → %s | duration=%s sent=%d recv=%d",
		src, origDst, elapsed, sent, recv)
}

// ─── SO_ORIGINAL_DST ─────────────────────────────────────────────────────

// getOriginalDst retrieves the original destination address from an iptables-
// redirected TCP connection using the SO_ORIGINAL_DST socket option.
//
// The kernel stores the pre-NAT destination in the connection tracking entry.
// getsockopt(fd, SOL_IP, SO_ORIGINAL_DST, ...) fills a sockaddr_in:
//
//	struct sockaddr_in {
//	    uint16_t sin_family;  // AF_INET
//	    uint16_t sin_port;    // port in network byte order
//	    uint32_t sin_addr;    // IPv4 address in network byte order
//	    uint8_t  sin_zero[8]; // padding
//	};
func getOriginalDst(conn *net.TCPConn) (string, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return "", fmt.Errorf("SyscallConn: %w", err)
	}

	var origAddr [16]byte
	var ssoErr error

	err = rawConn.Control(func(fd uintptr) {
		size := uint32(16)
		_, _, errno := syscall.RawSyscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(solIP),
			uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&origAddr[0])),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			ssoErr = fmt.Errorf("getsockopt SO_ORIGINAL_DST: %w", errno)
		}
	})
	if err != nil {
		return "", err
	}
	if ssoErr != nil {
		return "", ssoErr
	}

	// Parse sockaddr_in.
	// Bytes [0:2] = sin_family (AF_INET = 2, little-endian on x86)
	// Bytes [2:4] = sin_port (big-endian)
	// Bytes [4:8] = sin_addr (network byte order = big-endian)
	port := binary.BigEndian.Uint16(origAddr[2:4])
	ip := net.IP(origAddr[4:8])

	return fmt.Sprintf("%s:%d", ip.String(), port), nil
}

// ─── Bidirectional relay ──────────────────────────────────────────────────

func relay(client, target *net.TCPConn) (sent, recv int64) {
	done := make(chan int64, 2)

	go func() {
		n, _ := copyAndClose(target, client)
		done <- n
	}()
	go func() {
		n, _ := copyAndClose(client, target)
		done <- n
	}()

	sent = <-done
	recv = <-done
	return
}

func copyAndClose(dst, src *net.TCPConn) (int64, error) {
	n, err := copyBuffer(dst, src)
	_ = dst.CloseWrite()
	return n, err
}

// copyBuffer is a minimal io.Copy equivalent that avoids allocating.
func copyBuffer(dst net.Conn, src net.Conn) (written int64, err error) {
	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw < 0 || nr < nw {
				nw = 0
			}
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
		}
		if er != nil {
			return written, er
		}
	}
}

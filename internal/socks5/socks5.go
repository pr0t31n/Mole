// Package socks5 implements a SOCKS5 proxy server (RFC 1928 / RFC 1929).
//
// Supported features:
//   - NO_AUTH and USERNAME/PASSWORD authentication methods
//   - CONNECT command (TCP tunnelling)
//   - IPv4, IPv6, and domain-name address types
//   - Per-connection goroutines for concurrent handling
package socks5

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/mole/internal/logger"
)

// ─── SOCKS5 constants (RFC 1928) ──────────────────────────────────────────

const (
	socks5Version = 0x05

	// Authentication methods.
	authNone     = 0x00
	authPassword = 0x02
	authNoAccept = 0xFF

	// Commands.
	cmdConnect = 0x01

	// Address types.
	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	// Reply codes.
	repSuccess        = 0x00
	repGeneralFailure = 0x01
	repNotAllowed     = 0x02
	repNetUnreachable = 0x03
	repHostUnreachable = 0x04
	repConnRefused    = 0x05
	repCmdNotSupported = 0x07
	repAddrNotSupported = 0x08
)

// ─── Server ───────────────────────────────────────────────────────────────

// Server is a SOCKS5 proxy server.
type Server struct {
	Addr     string // "host:port" to listen on
	Username string // empty = no authentication required
	Password string

	Log *logger.Logger

	// DialTimeout is applied when connecting to the target host.
	DialTimeout time.Duration
}

// ListenAndServe starts accepting connections.  It blocks until an error occurs.
func (s *Server) ListenAndServe() error {
	if s.DialTimeout == 0 {
		s.DialTimeout = 30 * time.Second
	}

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("socks5: listen on %s: %w", s.Addr, err)
	}
	defer ln.Close()

	s.Log.Info("SOCKS5 server listening on %s", s.Addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			// A closed listener is a clean shutdown.
			s.Log.Error("SOCKS5 accept error: %v", err)
			return err
		}
		go s.handleConn(conn)
	}
}

// ─── Connection handler ───────────────────────────────────────────────────

func (s *Server) handleConn(clientConn net.Conn) {
	defer clientConn.Close()

	remote := clientConn.RemoteAddr().String()
	s.Log.Conn("SOCKS5 new connection from %s", remote)

	start := time.Now()

	// ── Step 1: Negotiate authentication method ──────────────────────────
	if err := s.negotiateAuth(clientConn); err != nil {
		s.Log.ConnError("SOCKS5 auth negotiation failed from %s: %v", remote, err)
		return
	}

	// ── Step 2: Authenticate (if username/password configured) ───────────
	if s.Username != "" {
		if err := s.authenticateUser(clientConn, remote); err != nil {
			s.Log.ConnError("SOCKS5 authentication failed from %s: %v", remote, err)
			return
		}
		s.Log.Debug("SOCKS5 authenticated from %s", remote)
	}

	// ── Step 3: Read the CONNECT request ─────────────────────────────────
	target, err := s.readRequest(clientConn)
	if err != nil {
		s.Log.ConnError("SOCKS5 bad request from %s: %v", remote, err)
		s.sendReply(clientConn, repGeneralFailure, nil)
		return
	}

	// ── Step 4: Dial the target host ─────────────────────────────────────
	s.Log.Conn("SOCKS5 CONNECT %s → %s", remote, target)

	targetConn, err := net.DialTimeout("tcp", target, s.DialTimeout)
	if err != nil {
		s.Log.ConnError("SOCKS5 cannot reach target %s (requested by %s): %v", target, remote, err)
		s.sendReply(clientConn, mapDialError(err), nil)
		return
	}
	defer targetConn.Close()

	// Reply with success and the bound address.
	if err := s.sendReply(clientConn, repSuccess, targetConn.LocalAddr()); err != nil {
		s.Log.ConnError("SOCKS5 failed to send success reply to %s: %v", remote, err)
		return
	}

	// ── Step 5: Relay data bidirectionally ───────────────────────────────
	bytesSent, bytesRecv := relay(clientConn, targetConn)
	elapsed := time.Since(start).Round(time.Millisecond)

	s.Log.Conn("SOCKS5 closed %s → %s | duration=%s sent=%d recv=%d bytes",
		remote, target, elapsed, bytesSent, bytesRecv)
}

// ─── Auth negotiation (RFC 1928 §3) ──────────────────────────────────────

func (s *Server) negotiateAuth(conn net.Conn) error {
	// Read: VER | NMETHODS | METHODS...
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("reading method header: %w", err)
	}
	if header[0] != socks5Version {
		return fmt.Errorf("unsupported SOCKS version 0x%02x (expected 0x%02x)", header[0], socks5Version)
	}

	nMethods := int(header[1])
	if nMethods == 0 {
		return fmt.Errorf("client offered zero authentication methods")
	}

	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("reading auth methods: %w", err)
	}

	// Choose method.
	chosen := byte(authNoAccept)
	if s.Username == "" {
		// Server requires no auth — accept if client also supports it.
		for _, m := range methods {
			if m == authNone {
				chosen = authNone
				break
			}
		}
	} else {
		// Server requires username/password.
		for _, m := range methods {
			if m == authPassword {
				chosen = authPassword
				break
			}
		}
	}

	// Send chosen method.
	if _, err := conn.Write([]byte{socks5Version, chosen}); err != nil {
		return fmt.Errorf("writing method selection: %w", err)
	}

	if chosen == authNoAccept {
		return fmt.Errorf("no acceptable authentication method found in client list")
	}
	return nil
}

// ─── Username/password authentication (RFC 1929) ──────────────────────────

func (s *Server) authenticateUser(conn net.Conn, remote string) error {
	// Read: VER | ULEN | UNAME | PLEN | PASSWD
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("reading auth header: %w", err)
	}
	// RFC 1929 uses version 0x01 for sub-negotiation.
	if header[0] != 0x01 {
		return fmt.Errorf("unsupported auth sub-negotiation version 0x%02x", header[0])
	}

	ulen := int(header[1])
	uname := make([]byte, ulen)
	if _, err := io.ReadFull(conn, uname); err != nil {
		return fmt.Errorf("reading username: %w", err)
	}

	plenBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, plenBuf); err != nil {
		return fmt.Errorf("reading password length: %w", err)
	}
	passwd := make([]byte, int(plenBuf[0]))
	if _, err := io.ReadFull(conn, passwd); err != nil {
		return fmt.Errorf("reading password: %w", err)
	}

	if string(uname) != s.Username || string(passwd) != s.Password {
		_, _ = conn.Write([]byte{0x01, 0x01}) // version=1, status=failure
		return fmt.Errorf("invalid credentials for user %q from %s", string(uname), remote)
	}

	_, err := conn.Write([]byte{0x01, 0x00}) // version=1, status=success
	return err
}

// ─── Request parsing (RFC 1928 §4) ───────────────────────────────────────

// readRequest reads a SOCKS5 request and returns the "host:port" target string.
func (s *Server) readRequest(conn net.Conn) (string, error) {
	// Read: VER | CMD | RSV | ATYP
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", fmt.Errorf("reading request header: %w", err)
	}
	if header[0] != socks5Version {
		return "", fmt.Errorf("unexpected SOCKS version 0x%02x in request", header[0])
	}
	if header[1] != cmdConnect {
		s.sendReply(conn, repCmdNotSupported, nil)
		return "", fmt.Errorf("unsupported command 0x%02x (only CONNECT is supported)", header[1])
	}

	atyp := header[3]
	var host string

	switch atyp {
	case atypIPv4:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", fmt.Errorf("reading IPv4 address: %w", err)
		}
		host = net.IP(addr).String()

	case atypIPv6:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", fmt.Errorf("reading IPv6 address: %w", err)
		}
		host = "[" + net.IP(addr).String() + "]"

	case atypDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", fmt.Errorf("reading domain length: %w", err)
		}
		domain := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", fmt.Errorf("reading domain name: %w", err)
		}
		host = string(domain)

	default:
		s.sendReply(conn, repAddrNotSupported, nil)
		return "", fmt.Errorf("unsupported address type 0x%02x", atyp)
	}

	// Read port (2 bytes, big-endian).
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", fmt.Errorf("reading port: %w", err)
	}
	port := binary.BigEndian.Uint16(portBuf)

	return fmt.Sprintf("%s:%d", host, port), nil
}

// ─── Reply builder (RFC 1928 §6) ─────────────────────────────────────────

func (s *Server) sendReply(conn net.Conn, rep byte, boundAddr net.Addr) error {
	// VER | REP | RSV | ATYP | BND.ADDR | BND.PORT
	buf := []byte{socks5Version, rep, 0x00}

	if boundAddr == nil {
		// Use a zero IPv4 address.
		buf = append(buf, atypIPv4, 0, 0, 0, 0, 0, 0)
	} else {
		tcpAddr, ok := boundAddr.(*net.TCPAddr)
		if !ok || tcpAddr == nil || tcpAddr.IP == nil {
			buf = append(buf, atypIPv4, 0, 0, 0, 0, 0, 0)
		} else if ip4 := tcpAddr.IP.To4(); ip4 != nil {
			buf = append(buf, atypIPv4)
			buf = append(buf, ip4...)
			buf = append(buf, byte(tcpAddr.Port>>8), byte(tcpAddr.Port))
		} else {
			buf = append(buf, atypIPv6)
			buf = append(buf, tcpAddr.IP.To16()...)
			buf = append(buf, byte(tcpAddr.Port>>8), byte(tcpAddr.Port))
		}
	}

	_, err := conn.Write(buf)
	return err
}

// ─── Bidirectional relay ──────────────────────────────────────────────────

// relay copies data between src and dst concurrently, returning total bytes
// sent (client→target) and received (target→client).
func relay(client, target net.Conn) (sent, recv int64) {
	done := make(chan int64, 2)

	go func() {
		n, _ := io.Copy(target, client)
		// Signal target that we're done sending.
		if tc, ok := target.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- n
	}()

	go func() {
		n, _ := io.Copy(client, target)
		if tc, ok := client.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- n
	}()

	sent = <-done
	recv = <-done
	return sent, recv
}

// ─── Error mapping ────────────────────────────────────────────────────────

// mapDialError converts a dial error into an appropriate SOCKS5 reply code.
func mapDialError(err error) byte {
	if err == nil {
		return repSuccess
	}
	netErr, ok := err.(*net.OpError)
	if !ok {
		return repGeneralFailure
	}
	switch netErr.Op {
	case "dial":
		if netErr.Timeout() {
			return repHostUnreachable
		}
		return repConnRefused
	}
	return repGeneralFailure
}

// Package dnsproxy provides both server-side and client-side DNS proxy implementations.
//
// Server side: accepts TCP DNS connections (from clients tunnelled through SOCKS5)
// and UDP DNS connections (local), forwarding queries to a configured upstream.
//
// Client side: accepts UDP DNS on the loopback and tunnels queries through SOCKS5.
package dnsproxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/miekg/dns"
	"github.com/mole/internal/logger"
)

// ─── Server-side DNS proxy ─────────────────────────────────────────────────

// Server listens on both UDP and TCP and forwards queries to an upstream resolver.
// TCP mode is used by clients that tunnel through SOCKS5 (DNS-over-TCP).
// UDP mode handles any direct queries.
type Server struct {
	// BindAddr is the address to listen on, e.g. "0.0.0.0:5353".
	BindAddr string

	// UpstreamDNS is the upstream resolver, e.g. "1.1.1.1:53".
	UpstreamDNS string

	Log *logger.Logger

	// QueryTimeout controls how long to wait for an upstream response.
	QueryTimeout time.Duration
}

// ListenAndServe starts both TCP and UDP listeners. It blocks until an error occurs.
func (s *Server) ListenAndServe() error {
	if s.QueryTimeout == 0 {
		s.QueryTimeout = 10 * time.Second
	}

	errCh := make(chan error, 2)

	// UDP listener.
	go func() {
		conn, err := net.ListenPacket("udp", s.BindAddr)
		if err != nil {
			errCh <- fmt.Errorf("dns-server: UDP listen on %s: %w", s.BindAddr, err)
			return
		}
		defer conn.Close()
		s.Log.Info("DNS proxy (UDP) listening on %s", s.BindAddr)
		errCh <- s.serveUDP(conn)
	}()

	// TCP listener (for clients tunnelled via SOCKS5).
	go func() {
		ln, err := net.Listen("tcp", s.BindAddr)
		if err != nil {
			errCh <- fmt.Errorf("dns-server: TCP listen on %s: %w", s.BindAddr, err)
			return
		}
		defer ln.Close()
		s.Log.Info("DNS proxy (TCP / SOCKS5 tunnel) listening on %s", s.BindAddr)
		errCh <- s.serveTCP(ln)
	}()

	// Return on first error.
	return <-errCh
}

// ─── UDP handler ──────────────────────────────────────────────────────────

func (s *Server) serveUDP(conn net.PacketConn) error {
	buf := make([]byte, 4096)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return fmt.Errorf("dns-server: UDP read: %w", err)
		}
		msg := make([]byte, n)
		copy(msg, buf[:n])
		go s.handleUDPQuery(conn, addr, msg)
	}
}

func (s *Server) handleUDPQuery(conn net.PacketConn, src net.Addr, raw []byte) {
	var q dns.Msg
	if err := q.Unpack(raw); err != nil {
		s.Log.Error("DNS proxy: malformed UDP query from %s: %v", src, err)
		return
	}

	if len(q.Question) > 0 {
		s.Log.Debug("DNS query (UDP) from %s: %s %s", src,
			q.Question[0].Name, dns.TypeToString[q.Question[0].Qtype])
	}

	resp, err := s.forwardQuery(raw, false)
	if err != nil {
		s.Log.Error("DNS proxy: upstream query failed for %s: %v", src, err)
		// Return SERVFAIL.
		var fail dns.Msg
		fail.SetReply(&q)
		fail.Rcode = dns.RcodeServerFailure
		resp, _ = fail.Pack()
	}

	if _, err := conn.WriteTo(resp, src); err != nil {
		s.Log.Error("DNS proxy: UDP write to %s: %v", src, err)
	}
}

// ─── TCP handler (DNS-over-TCP, RFC 1035 §4.2.2) ─────────────────────────

func (s *Server) serveTCP(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("dns-server: TCP accept: %w", err)
		}
		go s.handleTCPConn(conn)
	}
}

func (s *Server) handleTCPConn(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	s.Log.Debug("DNS proxy: TCP connection from %s", remote)

	for {
		// DNS-over-TCP: 2-byte big-endian length prefix followed by the message.
		var msgLen uint16
		if err := binary.Read(conn, binary.BigEndian, &msgLen); err != nil {
			if err != io.EOF {
				s.Log.Debug("DNS proxy: TCP read length from %s: %v", remote, err)
			}
			return
		}
		if msgLen == 0 {
			continue
		}

		raw := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, raw); err != nil {
			s.Log.Error("DNS proxy: TCP read message from %s: %v", remote, err)
			return
		}

		var q dns.Msg
		if err := q.Unpack(raw); err != nil {
			s.Log.Error("DNS proxy: malformed TCP DNS message from %s: %v", remote, err)
			return
		}

		if len(q.Question) > 0 {
			s.Log.Debug("DNS query (TCP) from %s: %s %s", remote,
				q.Question[0].Name, dns.TypeToString[q.Question[0].Qtype])
		}

		// Forward the query and get a response.
		resp, err := s.forwardQuery(raw, true)
		if err != nil {
			s.Log.Error("DNS proxy: upstream query failed for %s: %v", remote, err)
			var fail dns.Msg
			fail.SetReply(&q)
			fail.Rcode = dns.RcodeServerFailure
			resp, _ = fail.Pack()
		}

		// Write 2-byte length prefix then response.
		lenBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(len(resp)))
		if _, err := conn.Write(lenBuf); err != nil {
			s.Log.Error("DNS proxy: TCP write length to %s: %v", remote, err)
			return
		}
		if _, err := conn.Write(resp); err != nil {
			s.Log.Error("DNS proxy: TCP write response to %s: %v", remote, err)
			return
		}
	}
}

// ─── Upstream forwarder ───────────────────────────────────────────────────

// forwardQuery sends a raw DNS query to the upstream server and returns the raw response.
// If useTCP is true the upstream connection uses TCP; otherwise UDP.
func (s *Server) forwardQuery(raw []byte, useTCP bool) ([]byte, error) {
	network := "udp"
	if useTCP {
		network = "tcp"
	}

	conn, err := net.DialTimeout(network, s.UpstreamDNS, s.QueryTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial upstream %s/%s: %w", s.UpstreamDNS, network, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(s.QueryTimeout))

	if useTCP {
		// Length-prefixed write.
		lenBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(len(raw)))
		if _, err := conn.Write(lenBuf); err != nil {
			return nil, fmt.Errorf("write DNS length to upstream: %w", err)
		}
	}
	if _, err := conn.Write(raw); err != nil {
		return nil, fmt.Errorf("write DNS query to upstream: %w", err)
	}

	if useTCP {
		// Read length-prefixed response.
		var respLen uint16
		if err := binary.Read(conn, binary.BigEndian, &respLen); err != nil {
			return nil, fmt.Errorf("read DNS response length from upstream: %w", err)
		}
		resp := make([]byte, respLen)
		if _, err := io.ReadFull(conn, resp); err != nil {
			return nil, fmt.Errorf("read DNS response from upstream: %w", err)
		}
		return resp, nil
	}

	// UDP response.
	resp := make([]byte, 4096)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, fmt.Errorf("read DNS UDP response from upstream: %w", err)
	}
	return resp[:n], nil
}

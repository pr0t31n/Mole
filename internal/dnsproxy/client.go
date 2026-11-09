// Client-side DNS proxy.
//
// Listens on the local machine (typically 127.0.0.1:53) for UDP DNS queries
// from applications.  For each query it:
//  1. Dials the SOCKS5 proxy server.
//  2. Sends a SOCKS5 CONNECT request targeting the configured upstream DNS address.
//  3. Forwards the DNS query over TCP (DNS-over-TCP, RFC 1035 §4.2.2).
//  4. Reads the DNS response and returns it as a UDP datagram to the caller.
//
// This approach avoids the complexity of SOCKS5 UDP ASSOCIATE and is
// universally compatible with SOCKS5 servers.

package dnsproxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/mole/internal/config"
	"github.com/mole/internal/logger"
)

// ClientProxy is the client-side DNS proxy.
type ClientProxy struct {
	// ListenAddr is the local UDP address to bind on, e.g. "127.0.0.1:53".
	ListenAddr string

	// Server is the currently-active proxy server to tunnel through.
	Server config.ServerEntry

	// Auth holds optional SOCKS5 credentials.
	Auth config.AuthConfig

	// UpstreamDNS is the DNS server to ultimately query, e.g. "8.8.8.8:53".
	// Queries are sent to this address via the SOCKS5 tunnel.
	UpstreamDNS string

	Log *logger.Logger

	QueryTimeout time.Duration

	// OnDialFail is called when a SOCKS5 dial to the upstream server fails.
	OnDialFail func()

	serverMu sync.RWMutex
	conn     net.PacketConn
	stopChan chan struct{}
}

// Start begins listening for DNS queries. It returns immediately; use Stop() to halt.
func (p *ClientProxy) Start() error {
	if p.QueryTimeout == 0 {
		p.QueryTimeout = 10 * time.Second
	}
	p.stopChan = make(chan struct{})

	conn, err := net.ListenPacket("udp", p.ListenAddr)
	if err != nil {
		return fmt.Errorf("dns-client: listen on %s: %w", p.ListenAddr, err)
	}
	p.conn = conn

	p.Log.Info("DNS client proxy listening on UDP %s → tunnelling via SOCKS5 %s → %s",
		p.ListenAddr, p.Server.Socks5Addr(), p.UpstreamDNS)

	go p.serve()
	return nil
}

// Stop shuts down the DNS proxy.
func (p *ClientProxy) Stop() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	if p.stopChan != nil {
		close(p.stopChan)
	}
	p.Log.Info("DNS client proxy stopped")
}

// UpdateServer switches to a new proxy server without restarting the listener (thread-safe).
func (p *ClientProxy) UpdateServer(srv config.ServerEntry) {
	p.serverMu.Lock()
	p.Server = srv
	p.serverMu.Unlock()
	p.Log.Info("DNS client proxy switched to server %s (%s)", srv.Name, srv.Socks5Addr())
}

func (p *ClientProxy) currentServer() config.ServerEntry {
	p.serverMu.RLock()
	defer p.serverMu.RUnlock()
	return p.Server
}

// ─── Internal serving loop ────────────────────────────────────────────────

func (p *ClientProxy) serve() {
	buf := make([]byte, 4096)
	for {
		n, src, err := p.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-p.stopChan:
				return // clean shutdown
			default:
				p.Log.Error("DNS client proxy: read error: %v", err)
				return
			}
		}
		raw := make([]byte, n)
		copy(raw, buf[:n])
		go p.handleQuery(src, raw)
	}
}

func (p *ClientProxy) handleQuery(src net.Addr, raw []byte) {
	resp, err := p.tunnelQuery(raw)
	if err != nil {
		p.Log.Error("DNS client proxy: tunnel query failed (src=%s): %v", src, err)
		// Send SERVFAIL.
		resp = buildServFail(raw)
	}

	if resp == nil {
		return
	}

	if _, err := p.conn.WriteTo(resp, src); err != nil {
		p.Log.Error("DNS client proxy: write response to %s: %v", src, err)
	}
}

// tunnelQuery sends a DNS query through the SOCKS5 tunnel and returns the raw response.
func (p *ClientProxy) tunnelQuery(raw []byte) ([]byte, error) {
	srv := p.currentServer()

	// Build SOCKS5 dialer.
	var auth *proxy.Auth
	if p.Auth.Username != "" {
		auth = &proxy.Auth{User: p.Auth.Username, Password: p.Auth.Password}
	}

	dialer, err := proxy.SOCKS5("tcp", srv.Socks5Addr(), auth, proxy.Direct)
	if err != nil {
		if p.OnDialFail != nil {
			p.OnDialFail()
		}
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
	}

	// Connect through SOCKS5 to the upstream DNS server.
	conn, err := dialer.Dial("tcp", p.UpstreamDNS)
	if err != nil {
		if p.OnDialFail != nil {
			p.OnDialFail()
		}
		return nil, fmt.Errorf("SOCKS5 dial to upstream DNS %s via %s: %w",
			p.UpstreamDNS, srv.Socks5Addr(), err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(p.QueryTimeout))

	// Send DNS-over-TCP: 2-byte length prefix + query.
	lenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBuf, uint16(len(raw)))
	if _, err := conn.Write(lenBuf); err != nil {
		return nil, fmt.Errorf("write DNS length: %w", err)
	}
	if _, err := conn.Write(raw); err != nil {
		return nil, fmt.Errorf("write DNS query: %w", err)
	}

	// Read the length-prefixed response.
	var respLen uint16
	if err := binary.Read(conn, binary.BigEndian, &respLen); err != nil {
		return nil, fmt.Errorf("read DNS response length: %w", err)
	}
	resp := make([]byte, respLen)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("read DNS response body: %w", err)
	}

	return resp, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// buildServFail constructs a minimal SERVFAIL response for the given raw query.
// If the query is malformed it returns nil.
func buildServFail(raw []byte) []byte {
	if len(raw) < 12 {
		return nil
	}
	// Copy the query ID and set QR=1, RCODE=SERVFAIL (2) in the flags.
	resp := make([]byte, 12)
	copy(resp[0:2], raw[0:2])           // ID
	resp[2] = raw[2] | 0x80             // QR=1
	resp[3] = (raw[3] & 0xF0) | 0x02   // RCODE=SERVFAIL
	binary.BigEndian.PutUint16(resp[4:], 0) // QDCOUNT=0
	return resp
}

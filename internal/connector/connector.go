// Package connector manages server selection, failover, and periodic
// re-evaluation for the Mole client.
//
// It probes all configured servers concurrently, ranks them by priority
// then latency, and coordinates live server switching via registered
// callbacks when a failover or health-check-driven switch occurs.
package connector

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	// golang.org/x/net/proxy — SOCKS5 dialer by The Go Authors / Google (https://pkg.go.dev/golang.org/x/net/proxy)
	"golang.org/x/net/proxy"

	"github.com/mole/internal/config"
	"github.com/mole/internal/logger"
)

// ServerStatus holds the result of probing a single server.
type ServerStatus struct {
	Server  config.ServerEntry
	Alive   bool
	Latency time.Duration
}

// Connector manages server selection, failover, and periodic re-evaluation.
type Connector struct {
	cfg *config.Config
	log *logger.Logger

	activeMu sync.RWMutex
	active   config.ServerEntry

	// activeConns tracks how many tunnel connections are in flight.
	// The transparent proxy increments/decrements this via callbacks.
	activeConns atomic.Int64

	failCh chan struct{}
	stopCh chan struct{}

	onSwitch []func(config.ServerEntry)

	failMu    sync.Mutex
	failCount int
	failStart time.Time
}

// New creates a Connector for the given configuration.
func New(cfg *config.Config, log *logger.Logger) *Connector {
	return &Connector{
		cfg:    cfg,
		log:    log,
		failCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
}

// OnServerSwitch registers a callback invoked when the active server changes.
// Callbacks are called synchronously in the order registered.
func (c *Connector) OnServerSwitch(fn func(config.ServerEntry)) {
	c.onSwitch = append(c.onSwitch, fn)
}

// ActiveServer returns the currently selected server.
func (c *Connector) ActiveServer() config.ServerEntry {
	c.activeMu.RLock()
	defer c.activeMu.RUnlock()
	return c.active
}

// IncrConns increments the active connection count.
func (c *Connector) IncrConns() { c.activeConns.Add(1) }

// DecrConns decrements the active connection count.
func (c *Connector) DecrConns() { c.activeConns.Add(-1) }

// ActiveConns returns the number of in-flight tunnel connections.
func (c *Connector) ActiveConns() int64 { return c.activeConns.Load() }

// ─── Probing ─────────────────────────────────────────────────────────────

// ProbeAll tests all enabled servers concurrently and returns their status
// sorted by effective priority then latency.
func (c *Connector) ProbeAll() []ServerStatus {
	enabled := c.cfg.EnabledServers()
	if len(enabled) == 0 {
		return nil
	}

	results := make([]ServerStatus, len(enabled))
	var wg sync.WaitGroup
	for i, srv := range enabled {
		wg.Add(1)
		go func(idx int, s config.ServerEntry) {
			defer wg.Done()
			start := time.Now()
			err := probeServer(s, c.cfg)
			latency := time.Since(start)
			results[idx] = ServerStatus{
				Server:  s,
				Alive:   err == nil,
				Latency: latency,
			}
		}(i, srv)
	}
	wg.Wait()
	return results
}

// SelectBest picks the best server from probe results.
// Ranking: priority tier ascending (1 before 2 before 3), then latency ascending.
func SelectBest(statuses []ServerStatus) (ServerStatus, error) {
	var alive []ServerStatus
	for _, s := range statuses {
		if s.Alive {
			alive = append(alive, s)
		}
	}
	if len(alive) == 0 {
		return ServerStatus{}, fmt.Errorf("all servers unreachable")
	}

	sort.Slice(alive, func(i, j int) bool {
		pi := alive[i].Server.EffectivePriority()
		pj := alive[j].Server.EffectivePriority()
		if pi != pj {
			return pi < pj
		}
		return alive[i].Latency < alive[j].Latency
	})

	return alive[0], nil
}

// InitialSelect probes all servers concurrently and selects the best one.
// It logs a detailed report of all server statuses.
func (c *Connector) InitialSelect() (config.ServerEntry, error) {
	total := len(c.cfg.Servers)
	enabled := c.cfg.EnabledServers()

	c.log.Info("Probing %d server(s) concurrently (%d enabled, %d disabled)…",
		total, len(enabled), total-len(enabled))

	for _, srv := range c.cfg.Servers {
		if !srv.IsEnabled() {
			c.log.Info("  ⊘ %s  (%s)  — DISABLED (priority=0)", srv.Name, srv.Socks5Addr())
		}
	}

	statuses := c.ProbeAll()

	for _, s := range statuses {
		if s.Alive {
			c.log.Info("  ✓ %s  (%s)  — %v  [priority %d]",
				s.Server.Name, s.Server.Socks5Addr(),
				s.Latency.Round(time.Millisecond),
				s.Server.EffectivePriority())
		} else {
			c.log.Warn("  ✗ %s  (%s)  — UNREACHABLE  [priority %d]",
				s.Server.Name, s.Server.Socks5Addr(),
				s.Server.EffectivePriority())
		}
	}

	best, err := SelectBest(statuses)
	if err != nil {
		return config.ServerEntry{}, fmt.Errorf(
			"all %d enabled server(s) unreachable — check VPN connection and server status",
			len(enabled))
	}

	c.activeMu.Lock()
	c.active = best.Server
	c.activeMu.Unlock()

	return best.Server, nil
}

// ─── Dial failure tracking ───────────────────────────────────────────────

const (
	failoverThreshold = 3
	failoverWindow    = 30 * time.Second
)

// ReportDialFailure records a SOCKS5 dial failure. After failoverThreshold
// failures within failoverWindow, a failover is triggered.
func (c *Connector) ReportDialFailure() {
	c.failMu.Lock()
	now := time.Now()
	if now.Sub(c.failStart) > failoverWindow {
		c.failCount = 0
		c.failStart = now
	}
	c.failCount++
	count := c.failCount
	c.failMu.Unlock()

	if count >= failoverThreshold {
		select {
		case c.failCh <- struct{}{}:
		default:
		}
	}
}

// ─── Server switching ────────────────────────────────────────────────────

func (c *Connector) switchTo(srv config.ServerEntry) {
	c.activeMu.Lock()
	old := c.active
	c.active = srv
	c.activeMu.Unlock()

	c.log.Info("══════════════════════════════════════════════════════════")
	c.log.Info("  SERVER SWITCH: %s → %s", old.Name, srv.Name)
	c.log.Info("══════════════════════════════════════════════════════════")

	for _, fn := range c.onSwitch {
		fn(srv)
	}

	c.failMu.Lock()
	c.failCount = 0
	c.failMu.Unlock()
}

func (c *Connector) isSameServer(a, b config.ServerEntry) bool {
	return a.Host == b.Host && a.Socks5Port == b.Socks5Port
}

// ─── Failover loop ───────────────────────────────────────────────────────

// RunFailoverLoop blocks, listening for dial-failure signals and executing
// failover when triggered. Call in a goroutine.
func (c *Connector) RunFailoverLoop() {
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.failCh:
			c.log.Warn("Failover triggered — probing alternative servers…")
			c.doFailover()
		}
	}
}

func (c *Connector) doFailover() {
	current := c.ActiveServer()
	statuses := c.ProbeAll()

	best, err := SelectBest(statuses)
	if err != nil {
		c.log.Error("Failover: all servers unreachable — retrying in 30s")
		go func() {
			select {
			case <-time.After(30 * time.Second):
				select {
				case c.failCh <- struct{}{}:
				default:
				}
			case <-c.stopCh:
			}
		}()
		return
	}

	if c.isSameServer(best.Server, current) {
		c.log.Info("Failover: current server %s is reachable again (latency %v) — no switch",
			current.Name, best.Latency.Round(time.Millisecond))
		return
	}

	c.log.Info("Failover: switching to %s (priority %d, latency %v)",
		best.Server.Name, best.Server.EffectivePriority(),
		best.Latency.Round(time.Millisecond))
	c.switchTo(best.Server)
}

// ─── Periodic health check loop ──────────────────────────────────────────

// RunHealthCheckLoop periodically re-evaluates all servers and switches to a
// better one when safe (no active connections). Call in a goroutine.
func (c *Connector) RunHealthCheckLoop() {
	interval := c.cfg.HealthCheckInterval()
	if interval <= 0 {
		c.log.Info("Periodic health checks disabled (interval=0)")
		return
	}
	c.log.Info("Periodic health check loop started — interval %v", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.doHealthCheck()
		}
	}
}

func (c *Connector) doHealthCheck() {
	c.log.Info("Periodic health check — probing all servers…")
	current := c.ActiveServer()
	statuses := c.ProbeAll()

	for _, s := range statuses {
		if s.Alive {
			c.log.Info("  [P%d] %s: OK (%v)",
				s.Server.EffectivePriority(), s.Server.Name,
				s.Latency.Round(time.Millisecond))
		} else {
			c.log.Warn("  [P%d] %s: UNREACHABLE",
				s.Server.EffectivePriority(), s.Server.Name)
		}
	}

	best, err := SelectBest(statuses)
	if err != nil {
		c.log.Warn("Health check: all servers unreachable — staying on %s", current.Name)
		return
	}

	if c.isSameServer(best.Server, current) {
		c.log.Info("Health check: current server %s is still the best choice ✓", current.Name)
		return
	}

	conns := c.ActiveConns()
	if conns > 0 {
		c.log.Info("Health check: better server %s available (P%d) but %d active connection(s) — deferring",
			best.Server.Name, best.Server.EffectivePriority(), conns)
		return
	}

	c.log.Info("Health check: switching to %s (P%d, latency %v)",
		best.Server.Name, best.Server.EffectivePriority(),
		best.Latency.Round(time.Millisecond))
	c.switchTo(best.Server)
}

// ─── Lifecycle ───────────────────────────────────────────────────────────

// Stop signals all background goroutines to exit.
func (c *Connector) Stop() {
	close(c.stopCh)
}

// ─── SOCKS5 probe ────────────────────────────────────────────────────────

// probeServer dials the SOCKS5 server and issues a CONNECT to 1.1.1.1:80
// to verify end-to-end tunnelling is working.
func probeServer(srv config.ServerEntry, cfg *config.Config) error {
	var auth *proxy.Auth
	if cfg.Auth.Username != "" {
		auth = &proxy.Auth{User: cfg.Auth.Username, Password: cfg.Auth.Password}
	}

	dialer, err := proxy.SOCKS5("tcp", srv.Socks5Addr(), auth, &net.Dialer{
		Timeout: cfg.ConnectTimeout(),
	})
	if err != nil {
		return fmt.Errorf("build SOCKS5 dialer: %w", err)
	}

	conn, err := dialer.Dial("tcp", "1.1.1.1:80")
	if err != nil {
		return fmt.Errorf("SOCKS5 CONNECT to 1.1.1.1:80 via %s: %w", srv.Socks5Addr(), err)
	}
	_ = conn.Close()
	return nil
}

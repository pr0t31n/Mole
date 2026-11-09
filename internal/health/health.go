// Package health implements server-side health checking for Mole Server.
//
// The checker periodically probes a set of critical endpoints (internet,
// GitHub, apt/brew repositories) via TCP connect tests. When critical
// endpoints become unreachable the server transitions to "unhealthy" and
// triggers callbacks that kill and block client connections, forcing them
// to fail over to a healthy server.
package health

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mole/internal/logger"
)

// Target is a single endpoint to probe via TCP connect.
type Target struct {
	Name     string // Human-readable label for logging.
	Addr     string // "host:port" for net.DialTimeout.
	Critical bool   // If true, failure marks the server unhealthy.
}

// DefaultTargets returns the standard set of health-check endpoints.
func DefaultTargets() []Target {
	return []Target{
		{"Internet (Cloudflare)", "1.1.1.1:80", true},
		{"Internet (Google DNS)", "8.8.8.8:53", true},
		{"GitHub", "github.com:443", true},
		{"Ubuntu APT", "archive.ubuntu.com:80", false},
		{"Ubuntu Security", "security.ubuntu.com:80", false},
		{"Kali APT", "http.kali.org:80", false},
		{"Homebrew API", "formulae.brew.sh:443", false},
	}
}

// Checker performs periodic health checks and manages the healthy/unhealthy state.
type Checker struct {
	Targets  []Target
	Interval time.Duration
	Timeout  time.Duration
	Log      *logger.Logger

	// OnUnhealthy is called once when the server transitions to unhealthy.
	OnUnhealthy func()

	// OnHealthy is called once when health is restored.
	OnHealthy func()

	healthy atomic.Bool
	stopCh  chan struct{}
}

// NewChecker creates a Checker with the given parameters.
// The checker starts in the healthy state.
func NewChecker(targets []Target, interval, timeout time.Duration, log *logger.Logger) *Checker {
	c := &Checker{
		Targets:  targets,
		Interval: interval,
		Timeout:  timeout,
		Log:      log,
		stopCh:   make(chan struct{}),
	}
	c.healthy.Store(true)
	return c
}

// IsHealthy returns the current health state.
func (c *Checker) IsHealthy() bool { return c.healthy.Load() }

// Run starts the health check loop. It runs an initial check immediately,
// then repeats at the configured interval. Blocks until Stop() is called.
func (c *Checker) Run() {
	c.check()

	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.check()
		}
	}
}

// Stop signals the health check loop to exit.
func (c *Checker) Stop() { close(c.stopCh) }

// ─── Internal ────────────────────────────────────────────────────────────

type probeResult struct {
	Target Target
	OK     bool
	Err    error
}

func (c *Checker) check() {
	c.Log.Info("Health check: testing %d endpoint(s)…", len(c.Targets))

	results := make([]probeResult, len(c.Targets))
	var wg sync.WaitGroup
	for i, t := range c.Targets {
		wg.Add(1)
		go func(idx int, target Target) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", target.Addr, c.Timeout)
			if err != nil {
				results[idx] = probeResult{Target: target, OK: false, Err: err}
			} else {
				_ = conn.Close()
				results[idx] = probeResult{Target: target, OK: true}
			}
		}(i, t)
	}
	wg.Wait()

	criticalOK := true
	var failures []string

	for _, r := range results {
		if r.OK {
			c.Log.Info("  ✓ %s", r.Target.Name)
		} else {
			c.Log.Warn("  ✗ %s — %v", r.Target.Name, r.Err)
			if r.Target.Critical {
				criticalOK = false
			}
			failures = append(failures, fmt.Sprintf("%s (%v)", r.Target.Name, r.Err))
		}
	}

	wasHealthy := c.healthy.Load()

	if wasHealthy && !criticalOK {
		c.healthy.Store(false)
		c.Log.Error("══════════════════════════════════════════════════════════")
		c.Log.Error("  HEALTH CHECK FAILED — SERVER MARKED UNHEALTHY")
		c.Log.Error("  Killing and blocking all client connections")
		c.Log.Error("══════════════════════════════════════════════════════════")
		for _, f := range failures {
			c.Log.Error("  Failed: %s", f)
		}
		if c.OnUnhealthy != nil {
			c.OnUnhealthy()
		}
	} else if !wasHealthy && criticalOK {
		c.healthy.Store(true)
		c.Log.Info("══════════════════════════════════════════════════════════")
		c.Log.Info("  HEALTH RESTORED — SERVER ACCEPTING CONNECTIONS")
		c.Log.Info("══════════════════════════════════════════════════════════")
		if c.OnHealthy != nil {
			c.OnHealthy()
		}
	} else if criticalOK {
		c.Log.Info("Health check: all critical endpoints reachable ✓")
	} else {
		c.Log.Warn("Health check: still unhealthy — %d failure(s)", len(failures))
	}
}

//go:build !linux

// Package transparent provides a stub transparent proxy for non-Linux platforms.
//
// On macOS and Windows, true kernel-level transparent proxying requires
// either TUN adapters (e.g., WireGuard/tun2socks) or kernel extensions
// (WinDivert / Network Extension).  Instead, those platforms use system-wide
// SOCKS5 proxy settings via the platform package, which covers the vast
// majority of applications.
//
// This file satisfies the import so the rest of the codebase compiles on
// all target platforms.
package transparent

import (
	"fmt"
	"time"

	"github.com/mole/internal/config"
	"github.com/mole/internal/logger"
)

// Proxy is a no-op stub on non-Linux systems.
type Proxy struct {
	ListenAddr  string
	Server      config.ServerEntry
	Auth        config.AuthConfig
	Log         *logger.Logger
	DialTimeout time.Duration
}

// Start logs an informational notice on non-Linux platforms.
func (p *Proxy) Start() error {
	p.Log.Info("Transparent proxy: kernel-level interception not available on this OS.")
	p.Log.Info("All application traffic will be routed via system SOCKS5 proxy settings.")
	return nil
}

// Stop is a no-op stub.
func (p *Proxy) Stop() {}

// UpdateServer records the new server entry (used by the connector).
func (p *Proxy) UpdateServer(srv config.ServerEntry) {
	p.Server = srv
}

// IsSupported returns false on non-Linux platforms.
func IsSupported() bool { return false }

// NotSupportedErr returns an explanation of why transparent proxying is unavailable.
func NotSupportedErr() error {
	return fmt.Errorf("kernel transparent proxy requires Linux (iptables); " +
		"this platform uses system-wide SOCKS5 proxy settings instead")
}

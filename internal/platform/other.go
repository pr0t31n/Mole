//go:build !linux && !darwin && !windows

// Package platform provides a stub for unsupported platforms.
package platform

import (
	"fmt"

	"github.com/mole/internal/logger"
)

// Manager is a no-op stub for unsupported operating systems.
type Manager struct {
	setup Setup
	log   *logger.Logger
}

// NewManager creates a no-op manager for unsupported platforms.
func NewManager(s Setup, log *logger.Logger) *Manager {
	return &Manager{setup: s, log: log}
}

// Apply logs a warning that this platform is not fully supported.
func (m *Manager) Apply() error {
	m.log.Warn("Platform: This OS is not fully supported.")
	m.log.Warn("Platform: Manual proxy and DNS configuration required.")
	m.log.Warn("Platform: Set your SOCKS5 proxy to %s:%d",
		m.setup.ActiveServerHost, m.setup.ActiveServerSocks5Port)
	m.log.Warn("Platform: Set your DNS server to 127.0.0.1")
	return fmt.Errorf("unsupported platform — manual configuration required")
}

// Revert is a no-op.
func (m *Manager) Revert() {
	m.log.Info("Platform: Nothing to revert (no automatic configuration was applied)")
}

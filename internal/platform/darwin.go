//go:build darwin

// macOS platform: networksetup(8) SOCKS5 proxy + DNS override.
// Covers applications that respect SCDynamicStore proxy settings.
// For complete kernel-level interception, a TUN adapter (e.g., tun2socks) is needed.
//
// networksetup is an Apple macOS utility (https://support.apple.com/guide/mac-help/)
// Used here to configure system-wide SOCKS5 proxy and DNS resolver settings.
package platform

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/mole/internal/logger"
)

// Manager handles macOS-specific proxy and DNS configuration.
type Manager struct {
	setup      Setup
	log        *logger.Logger
	interfaces []string
	origDNS    map[string][]string
}

// NewManager creates a macOS platform manager.
func NewManager(s Setup, log *logger.Logger) *Manager {
	return &Manager{setup: s, log: log, origDNS: make(map[string][]string)}
}

// Apply configures system SOCKS5 proxy and DNS on all active interfaces.
func (m *Manager) Apply() error {
	m.log.Info("Platform (macOS): Applying system configuration…")

	ifaces, err := activeNetworkServices()
	if err != nil || len(ifaces) == 0 {
		return fmt.Errorf("platform: no active network services found: %v", err)
	}
	m.interfaces = ifaces
	m.log.Info("Platform (macOS): Active services: %s", strings.Join(ifaces, ", "))

	serverHost := m.setup.ActiveServerHost
	serverPort := m.setup.ActiveServerSocks5Port

	for _, iface := range ifaces {
		// Save original DNS.
		orig, _ := networksetupGetList(iface, "-getdnsservers")
		m.origDNS[iface] = orig

		// Set SOCKS5 proxy.
		if err := nsRun("-setsocksfirewallproxy", iface, serverHost, fmt.Sprintf("%d", serverPort)); err != nil {
			m.log.Warn("Platform (macOS): Set SOCKS5 on %q: %v", iface, err)
		} else {
			m.log.Info("Platform (macOS): SOCKS5 proxy → %s:%d on %q", serverHost, serverPort, iface)
		}
		_ = nsRun("-setsocksfirewallproxystate", iface, "on")

		// Override DNS.
		if err := nsRun("-setdnsservers", iface, "127.0.0.1"); err != nil {
			m.log.Warn("Platform (macOS): Set DNS on %q: %v", iface, err)
		} else {
			m.log.Info("Platform (macOS): DNS → 127.0.0.1 on %q", iface)
		}
	}

	m.log.Info("Platform (macOS): Configuration applied.")
	m.log.Info("Platform (macOS): NOTE — Apps bypassing system proxy will not be tunnelled.")
	return nil
}

// Revert removes proxy settings and restores original DNS.
func (m *Manager) Revert() {
	m.log.Info("Platform (macOS): Reverting system configuration…")
	for _, iface := range m.interfaces {
		_ = nsRun("-setsocksfirewallproxystate", iface, "off")

		origDNS := m.origDNS[iface]
		if len(origDNS) == 0 {
			origDNS = []string{"empty"}
		}
		args := append([]string{"-setdnsservers", iface}, origDNS...)
		if err := nsRun(args...); err != nil {
			m.log.Warn("Platform (macOS): Restore DNS on %q: %v", iface, err)
		} else {
			m.log.Info("Platform (macOS): DNS restored on %q → %v", iface, origDNS)
		}
	}
	m.log.Info("Platform (macOS): Configuration reverted.")
}

func nsRun(args ...string) error {
	out, err := exec.Command("networksetup", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w — %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func networksetupGetList(iface, flag string) ([]string, error) {
	out, err := exec.Command("networksetup", flag, iface).Output()
	if err != nil {
		return nil, err
	}
	var results []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "There aren") {
			results = append(results, line)
		}
	}
	return results, nil
}

func activeNetworkServices() ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, fmt.Errorf("networksetup -listallnetworkservices: %w", err)
	}
	var services []string
	for i, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if i == 0 || line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		infoOut, err := exec.Command("networksetup", "-getinfo", line).Output()
		if err != nil {
			continue
		}
		if strings.Contains(string(infoOut), "IP address:") {
			services = append(services, line)
		}
	}
	return services, nil
}

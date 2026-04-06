//go:build windows

// Windows platform: system SOCKS5 proxy via registry + DNS override via netsh.
// Covers apps that respect WinHTTP/WinInet proxy settings.
package platform

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/mole/internal/logger"
)

// Manager handles Windows-specific proxy and DNS configuration.
type Manager struct {
	setup    Setup
	log      *logger.Logger
	adapters []string
	origDNS  map[string]string
}

// NewManager creates a Windows platform manager.
func NewManager(s Setup, log *logger.Logger) *Manager {
	return &Manager{setup: s, log: log, origDNS: make(map[string]string)}
}

// Apply configures system SOCKS5 proxy and overrides DNS.
func (m *Manager) Apply() error {
	m.log.Info("Platform (Windows): Applying system configuration…")

	serverAddr := fmt.Sprintf("%s:%d", m.setup.ActiveServerHost, m.setup.ActiveServerSocks5Port)

	// ── Registry: Internet Settings (WinInet / browsers) ─────────────────
	regKey := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	if err := regSet(regKey, "ProxyServer", "REG_SZ", fmt.Sprintf("socks=%s", serverAddr)); err != nil {
		return fmt.Errorf("set ProxyServer registry: %w", err)
	}
	if err := regSet(regKey, "ProxyEnable", "REG_DWORD", "1"); err != nil {
		return fmt.Errorf("enable proxy registry: %w", err)
	}
	m.log.Info("Platform (Windows): System proxy (WinInet) → socks=%s", serverAddr)

	// ── WinHTTP (PowerShell, system services) ────────────────────────────
	if err := netsh("winhttp", "set", "proxy", serverAddr); err != nil {
		m.log.Warn("Platform (Windows): WinHTTP proxy set failed: %v", err)
	} else {
		m.log.Info("Platform (Windows): WinHTTP proxy → %s", serverAddr)
	}

	// ── DNS override on all active adapters ──────────────────────────────
	adapters, _ := activeAdapters()
	m.adapters = adapters
	for _, adapter := range adapters {
		orig, _ := currentDNS(adapter)
		m.origDNS[adapter] = orig
		if err := netsh("interface", "ipv4", "set", "dns", adapter, "static", "127.0.0.1"); err != nil {
			m.log.Warn("Platform (Windows): Set DNS on %q: %v", adapter, err)
		} else {
			m.log.Info("Platform (Windows): DNS → 127.0.0.1 on %q", adapter)
		}
	}

	m.log.Info("Platform (Windows): Configuration applied.")
	m.log.Info("Platform (Windows): NOTE — Apps bypassing WinHTTP will not be tunnelled.")
	return nil
}

// Revert disables proxy settings and restores DNS.
func (m *Manager) Revert() {
	m.log.Info("Platform (Windows): Reverting system configuration…")

	regKey := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	_ = regSet(regKey, "ProxyEnable", "REG_DWORD", "0")
	_ = regDel(regKey, "ProxyServer")
	_ = netsh("winhttp", "reset", "proxy")
	m.log.Info("Platform (Windows): System proxy disabled")

	for _, adapter := range m.adapters {
		orig := m.origDNS[adapter]
		if orig == "" || orig == "dhcp" {
			_ = netsh("interface", "ipv4", "set", "dns", adapter, "dhcp")
		} else {
			_ = netsh("interface", "ipv4", "set", "dns", adapter, "static", orig)
		}
		m.log.Info("Platform (Windows): DNS restored on %q → %s", adapter, orig)
	}
	m.log.Info("Platform (Windows): Configuration reverted.")
}

func regSet(key, name, t, value string) error {
	out, err := exec.Command("reg", "add", key, "/v", name, "/t", t, "/d", value, "/f").CombinedOutput()
	if err != nil {
		return fmt.Errorf("reg add %s\\%s: %w — %s", key, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func regDel(key, name string) error {
	out, err := exec.Command("reg", "delete", key, "/v", name, "/f").CombinedOutput()
	if err != nil {
		return fmt.Errorf("reg delete %s\\%s: %w — %s", key, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func netsh(args ...string) error {
	out, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh %s: %w — %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func activeAdapters() ([]string, error) {
	out, err := exec.Command("netsh", "interface", "ipv4", "show", "interfaces").Output()
	if err != nil {
		return nil, err
	}
	var adapters []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[2] == "connected" {
			adapters = append(adapters, strings.Join(fields[4:], " "))
		}
	}
	return adapters, nil
}

func currentDNS(adapter string) (string, error) {
	out, err := exec.Command("netsh", "interface", "ipv4", "show", "dns", adapter).Output()
	if err != nil {
		return "dhcp", nil
	}
	if strings.Contains(string(out), "DHCP") {
		return "dhcp", nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "DNS") {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				return parts[len(parts)-1], nil
			}
		}
	}
	return "dhcp", nil
}

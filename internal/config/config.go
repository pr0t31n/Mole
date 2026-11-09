// Package config loads and validates the proxy client configuration from a YAML file.
package config

import (
	"fmt"
	"net"
	"os"
	"time"

	// gopkg.in/yaml.v3 — YAML config parsing by Canonical Ltd / go-yaml team (https://github.com/go-yaml/yaml)
	"gopkg.in/yaml.v3"
)

// Config is the top-level client configuration structure.
type Config struct {
	Servers []ServerEntry `yaml:"servers"`
	Auth    AuthConfig    `yaml:"auth"`
	Local   LocalConfig   `yaml:"local"`
	Network NetworkConfig `yaml:"network"`
	DNS     DNSConfig     `yaml:"dns"`
	Logging LoggingConfig `yaml:"logging"`
}

// ServerEntry describes a single proxy server candidate.
type ServerEntry struct {
	Name      string `yaml:"name"`       // Human-readable label used in log messages.
	Host      string `yaml:"host"`       // IP address or hostname of the proxy server.
	Socks5Port int   `yaml:"socks5_port"` // Port the SOCKS5 server listens on.
	DNSPort   int   `yaml:"dns_port"`   // Port the server's DNS proxy listens on (TCP).

	// Priority controls server selection order.
	//   nil (omitted) = active, default tier (treated as priority 2)
	//   0             = disabled / do not use
	//   1             = highest priority
	//   2             = medium priority
	//   3             = lowest priority
	// Within the same priority tier, servers are ranked by measured latency.
	Priority *int `yaml:"priority,omitempty"`
}

// EffectivePriority returns the server's selection tier (1–3).
// Nil (unset) defaults to 2; disabled (0) returns 0.
func (s ServerEntry) EffectivePriority() int {
	if s.Priority == nil {
		return 2
	}
	return *s.Priority
}

// IsEnabled returns true if the server participates in selection.
func (s ServerEntry) IsEnabled() bool {
	return s.Priority == nil || *s.Priority > 0
}

// AuthConfig holds optional SOCKS5 username/password credentials.
type AuthConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// LocalConfig configures the ports used by the local proxy daemons.
type LocalConfig struct {
	Socks5Port int `yaml:"socks5_port"` // Port for the local transparent SOCKS5 proxy.
	DNSPort    int `yaml:"dns_port"`    // Port for the local DNS proxy (normally 53).
}

// NetworkConfig controls connectivity behaviour.
type NetworkConfig struct {
	// BypassNetworks lists CIDR ranges that should NOT be tunnelled (sent direct).
	// RFC-1918 ranges plus loopback are always bypassed even if omitted here.
	BypassNetworks []string `yaml:"bypass_networks"`

	// ConnectTimeoutSec is how long (in seconds) to wait when probing a server.
	ConnectTimeoutSec int `yaml:"connect_timeout_sec"`

	// RetryAttempts is how many times to try a single server before giving up on it.
	RetryAttempts int `yaml:"retry_attempts"`

	// RetryDelaySec is how many seconds to wait between retry attempts.
	RetryDelaySec int `yaml:"retry_delay_sec"`

	// HealthCheckIntervalMin is how often (in minutes) to re-evaluate server
	// availability and potentially switch to a higher-priority server.
	// Default: 90 (1.5 hours). Set 0 to disable periodic health checks.
	HealthCheckIntervalMin int `yaml:"health_check_interval_min"`
}

// DNSConfig configures DNS behaviour on the client.
type DNSConfig struct {
	// UpstreamDNS is the DNS server address the client will forward queries TO
	// through the SOCKS5 tunnel (e.g., "8.8.8.8:53").
	UpstreamDNS string `yaml:"upstream_dns"`
}

// LoggingConfig controls client-side logging verbosity.
type LoggingConfig struct {
	Verbose bool `yaml:"verbose"`
}

// ─── Derived / computed fields ─────────────────────────────────────────────

// ConnectTimeout returns the configured timeout as a time.Duration.
func (c *Config) ConnectTimeout() time.Duration {
	if c.Network.ConnectTimeoutSec <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.Network.ConnectTimeoutSec) * time.Second
}

// RetryAttempts returns the configured retry count with a sensible default.
func (c *Config) RetryAttempts() int {
	if c.Network.RetryAttempts <= 0 {
		return 3
	}
	return c.Network.RetryAttempts
}

// RetryDelay returns the retry delay with a sensible default.
func (c *Config) RetryDelay() time.Duration {
	if c.Network.RetryDelaySec <= 0 {
		return 2 * time.Second
	}
	return time.Duration(c.Network.RetryDelaySec) * time.Second
}

// HealthCheckInterval returns the periodic health check interval.
// Returns 0 (disabled) when explicitly set to 0; default is 90 minutes.
func (c *Config) HealthCheckInterval() time.Duration {
	if c.Network.HealthCheckIntervalMin < 0 {
		return 0
	}
	if c.Network.HealthCheckIntervalMin == 0 {
		return 90 * time.Minute
	}
	return time.Duration(c.Network.HealthCheckIntervalMin) * time.Minute
}

// BypassNets returns parsed *net.IPNet objects for all configured bypass ranges
// plus the RFC-1918 and loopback ranges that are always implicit.
func (c *Config) BypassNets() ([]*net.IPNet, error) {
	always := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16", // link-local
		"::1/128",
		"fe80::/10",
	}

	merged := append(always, c.Network.BypassNetworks...)
	nets := make([]*net.IPNet, 0, len(merged))
	for _, cidr := range merged {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("config: invalid bypass_network %q: %w", cidr, err)
		}
		nets = append(nets, ipnet)
	}
	return nets, nil
}

// EnabledServers returns only the servers that participate in selection (priority > 0 or unset).
func (c *Config) EnabledServers() []ServerEntry {
	var out []ServerEntry
	for _, s := range c.Servers {
		if s.IsEnabled() {
			out = append(out, s)
		}
	}
	return out
}

// ─── Loading ───────────────────────────────────────────────────────────────

// Load reads and validates the YAML configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: cannot read %q: %w", path, err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: YAML parse error in %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// defaultConfig returns a Config pre-populated with sensible defaults.
func defaultConfig() *Config {
	return &Config{
		Local: LocalConfig{
			Socks5Port: 1081,
			DNSPort:    53,
		},
		Network: NetworkConfig{
			ConnectTimeoutSec: 10,
			RetryAttempts:     3,
			RetryDelaySec:     2,
		},
		DNS: DNSConfig{
			UpstreamDNS: "8.8.8.8:53",
		},
		Logging: LoggingConfig{
			Verbose: true,
		},
	}
}

// validate checks that all required fields are present and sensible.
func (c *Config) validate() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("config: at least one server must be defined under 'servers'")
	}

	enabledCount := 0
	for i, s := range c.Servers {
		if s.Host == "" {
			return fmt.Errorf("config: server[%d] is missing 'host'", i)
		}
		if s.Socks5Port <= 0 || s.Socks5Port > 65535 {
			return fmt.Errorf("config: server[%d] has invalid socks5_port %d", i, s.Socks5Port)
		}
		if s.DNSPort < 0 || s.DNSPort > 65535 {
			return fmt.Errorf("config: server[%d] has invalid dns_port %d", i, s.DNSPort)
		}
		if s.Name == "" {
			c.Servers[i].Name = fmt.Sprintf("%s:%d", s.Host, s.Socks5Port)
		}
		if s.Priority != nil && (*s.Priority < 0 || *s.Priority > 3) {
			return fmt.Errorf("config: server[%d] (%s) has invalid priority %d (must be 0–3)",
				i, s.Name, *s.Priority)
		}
		if s.IsEnabled() {
			enabledCount++
		}
	}

	if enabledCount == 0 {
		return fmt.Errorf("config: all servers are disabled (priority=0); at least one must be enabled")
	}

	if c.Local.Socks5Port <= 0 || c.Local.Socks5Port > 65535 {
		return fmt.Errorf("config: local.socks5_port is invalid")
	}
	if c.Local.DNSPort <= 0 || c.Local.DNSPort > 65535 {
		return fmt.Errorf("config: local.dns_port is invalid")
	}

	// Validate bypass networks.
	for _, cidr := range c.Network.BypassNetworks {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("config: invalid bypass_network %q: %w", cidr, err)
		}
	}

	// Validate upstream DNS address.
	host, port, err := net.SplitHostPort(c.DNS.UpstreamDNS)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("config: dns.upstream_dns must be in 'host:port' format, got %q", c.DNS.UpstreamDNS)
	}

	return nil
}

// Socks5Addr returns the server SOCKS5 address as "host:port".
func (s ServerEntry) Socks5Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Socks5Port)
}

// DNSAddr returns the server DNS proxy address as "host:port".
func (s ServerEntry) DNSAddr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.DNSPort)
}

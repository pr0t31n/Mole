// moled: SOCKS5 + DNS proxy server.
//
// Usage:
//
//	moled [--config /path/to/server-config.yaml]
//
// The server exposes:
//   - A SOCKS5 proxy on the configured port (default 1080)
//   - A DNS proxy on the configured port (default 5353) for TCP DNS-over-TCP
//     connections from tunnelled clients, plus UDP for direct queries.
//
// All connections and disconnections are logged to the connection log.
// Errors are logged to the error log.
// Both log files rotate daily and are purged based on configured retention.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mole/internal/dnsproxy"
	"github.com/mole/internal/logger"
	"github.com/mole/internal/socks5"
)

// ─── Server configuration ─────────────────────────────────────────────────

// ServerConfig is the server-side YAML configuration.
type ServerConfig struct {
	Server struct {
		BindAddress string `yaml:"bind_address"`
		Socks5Port  int    `yaml:"socks5_port"`
		DNSPort     int    `yaml:"dns_port"`
	} `yaml:"server"`

	Auth struct {
		Enabled  bool   `yaml:"enabled"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"auth"`

	UpstreamDNS string `yaml:"upstream_dns"`

	Logging struct {
		Dir                  string `yaml:"dir"`
		Verbose              bool   `yaml:"verbose"`
		ConnectionRetainDays int    `yaml:"connection_retain_days"`
		ErrorRetainDays      int    `yaml:"error_retain_days"`
	} `yaml:"logging"`
}

func defaultServerConfig() *ServerConfig {
	cfg := &ServerConfig{}
	cfg.Server.BindAddress = "0.0.0.0"
	cfg.Server.Socks5Port = 1080
	cfg.Server.DNSPort = 5353
	cfg.UpstreamDNS = "1.1.1.1:53"
	cfg.Logging.Dir = "/var/log/mole"
	cfg.Logging.Verbose = true
	cfg.Logging.ConnectionRetainDays = 90
	cfg.Logging.ErrorRetainDays = 30
	return cfg
}

// ─── main ─────────────────────────────────────────────────────────────────

func main() {
	configPath := flag.String("config", "/etc/mole/moled.yaml", "Path to server configuration file")
	flag.Parse()

	// ── Load configuration ────────────────────────────────────────────────
	cfg := defaultServerConfig()
	if data, err := os.ReadFile(*configPath); err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: cannot parse config %s: %v\n", *configPath, err)
			os.Exit(1)
		}
	} else {
		fmt.Fprintf(os.Stderr, "WARNING: config file %s not found; using defaults\n", *configPath)
	}

	// ── Initialize logger ─────────────────────────────────────────────────
	log, err := logger.New(
		cfg.Logging.Dir,
		"moled",
		cfg.Logging.ConnectionRetainDays,
		cfg.Logging.ErrorRetainDays,
		cfg.Logging.Verbose,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot initialise logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Close()

	// ── Startup banner ────────────────────────────────────────────────────
	log.Info("══════════════════════════════════════════════════════")
	log.Info("  Mole Server starting — %s UTC", time.Now().UTC().Format("2006-01-02 15:04:05"))
	log.Info("══════════════════════════════════════════════════════")
	log.Info("  SOCKS5 port  : %d", cfg.Server.Socks5Port)
	log.Info("  DNS port     : %d", cfg.Server.DNSPort)
	log.Info("  Bind address : %s", cfg.Server.BindAddress)
	log.Info("  Upstream DNS : %s", cfg.UpstreamDNS)
	log.Info("  Auth enabled : %v", cfg.Auth.Enabled)
	log.Info("  Log dir      : %s", cfg.Logging.Dir)
	log.Info("  Conn retain  : %d days", cfg.Logging.ConnectionRetainDays)
	log.Info("  Err retain   : %d days", cfg.Logging.ErrorRetainDays)
	log.Info("══════════════════════════════════════════════════════")

	// ── Build component addresses ─────────────────────────────────────────
	socks5Addr := fmt.Sprintf("%s:%d", cfg.Server.BindAddress, cfg.Server.Socks5Port)
	dnsAddr := fmt.Sprintf("%s:%d", cfg.Server.BindAddress, cfg.Server.DNSPort)

	// ── Start SOCKS5 server ───────────────────────────────────────────────
	s5 := &socks5.Server{
		Addr:        socks5Addr,
		Log:         log,
		DialTimeout: 30 * time.Second,
	}
	if cfg.Auth.Enabled {
		if cfg.Auth.Username == "" || cfg.Auth.Password == "" {
			log.Error("Auth is enabled but username or password is empty — refusing to start")
			os.Exit(1)
		}
		s5.Username = cfg.Auth.Username
		s5.Password = cfg.Auth.Password
		log.Info("SOCKS5 authentication: ENABLED (user=%s)", cfg.Auth.Username)
	} else {
		log.Info("SOCKS5 authentication: DISABLED (no auth required)")
	}

	go func() {
		if err := s5.ListenAndServe(); err != nil {
			log.Error("SOCKS5 server stopped: %v", err)
			os.Exit(1)
		}
	}()

	// ── Start DNS proxy ───────────────────────────────────────────────────
	dnsProxy := &dnsproxy.Server{
		BindAddr:     dnsAddr,
		UpstreamDNS:  cfg.UpstreamDNS,
		Log:          log,
		QueryTimeout: 10 * time.Second,
	}
	go func() {
		if err := dnsProxy.ListenAndServe(); err != nil {
			log.Error("DNS proxy stopped: %v", err)
			os.Exit(1)
		}
	}()

	log.Info("Server ready. Waiting for connections…")

	// ── Wait for shutdown signal ──────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	log.Info("Received signal %v — shutting down gracefully…", sig)
	log.Info("Mole Server stopped at %s UTC", time.Now().UTC().Format("2006-01-02 15:04:05"))
}

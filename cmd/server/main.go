// moled: SOCKS5 + DNS proxy server with health checking.
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
// Health checking:
//   When enabled, the server periodically probes critical endpoints (internet,
//   GitHub, apt/brew). If probes fail, all client connections are killed and
//   new connections are rejected until health is restored. This forces clients
//   to fail over to a healthy server.
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
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mole/internal/dnsproxy"
	"github.com/mole/internal/health"
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
		Enabled   bool   `yaml:"enabled"`
		Username  string `yaml:"username"`
		Password  string `yaml:"password"`
		VerifyURL string `yaml:"verify_url"` // §10.3 per-activation token callback
	} `yaml:"auth"`

	UpstreamDNS string `yaml:"upstream_dns"`

	HealthCheck struct {
		// Enabled turns on periodic health checking. When a check fails,
		// all client connections are killed and new ones are rejected.
		Enabled bool `yaml:"enabled"`

		// IntervalMin is how often (in minutes) to run health checks.
		// Default: 90 (1.5 hours).
		IntervalMin int `yaml:"interval_min"`

		// TimeoutSec is the TCP connect timeout for each probe.
		// Default: 10.
		TimeoutSec int `yaml:"timeout_sec"`

		// Targets is an optional list of additional "host:port" endpoints
		// to probe. The default set (internet, GitHub, apt, Kali, brew)
		// is always included.
		Targets []string `yaml:"targets"`
	} `yaml:"health_check"`

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
	cfg.HealthCheck.Enabled = true
	cfg.HealthCheck.IntervalMin = 90
	cfg.HealthCheck.TimeoutSec = 10
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
	log.Info("  Health check : %v (interval: %dm)", cfg.HealthCheck.Enabled, healthInterval(cfg))
	log.Info("  Log dir      : %s", cfg.Logging.Dir)
	log.Info("  Conn retain  : %d days", cfg.Logging.ConnectionRetainDays)
	log.Info("  Err retain   : %d days", cfg.Logging.ErrorRetainDays)
	log.Info("══════════════════════════════════════════════════════")

	// ── Build component addresses ─────────────────────────────────────────
	socks5Addr := fmt.Sprintf("%s:%d", cfg.Server.BindAddress, cfg.Server.Socks5Port)
	dnsAddr := fmt.Sprintf("%s:%d", cfg.Server.BindAddress, cfg.Server.DNSPort)

	// ── Shared health state ───────────────────────────────────────────────
	var healthy atomic.Bool
	healthy.Store(true)

	// ── Start SOCKS5 server ───────────────────────────────────────────────
	s5 := &socks5.Server{
		Addr:        socks5Addr,
		Log:         log,
		DialTimeout: 30 * time.Second,
		Healthy:     &healthy,
	}
	if cfg.Auth.Enabled {
		if cfg.Auth.VerifyURL != "" {
			// §10.3 per-activation token callback (takes precedence over static).
			s5.VerifyURL = cfg.Auth.VerifyURL
			log.Info("SOCKS5 authentication: TOKEN-CALLBACK (%s)", cfg.Auth.VerifyURL)
		} else if cfg.Auth.Username == "" || cfg.Auth.Password == "" {
			log.Error("Auth is enabled but username/password (or verify_url) is empty — refusing to start")
			os.Exit(1)
		} else {
			s5.Username = cfg.Auth.Username
			s5.Password = cfg.Auth.Password
			log.Info("SOCKS5 authentication: ENABLED (user=%s)", cfg.Auth.Username)
		}
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

	// ── Start health checker ──────────────────────────────────────────────
	var hc *health.Checker
	if cfg.HealthCheck.Enabled {
		targets := health.DefaultTargets()
		for _, t := range cfg.HealthCheck.Targets {
			targets = append(targets, health.Target{
				Name:     t,
				Addr:     t,
				Critical: false,
			})
		}

		interval := time.Duration(healthInterval(cfg)) * time.Minute
		timeout := time.Duration(healthTimeout(cfg)) * time.Second

		hc = health.NewChecker(targets, interval, timeout, log)
		hc.OnUnhealthy = func() {
			s5.KillAllConns()
		}
		hc.OnHealthy = func() {
			log.Info("Health restored — SOCKS5 server accepting connections again")
		}

		go hc.Run()
		log.Info("Health checker started: %d target(s), every %v", len(targets), interval)
	}

	log.Info("Server ready. Waiting for connections…")

	// ── Wait for shutdown signal ──────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	log.Info("Received signal %v — shutting down gracefully…", sig)
	if hc != nil {
		hc.Stop()
	}
	log.Info("Mole Server stopped at %s UTC", time.Now().UTC().Format("2006-01-02 15:04:05"))
}

// ─── Helpers ─────────────────────────────────────────────────────────────

func healthInterval(cfg *ServerConfig) int {
	if cfg.HealthCheck.IntervalMin <= 0 {
		return 90
	}
	return cfg.HealthCheck.IntervalMin
}

func healthTimeout(cfg *ServerConfig) int {
	if cfg.HealthCheck.TimeoutSec <= 0 {
		return 10
	}
	return cfg.HealthCheck.TimeoutSec
}

// mole: transparent SOCKS5 + DNS proxy client.
//
// Usage:
//
//	mole [--config /path/to/config.yaml] [--check]
//
// Flags:
//
//	--config   Path to config YAML (default: ./config.yaml)
//	--check    Probe all servers, print status, and exit
//
// What it does:
//  1. Loads configuration from YAML.
//  2. Tries each server in order until one passes a live SOCKS5 probe.
//  3. Starts a local DNS proxy (127.0.0.1:53) that tunnels queries via SOCKS5.
//  4. Starts a local transparent TCP proxy (Linux: SO_ORIGINAL_DST via iptables).
//  5. Applies platform-specific routing (iptables/networksetup/netsh).
//  6. On SIGINT/SIGTERM, reverts all changes and exits cleanly.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	// golang.org/x/net/proxy — SOCKS5 dialer by The Go Authors / Google (https://pkg.go.dev/golang.org/x/net/proxy)
	"golang.org/x/net/proxy"

	"github.com/mole/internal/config"
	"github.com/mole/internal/dnsproxy"
	"github.com/mole/internal/logger"
	"github.com/mole/internal/platform"
	"github.com/mole/internal/transparent"
)

// version is the default version string, overridden at build time via:
//   -ldflags="-X main.version=<tag>"
// (see build.sh)
var version = "1.1.6"

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML configuration file")
	checkMode := flag.Bool("check", false, "Test connectivity to all servers and exit")
	flag.Parse()

	// ── Load & validate configuration ─────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		fmt.Fprintf(os.Stderr, "Run with --help for usage, or check config.yaml.example\n")
		os.Exit(1)
	}

	// ── Initialise logger ─────────────────────────────────────────────────
	log, err := logger.New(clientLogDir(), "mole", 90, 30, cfg.Logging.Verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot initialise logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Close()

	// ── Startup banner ────────────────────────────────────────────────────
	log.Info("══════════════════════════════════════════════════════════")
	log.Info("  Mole Client  v%s  —  %s UTC", version, time.Now().UTC().Format("2006-01-02 15:04:05"))
	log.Info("  Platform : %s/%s", runtime.GOOS, runtime.GOARCH)
	log.Info("  Config   : %s", *configPath)
	log.Info("  Servers  : %d configured", len(cfg.Servers))
	for i, s := range cfg.Servers {
		log.Info("    [%d] %s  →  %s", i+1, s.Name, s.Socks5Addr())
	}
	log.Info("══════════════════════════════════════════════════════════")

	// ── Check mode: probe all servers and exit ────────────────────────────
	if *checkMode {
		runCheckMode(cfg, log)
		return
	}

	// ── Find a working server (tries in order with retries) ───────────────
	activeServer, err := findWorkingServer(cfg, log)
	if err != nil {
		log.Error("FATAL: %v", err)
		log.Error("Ensure the VPN is active and at least one proxy server is running.")
		os.Exit(1)
	}
	log.Info("Active server selected: %s  (%s)", activeServer.Name, activeServer.Socks5Addr())

	// ── Start local DNS proxy ─────────────────────────────────────────────
	dnsListenAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Local.DNSPort)
	dnsProxy := &dnsproxy.ClientProxy{
		ListenAddr:   dnsListenAddr,
		Server:       activeServer,
		Auth:         cfg.Auth,
		UpstreamDNS:  cfg.DNS.UpstreamDNS,
		Log:          log,
		QueryTimeout: 10 * time.Second,
	}
	if err := dnsProxy.Start(); err != nil {
		log.Error("FATAL: DNS proxy failed to start on %s: %v", dnsListenAddr, err)
		log.Error("Hint: Port %d may be in use. Check: ss -tulpn | grep :%d",
			cfg.Local.DNSPort, cfg.Local.DNSPort)
		log.Error("Hint: On Ubuntu, try: sudo systemctl stop systemd-resolved")
		os.Exit(1)
	}
	defer dnsProxy.Stop()
	log.Info("DNS proxy started: UDP %s  →  SOCKS5  →  %s", dnsListenAddr, cfg.DNS.UpstreamDNS)

	// ── Start transparent TCP proxy (full on Linux, stub on others) ───────
	tpListenAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Local.Socks5Port)
	tProxy := &transparent.Proxy{
		ListenAddr:  tpListenAddr,
		Server:      activeServer,
		Auth:        cfg.Auth,
		Log:         log,
		DialTimeout: cfg.ConnectTimeout(),
	}
	if err := tProxy.Start(); err != nil {
		log.Error("FATAL: Transparent proxy failed to start on %s: %v", tpListenAddr, err)
		dnsProxy.Stop()
		os.Exit(1)
	}
	defer tProxy.Stop()

	// ── Apply platform routing & DNS override ─────────────────────────────
	ps := platform.Setup{
		ServerIPs:              collectServerIPs(cfg),
		ActiveServerHost:       activeServer.Host,
		ActiveServerSocks5Port: activeServer.Socks5Port,
		TransparentProxyPort:   cfg.Local.Socks5Port,
		DNSListenAddr:          dnsListenAddr,
	}
	mgr := platform.NewManager(ps, log)
	if err := mgr.Apply(); err != nil {
		log.Error("FATAL: Platform setup failed: %v", err)
		tProxy.Stop()
		dnsProxy.Stop()
		os.Exit(1)
	}
	// Platform revert is always called, even on panic.
	defer mgr.Revert()

	// ── Ready banner ──────────────────────────────────────────────────────
	log.Info("══════════════════════════════════════════════════════════")
	log.Info("  Mole Client ACTIVE")
	log.Info("  Server      : %s  (%s)", activeServer.Name, activeServer.Socks5Addr())
	log.Info("  DNS proxy   : %s  →  %s  (via SOCKS5)", dnsListenAddr, cfg.DNS.UpstreamDNS)
	log.Info("  TCP proxy   : %s  (transparent intercept)", tpListenAddr)
	log.Info("  Press Ctrl+C to disconnect and restore all system settings.")
	log.Info("══════════════════════════════════════════════════════════")

	// ── Block until SIGINT / SIGTERM ──────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	log.Info("")
	log.Info("Signal received: %v — initiating clean shutdown…", sig)
	// Deferred calls handle revert, tProxy.Stop(), dnsProxy.Stop()
	log.Info("All system settings restored. Mole Client stopped at %s UTC.",
		time.Now().UTC().Format("2006-01-02 15:04:05"))
}

// ─── Server selection ─────────────────────────────────────────────────────

// findWorkingServer iterates the configured server list in order, retrying
// each one up to RetryAttempts times.  Returns the first server that passes
// a live SOCKS5 connectivity probe.
func findWorkingServer(cfg *config.Config, log *logger.Logger) (config.ServerEntry, error) {
	log.Info("Probing %d configured server(s) — trying in order…", len(cfg.Servers))

	for idx, srv := range cfg.Servers {
		log.Info("[%d/%d] Probing: %s  (%s)…", idx+1, len(cfg.Servers), srv.Name, srv.Socks5Addr())

		var lastErr error
		for attempt := 1; attempt <= cfg.RetryAttempts(); attempt++ {
			if attempt > 1 {
				log.Info("  → Retry %d/%d in %v…", attempt, cfg.RetryAttempts(), cfg.RetryDelay())
				time.Sleep(cfg.RetryDelay())
			}

			if err := probeServer(srv, cfg); err != nil {
				lastErr = err
				log.Warn("  → Attempt %d/%d failed: %v", attempt, cfg.RetryAttempts(), err)
				continue
			}

			log.Info("  → SUCCESS — server is reachable ✓")
			return srv, nil
		}

		log.Warn("[%d/%d] FAILED: %s — all %d attempt(s) exhausted: %v",
			idx+1, len(cfg.Servers), srv.Name, cfg.RetryAttempts(), lastErr)
	}

	return config.ServerEntry{}, fmt.Errorf(
		"all %d server(s) unreachable — check VPN connection and server status",
		len(cfg.Servers),
	)
}

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

	// 1.1.1.1:80 is Cloudflare's DNS-over-HTTP — highly reliable, always up.
	conn, err := dialer.Dial("tcp", "1.1.1.1:80")
	if err != nil {
		return fmt.Errorf("SOCKS5 CONNECT to 1.1.1.1:80 via %s: %w", srv.Socks5Addr(), err)
	}
	_ = conn.Close()
	return nil
}

// ─── Check mode ───────────────────────────────────────────────────────────

func runCheckMode(cfg *config.Config, log *logger.Logger) {
	log.Info("Check mode: probing all %d configured server(s)…", len(cfg.Servers))
	log.Info("")

	allOK := true
	for i, srv := range cfg.Servers {
		log.Info("[%d/%d] %s", i+1, len(cfg.Servers), srv.Name)
		log.Info("       SOCKS5: %s", srv.Socks5Addr())
		log.Info("       DNS   : %s", srv.DNSAddr())

		start := time.Now()
		err := probeServer(srv, cfg)
		elapsed := time.Since(start).Round(time.Millisecond)

		if err != nil {
			log.Error("       Status: UNREACHABLE  (%v elapsed)", elapsed)
			log.Error("       Reason: %v", err)
			allOK = false
		} else {
			log.Info("       Status: REACHABLE ✓  (%v elapsed)", elapsed)
		}
		log.Info("")
	}

	if allOK {
		log.Info("All servers reachable.")
		os.Exit(0)
	} else {
		log.Error("One or more servers unreachable.")
		os.Exit(1)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────

func collectServerIPs(cfg *config.Config) []string {
	seen := make(map[string]struct{})
	var ips []string
	for _, srv := range cfg.Servers {
		if _, ok := seen[srv.Host]; !ok {
			seen[srv.Host] = struct{}{}
			ips = append(ips, srv.Host)
		}
	}
	return ips
}

func clientLogDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.mole/logs"
	}
	return "/tmp/mole/logs"
}

// mole: transparent SOCKS5 + DNS proxy client with automatic failover.
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
//  2. Probes all enabled servers concurrently, ranking by priority then speed.
//  3. Selects the best server and starts local DNS + transparent TCP proxies.
//  4. Applies platform-specific routing (iptables/networksetup/netsh).
//  5. Monitors the active server; fails over automatically on connection loss.
//  6. Periodically re-evaluates servers and switches to a higher-priority one
//     when safe (no active tunnel connections).
//  7. On SIGINT/SIGTERM, reverts all changes and exits cleanly.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/mole/internal/config"
	"github.com/mole/internal/connector"
	"github.com/mole/internal/logger"
	"github.com/mole/internal/platform"
	"github.com/mole/internal/transparent"
	"github.com/mole/internal/dnsproxy"
)

// version is the default version string, overridden at build time via:
//   -ldflags="-X main.version=<tag>"
// (see build.sh)
var version = "1.2.0"

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
	log.Info("  Servers  : %d configured (%d enabled)", len(cfg.Servers), len(cfg.EnabledServers()))
	for i, s := range cfg.Servers {
		pri := "default"
		if s.Priority != nil {
			if *s.Priority == 0 {
				pri = "DISABLED"
			} else {
				pri = fmt.Sprintf("P%d", *s.Priority)
			}
		}
		log.Info("    [%d] %s  →  %s  (%s)", i+1, s.Name, s.Socks5Addr(), pri)
	}
	log.Info("══════════════════════════════════════════════════════════")

	// ── Create connector ──────────────────────────────────────────────────
	conn := connector.New(cfg, log)

	// ── Check mode: probe all servers and exit ────────────────────────────
	if *checkMode {
		runCheckMode(conn, cfg, log)
		return
	}

	// ── Select best server (probes all concurrently) ─────────────────────
	activeServer, err := conn.InitialSelect()
	if err != nil {
		log.Error("FATAL: %v", err)
		log.Error("Ensure the VPN is active and at least one proxy server is running.")
		os.Exit(1)
	}
	log.Info("Active server selected: %s  (%s)  [P%d]",
		activeServer.Name, activeServer.Socks5Addr(), activeServer.EffectivePriority())

	// ── Start local DNS proxy ─────────────────────────────────────────────
	dnsListenAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Local.DNSPort)
	dnsProxy := &dnsproxy.ClientProxy{
		ListenAddr:   dnsListenAddr,
		Server:       activeServer,
		Auth:         cfg.Auth,
		UpstreamDNS:  cfg.DNS.UpstreamDNS,
		Log:          log,
		QueryTimeout: 10 * time.Second,
		OnDialFail:   func() { conn.ReportDialFailure() },
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
		OnConnOpen:  func() { conn.IncrConns() },
		OnConnClose: func() { conn.DecrConns() },
		OnDialFail:  func() { conn.ReportDialFailure() },
	}
	if err := tProxy.Start(); err != nil {
		log.Error("FATAL: Transparent proxy failed to start on %s: %v", tpListenAddr, err)
		dnsProxy.Stop()
		os.Exit(1)
	}
	defer tProxy.Stop()

	// ── Register server-switch callbacks ──────────────────────────────────
	conn.OnServerSwitch(func(srv config.ServerEntry) {
		tProxy.UpdateServer(srv)
		dnsProxy.UpdateServer(srv)
	})

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

	// ── Start background loops ────────────────────────────────────────────
	go conn.RunFailoverLoop()
	go conn.RunHealthCheckLoop()

	// ── Ready banner ──────────────────────────────────────────────────────
	log.Info("══════════════════════════════════════════════════════════")
	log.Info("  Mole Client ACTIVE")
	log.Info("  Server      : %s  (%s)  [P%d]", activeServer.Name, activeServer.Socks5Addr(), activeServer.EffectivePriority())
	log.Info("  DNS proxy   : %s  →  %s  (via SOCKS5)", dnsListenAddr, cfg.DNS.UpstreamDNS)
	log.Info("  TCP proxy   : %s  (transparent intercept)", tpListenAddr)
	log.Info("  Failover    : automatic (3 failures in 30s triggers switch)")
	log.Info("  Health check: every %v", cfg.HealthCheckInterval())
	log.Info("  Press Ctrl+C to disconnect and restore all system settings.")
	log.Info("══════════════════════════════════════════════════════════")

	// ── Block until SIGINT / SIGTERM ──────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	log.Info("")
	log.Info("Signal received: %v — initiating clean shutdown…", sig)
	conn.Stop()
	// Deferred calls handle revert, tProxy.Stop(), dnsProxy.Stop()
	log.Info("All system settings restored. Mole Client stopped at %s UTC.",
		time.Now().UTC().Format("2006-01-02 15:04:05"))
}

// ─── Check mode ───────────────────────────────────────────────────────────

func runCheckMode(conn *connector.Connector, cfg *config.Config, log *logger.Logger) {
	log.Info("Check mode: probing all %d configured server(s) concurrently…", len(cfg.Servers))
	log.Info("")

	// Show disabled servers.
	for _, srv := range cfg.Servers {
		if !srv.IsEnabled() {
			log.Info("[--] %s", srv.Name)
			log.Info("       SOCKS5 : %s", srv.Socks5Addr())
			log.Info("       DNS    : %s", srv.DNSAddr())
			log.Info("       Status : DISABLED (priority=0)")
			log.Info("")
		}
	}

	statuses := conn.ProbeAll()

	allOK := true
	for i, s := range statuses {
		log.Info("[%d/%d] %s", i+1, len(statuses), s.Server.Name)
		log.Info("       SOCKS5  : %s", s.Server.Socks5Addr())
		log.Info("       DNS     : %s", s.Server.DNSAddr())
		log.Info("       Priority: %d", s.Server.EffectivePriority())

		if s.Alive {
			log.Info("       Status  : REACHABLE ✓  (%v)", s.Latency.Round(time.Millisecond))
		} else {
			log.Error("       Status  : UNREACHABLE")
			allOK = false
		}
		log.Info("")
	}

	// Show what would be selected.
	best, err := connector.SelectBest(statuses)
	if err == nil {
		log.Info("Best server: %s  (P%d, latency %v)",
			best.Server.Name, best.Server.EffectivePriority(),
			best.Latency.Round(time.Millisecond))
	}

	if allOK {
		log.Info("All enabled servers reachable.")
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

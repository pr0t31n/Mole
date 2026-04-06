# MOLE — SOCKS5 + DNS Transparent Proxy Suite

Built for one specific scenario:

> A device on a network that **blocks all outbound traffic except VPN** needs full
> internet access, with DNS bypassing the network-controlled resolvers.

---

## Architecture

```
┌──────────────── RESTRICTED NETWORK ─────────────────────┐
│                                                          │
│  ┌────────────────────────┐                              │
│  │  CLIENT MACHINE        │      ┌──────────────────┐   │
│  │                        │ VPN  │  PROXY SERVER    │   │
│  │  App → DNS query       │══════│                  │   │
│  │   ↓ iptables           │      │  SOCKS5 :1080    │   │
│  │  DNS Proxy :53         │      │   ↓              │   │
│  │   ↓ TCP via SOCKS5     │      │  → internet      │   │
│  │                        │      │                  │   │
│  │  App → TCP :443        │      │  DNS Proxy :5353 │   │
│  │   ↓ iptables redirect  │      │   ↓ 8.8.8.8      │   │
│  │  Transparent :1081     │      └──────────────────┘   │
│  │   ↓ SOCKS5 tunnel      │                              │
│  └────────────────────────┘                              │
│  Network DNS: COMPLETELY BYPASSED                        │
└──────────────────────────────────────────────────────────┘
```

**Linux flow (full transparent):**
1. DNS query → iptables captures → local DNS proxy → SOCKS5 → server → 8.8.8.8
2. TCP connection → iptables REDIRECT → transparent proxy (SO_ORIGINAL_DST) → SOCKS5 → server → destination
3. `/etc/resolv.conf` → `nameserver 127.0.0.1` (network DNS bypassed entirely)

**macOS / Windows:** System SOCKS5 proxy + DNS override (covers most applications).

---

## Quick Start

### Build
```bash
bash scripts/build.sh all
# Outputs to ./dist/
```

### Server (Kali/Ubuntu)
```bash
# Copy binary, then:
sudo bash scripts/server-install.sh

# View live logs
sudo journalctl -u moled -f
tail -f /var/log/mole/server_connections_$(date +%Y-%m-%d).log
```

### Client
```bash
cp configs/config.yaml.example config.yaml
# Edit: set server host(s)

sudo ./mole --config config.yaml --check  # probe servers
sudo ./mole --config config.yaml          # activate
# Ctrl+C cleanly restores all system settings
```

---

## Configuration Reference

### Client `config.yaml`

```yaml
servers:
  - name: "Primary"
    host: "10.8.0.1"        # VPN IP of proxy server
    socks5_port: 1080
    dns_port: 5353

auth:
  username: ""               # leave empty if server auth disabled
  password: ""

local:
  dns_port: 53               # local DNS proxy (requires root)
  socks5_port: 1081          # local transparent proxy

network:
  bypass_networks:           # these CIDRs go direct, not proxied
    - "10.0.0.0/8"
    - "172.16.0.0/12"
    - "192.168.0.0/16"
  connect_timeout_sec: 10
  retry_attempts: 3
  retry_delay_sec: 2

dns:
  upstream_dns: "8.8.8.8:53"  # queried through SOCKS5 tunnel

logging:
  verbose: true
```

### Server `/etc/mole/server-config.yaml`

```yaml
server:
  bind_address: "0.0.0.0"
  socks5_port: 1080
  dns_port: 5353

auth:
  enabled: false
  username: ""
  password: ""

upstream_dns: "1.1.1.1:53"

logging:
  dir: "/var/log/mole"
  verbose: true
  connection_retain_days: 90   # SOCKS5 connect/disconnect events
  error_retain_days: 30
```

---

## Log Files

| File | Retention | Content |
|------|-----------|---------|
| `server_connections_YYYY-MM-DD.log` | 90 days | CONNECT / DISCONNECT events with bytes transferred |
| `server_errors_YYYY-MM-DD.log` | 30 days | All errors |
| `client_connections_YYYY-MM-DD.log` | 90 days | Connection events |
| `client_errors_YYYY-MM-DD.log` | 30 days | Client errors |

All timestamps are **UTC**. Logs rotate at midnight UTC.

---

## Troubleshooting

**All servers unreachable:**
```bash
sudo ./mole --config config.yaml --check
ping 10.8.0.1                 # VPN reachable?
nc -zv 10.8.0.1 1080          # port open?
```

**DNS not working:**
```bash
cat /etc/resolv.conf           # should say: nameserver 127.0.0.1
dig @127.0.0.1 google.com      # test local DNS proxy
ss -tulpn | grep :53           # proxy listening?
```

**Port 53 in use (Ubuntu):**
```bash
# The client auto-stops systemd-resolved.  If it fails:
sudo systemctl stop systemd-resolved
```

**Manual cleanup after crash (Linux):**
```bash
sudo iptables -t nat -D OUTPUT -j MOLE_OUTPUT 2>/dev/null
sudo iptables -t nat -F MOLE_OUTPUT 2>/dev/null
sudo iptables -t nat -X MOLE_OUTPUT 2>/dev/null
sudo cp /etc/resolv.conf.mole.bak /etc/resolv.conf
sudo systemctl start systemd-resolved
```

**Verify traffic is tunnelled:**
```bash
curl https://ifconfig.me   # should return server's IP, not yours
```

---

## Design

**Language:** Go — single static binary, zero runtime dependencies, cross-compiles to all targets.

**External packages (4):**
| Package | Role |
|---------|------|
| `github.com/miekg/dns` | DNS message parsing |
| `golang.org/x/net/proxy` | SOCKS5 client dialer |
| `golang.org/x/sys` | SO_ORIGINAL_DST syscall (Linux transparent proxy) |
| `gopkg.in/yaml.v3` | Config parsing |

**Binary sizes:** ~8–12 MB (includes all dependencies statically linked).
**Runtime memory:** ~5–15 MB RSS under typical load.

---

## Contributors

Mole is built on top of the following open-source libraries and tools. Full credit to their authors.

### Go Libraries

| Library | Author / Organisation | Link |
|---------|----------------------|------|
| `github.com/miekg/dns` | Miek Gieben (@miekg) | https://github.com/miekg/dns |
| `golang.org/x/net/proxy` | The Go Authors / Google | https://pkg.go.dev/golang.org/x/net/proxy |
| `golang.org/x/sys` | The Go Authors / Google | https://pkg.go.dev/golang.org/x/sys |
| `gopkg.in/yaml.v3` | Canonical Ltd / go-yaml team | https://github.com/go-yaml/yaml |

### Go Standard Library

| Package | Author |
|---------|--------|
| `net`, `net/http`, `flag`, `os`, `runtime`, `encoding/binary`, `io` | The Go Authors / Google (https://go.dev) |

### System Tools

| Tool | Author / Organisation | Link |
|------|-----------------------|------|
| iptables (Linux NAT/REDIRECT) | Netfilter Project / Harald Welte (@laforge) | https://www.netfilter.org |
| networksetup (macOS proxy config) | Apple Inc. | https://support.apple.com/guide/mac-help |
| netsh (Windows proxy config) | Microsoft Corporation | https://learn.microsoft.com/en-us/windows-server/networking/technologies/netsh |

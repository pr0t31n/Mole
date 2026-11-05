<details>
<summary><strong>FULL DISCLOSURE</strong> — AI Development & Legal Notice</summary>

---

### AI Development Disclosure

This tool is part of the **pr0t31n toolkit**, a collection of security testing and penetration testing automation tools. The toolkit was created through AI-assisted development, guided by the professional experience and domain expertise of a cybersecurity consultant and penetration tester. Ya, I named it after myself - but to be fair, AI is horrible at naming things.

**The pr0t31n toolkit exists to serve two purposes:**

1. **Automate repetitive penetration testing workflow steps.** Security assessments involve significant amounts of repetitive, procedural work — running sequential tool chains, parsing outputs, correlating findings across tools, and managing engagement data. Automating these steps frees testers to focus on the work that requires human judgment: manual testing, creative attack paths, analysis, and reporting.

2. **Demonstrate the benefits and failures of AI-assisted development.** This toolkit is a real-world case study in building production security tooling with AI assistance. It demonstrates both what AI-assisted development can accomplish and where it falls short. The quality of AI-generated code varies significantly — between context refreshes, the output can range from senior/principal-level software engineering to the work of a still-in-school intern. This inconsistency is itself part of the demonstration.

In the interest of transparency, every tool in the pr0t31n toolkit carries this disclosure. The creator believes that AI-assisted development should be acknowledged openly, not concealed. Users, reviewers, and anyone examining this code should know how it was built so they can evaluate it accordingly.

---

### Legal & Authorization Notice

> **⚠ WARNING: AUTHORIZED USE ONLY**

This tool is designed and intended **exclusively** for use in **authorized security testing engagements**. "Authorized" means **all** of the following conditions are met:

- **Explicit Written Permission.** You possess explicit, documented, written authorization from the owner or authorized representative of the target system(s), network(s), or application(s) to conduct security testing. Verbal agreements are not sufficient.
- **Defined Scope.** The authorization clearly defines the scope of testing, including which systems, networks, IP ranges, applications, and/or endpoints are included. You must not exceed the boundaries of this defined scope under any circumstances.
- **Defined Timeframe.** The authorization specifies the dates and times during which testing is permitted. Testing outside the authorized window is unauthorized testing.
- **Rules of Engagement.** You are operating under a set of rules of engagement (RoE) or equivalent governing document that establishes acceptable testing methods, escalation procedures, emergency contacts, and any restrictions or exclusions.
- **Legal Compliance.** Your testing complies with all applicable local, state, national, and international laws and regulations, including but not limited to the Computer Fraud and Abuse Act (CFAA), the General Data Protection Regulation (GDPR), and equivalent legislation in your jurisdiction.

**Prohibited uses** include but are not limited to: unauthorized access to or testing of any system; testing against systems without explicit written authorization; any activity that violates applicable laws; use for malicious purposes including data theft, extortion, sabotage, or disruption of services; distribution intended to facilitate unauthorized access; and use without a clear legal basis.

**THIS TOOL IS PROVIDED "AS IS," WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, AND NONINFRINGEMENT.**

The creator(s) and contributor(s) accept no responsibility or liability for any damage, loss, legal consequence, or other harm resulting from the use or misuse of this tool — including damage to systems or data, legal penalties or prosecution, loss of data or revenue, and any direct, indirect, incidental, special, consequential, or punitive damages.

**By using this tool, you acknowledge that you are solely responsible for ensuring that your use is authorized, lawful, and within the defined scope of your engagement. You assume all risk associated with the use of this tool.**

Security testing is a professional discipline with significant ethical and legal obligations. Users operating in a professional capacity are expected to adhere to the standards and codes of conduct established by recognized industry bodies, including PTES, OWASP, NIST, and any certifying body under which they hold credentials.

---

</details>

# MOLE — SOCKS5 + DNS Transparent Proxy Suite

Built for one specific scenario:

> A device on a network that **blocks all outbound traffic except VPN** needs full
> internet access, with DNS bypassing the network-controlled resolvers.

---


> **[View full documentation on the Codex Wiki](https://wiki.pr0t31n.com/tools/mole)** — detailed features, CLI reference, findings catalogue, and integration guides.

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

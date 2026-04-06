#!/usr/bin/env bash
# =============================================================================
# Mole Server Installer
# Installs moled as a systemd service on Kali Linux / Ubuntu.
#
# Usage:
#   sudo bash server-install.sh [--uninstall]
# =============================================================================
set -euo pipefail

SERVICE_NAME="moled"
SERVICE_USER="mole"
BINARY_SRC="./moled"
BINARY_DST="/usr/local/bin/moled"
CONFIG_DIR="/etc/mole"
CONFIG_FILE="${CONFIG_DIR}/moled.yaml"
LOG_DIR="/var/log/mole"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fatal() { echo -e "${RED}[FATAL]${NC} $*" >&2; exit 1; }

[[ $EUID -ne 0 ]] && fatal "Must run as root: sudo bash $0"

# ─── Uninstall ────────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--uninstall" ]]; then
    info "Uninstalling ${SERVICE_NAME}…"
    systemctl stop    "${SERVICE_NAME}" 2>/dev/null && ok "Stopped"   || true
    systemctl disable "${SERVICE_NAME}" 2>/dev/null && ok "Disabled"  || true
    rm -f "${UNIT_FILE}"; systemctl daemon-reload
    rm -f "${BINARY_DST}"; ok "Binary removed"
    id "${SERVICE_USER}" &>/dev/null && userdel -r "${SERVICE_USER}" 2>/dev/null && ok "User removed" || true
    warn "Config (${CONFIG_DIR}) and logs (${LOG_DIR}) were NOT removed."
    warn "Remove manually if desired: sudo rm -rf ${CONFIG_DIR} ${LOG_DIR}"
    ok "Uninstall complete."; exit 0
fi

# ─── Install ──────────────────────────────────────────────────────────────────
echo -e "\n${CYAN}╔══════════════════════════════════════════╗"
echo -e "║   Mole Server Installer            ║"
echo -e "╚══════════════════════════════════════════╝${NC}\n"

command -v systemctl &>/dev/null || fatal "systemd required but not found."
[[ -f "${BINARY_SRC}" ]]        || fatal "Binary not found: ${BINARY_SRC}\nRun: bash scripts/build.sh server"

# 1. Service user
info "Creating service user '${SERVICE_USER}'…"
if id "${SERVICE_USER}" &>/dev/null; then
    warn "User already exists — skipping."
else
    useradd --system --no-create-home --shell /usr/sbin/nologin \
            --comment "Mole daemon" "${SERVICE_USER}"
    ok "User '${SERVICE_USER}' created."
fi

# 2. Binary
info "Installing binary → ${BINARY_DST}"
install -m 0755 -o root -g root "${BINARY_SRC}" "${BINARY_DST}"
ok "Binary installed."

# 3. Directories
info "Creating directories…"
mkdir -p "${CONFIG_DIR}" "${LOG_DIR}"
chown "${SERVICE_USER}:${SERVICE_USER}" "${LOG_DIR}"
chmod 750 "${LOG_DIR}"
ok "Directories ready."

# 4. Default server config
if [[ ! -f "${CONFIG_FILE}" ]]; then
    info "Writing default config → ${CONFIG_FILE}"
    cat > "${CONFIG_FILE}" << 'YAML'
# ============================================================
# Mole Server Configuration
# Restart after changes: sudo systemctl restart moled
# ============================================================

server:
  # Listen on all interfaces. Restrict if needed (e.g. "10.8.0.1").
  bind_address: "0.0.0.0"

  # SOCKS5 proxy port — clients must connect to this.
  socks5_port: 1080

  # DNS proxy port — receives DNS-over-TCP from tunnelled clients.
  dns_port: 5353

# SOCKS5 authentication.
# enabled: false means any client may connect (suitable for VPN-protected servers).
# If true, clients must supply these credentials in their config.yaml.
auth:
  enabled: false
  username: ""
  password: ""

# Upstream DNS resolver used by the server to resolve external domains.
# Options: 1.1.1.1:53 (Cloudflare), 8.8.8.8:53 (Google), 9.9.9.9:53 (Quad9)
upstream_dns: "1.1.1.1:53"

logging:
  dir: "/var/log/mole"
  verbose: true
  # Connection logs (CONNECT/DISCONNECT events) — kept for 90 days.
  connection_retain_days: 90
  # Error logs — kept for 30 days.
  error_retain_days: 30
YAML
    chmod 640 "${CONFIG_FILE}"
    chown "root:${SERVICE_USER}" "${CONFIG_FILE}"
    ok "Default config written."
else
    warn "Config already exists — not overwriting: ${CONFIG_FILE}"
fi

# 5. Systemd unit
info "Installing systemd unit → ${UNIT_FILE}"
cat > "${UNIT_FILE}" << UNIT
[Unit]
Description=Mole SOCKS5 + DNS Proxy Server
Documentation=https://github.com/mole
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}

# CAP_NET_BIND_SERVICE is needed only if dns_port < 1024.
# If using port 5353 or higher, this can be removed.
AmbientCapabilities=CAP_NET_BIND_SERVICE

ExecStart=${BINARY_DST} --config ${CONFIG_FILE}
ExecReload=/bin/kill -HUP \$MAINPID

# Auto-restart on crash (not on clean exit).
Restart=on-failure
RestartSec=5s

# Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=${LOG_DIR}
PrivateTmp=yes
PrivateDevices=yes

StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
UNIT

chmod 644 "${UNIT_FILE}"
ok "Unit file installed."

# 6. Firewall (ufw)
SOCKS5_PORT=$(grep "socks5_port:" "${CONFIG_FILE}" | awk '{print $2}')
DNS_PORT=$(grep "dns_port:"    "${CONFIG_FILE}" | awk '{print $2}')

if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
    info "Configuring ufw firewall…"
    ufw allow "${SOCKS5_PORT}/tcp"  comment "Mole SOCKS5"   && ok "ufw: TCP ${SOCKS5_PORT} allowed"
    ufw allow "${DNS_PORT}/tcp"     comment "Mole DNS TCP"  && ok "ufw: TCP ${DNS_PORT} allowed"
    ufw allow "${DNS_PORT}/udp"     comment "Mole DNS UDP"  && ok "ufw: UDP ${DNS_PORT} allowed"
else
    warn "ufw not active. Manually allow: TCP ${SOCKS5_PORT}, TCP/UDP ${DNS_PORT}"
fi

# 7. Enable and start
info "Enabling and starting ${SERVICE_NAME}…"
systemctl daemon-reload
systemctl enable  "${SERVICE_NAME}"
systemctl restart "${SERVICE_NAME}"
sleep 2

if systemctl is-active --quiet "${SERVICE_NAME}"; then
    ok "Service is running!"
else
    warn "Service may not have started correctly."
    systemctl status "${SERVICE_NAME}" --no-pager -l || true
    warn "Check logs: journalctl -u ${SERVICE_NAME} -n 50 --no-pager"
    exit 1
fi

# ─── Summary ─────────────────────────────────────────────────────────────────
echo -e "\n${GREEN}╔══════════════════════════════════════════════════╗"
echo -e "║  Installation complete!                          ║"
echo -e "╚══════════════════════════════════════════════════╝${NC}\n"
echo -e "  Binary      : ${BINARY_DST}"
echo -e "  Config      : ${CONFIG_FILE}"
echo -e "  Logs        : ${LOG_DIR}/"
echo -e "  SOCKS5 port : ${SOCKS5_PORT}"
echo -e "  DNS port    : ${DNS_PORT}\n"
echo -e "  ${CYAN}Service commands:${NC}"
echo -e "    Status  : ${YELLOW}sudo systemctl status ${SERVICE_NAME}${NC}"
echo -e "    Logs    : ${YELLOW}sudo journalctl -u ${SERVICE_NAME} -f${NC}"
echo -e "    Restart : ${YELLOW}sudo systemctl restart ${SERVICE_NAME}${NC}"
echo -e "    Stop    : ${YELLOW}sudo systemctl stop ${SERVICE_NAME}${NC}\n"
echo -e "  ${CYAN}Log tailing:${NC}"
echo -e "    ${YELLOW}tail -f ${LOG_DIR}/server_connections_\$(date +%Y-%m-%d).log${NC}"
echo -e "    ${YELLOW}tail -f ${LOG_DIR}/server_errors_\$(date +%Y-%m-%d).log${NC}\n"
echo -e "  ${YELLOW}⚠  Edit ${CONFIG_FILE} before deploying clients!${NC}\n"

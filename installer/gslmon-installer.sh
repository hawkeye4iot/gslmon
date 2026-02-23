#!/usr/bin/env bash
# gslmon Installer Script
# Copyright (C) 2026 GetSetLive Pvt Ltd
# Source Code Free to Distribute under GPL License
#
# Created: 2026-02-23
# Purpose: Auto-detect RAID configuration, install dependencies, compile gslmon,
#          and deploy as a systemd service with auto-populated config.json.
#
# Usage: sudo bash install.sh
#        sudo bash install.sh --uninstall

set -euo pipefail

# ==================== Configuration ====================
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/gslmon"
LOG_DIR="/var/log/gslmon"
STATE_DIR="/var/lib/gslmon"
TMP_DIR="/var/lib/gslmon/tmp"
PID_DIR="/var/run/gslmon"
SERVICE_NAME="gslmon"
GO_MIN_VERSION="1.21"
GO_INSTALL_VERSION="1.22.5"

# ==================== Color Output ====================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
log_ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_err()   { echo -e "${RED}[ERROR]${NC} $*"; }
log_step()  { echo -e "${CYAN}[STEP]${NC}  $*"; }

# ==================== Root Check ====================
check_root() {
    if [[ $EUID -ne 0 ]]; then
        log_err "This installer must be run as root (sudo bash install.sh)"
        exit 1
    fi
}

# ==================== Uninstall ====================
do_uninstall() {
    log_step "Uninstalling gslmon..."
    systemctl stop gslmon 2>/dev/null || true
    systemctl disable gslmon 2>/dev/null || true
    rm -f /etc/systemd/system/gslmon.service
    systemctl daemon-reload 2>/dev/null || true
    rm -f "${INSTALL_DIR}/gslmon"
    rm -rf "${LOG_DIR}" "${STATE_DIR}" "${PID_DIR}"
    log_warn "Configuration preserved at ${CONFIG_DIR}/config.json (remove manually if needed)"
    log_ok "gslmon uninstalled successfully"
    exit 0
}

# ==================== OS Detection ====================
detect_os() {
    if [[ -f /etc/os-release ]]; then
        . /etc/os-release
        OS_ID="${ID}"
        OS_VERSION="${VERSION_ID:-unknown}"
        OS_NAME="${PRETTY_NAME:-${ID}}"
    elif [[ -f /etc/redhat-release ]]; then
        OS_ID="rhel"
        OS_VERSION=$(grep -oE '[0-9]+\.[0-9]+' /etc/redhat-release | head -1)
        OS_NAME=$(cat /etc/redhat-release)
    else
        OS_ID="unknown"
        OS_VERSION="unknown"
        OS_NAME="Unknown Linux"
    fi
    log_info "Detected OS: ${OS_NAME}"
}

# ==================== Package Manager Detection ====================
detect_package_manager() {
    if command -v dnf &>/dev/null; then
        PKG_MGR="dnf"
        PKG_INSTALL="dnf install -y"
    elif command -v yum &>/dev/null; then
        PKG_MGR="yum"
        PKG_INSTALL="yum install -y"
    elif command -v apt-get &>/dev/null; then
        PKG_MGR="apt"
        PKG_INSTALL="apt-get install -y"
        apt-get update -qq 2>/dev/null || true
    elif command -v zypper &>/dev/null; then
        PKG_MGR="zypper"
        PKG_INSTALL="zypper install -y"
    elif command -v pacman &>/dev/null; then
        PKG_MGR="pacman"
        PKG_INSTALL="pacman -S --noconfirm"
    else
        PKG_MGR="unknown"
        PKG_INSTALL=""
        log_warn "Could not detect package manager. Dependencies must be installed manually."
    fi
    log_info "Package manager: ${PKG_MGR}"
}

# ==================== Dependency Installation ====================
install_smartmontools() {
    if command -v smartctl &>/dev/null; then
        log_ok "smartmontools already installed ($(smartctl --version | head -1))"
        return 0
    fi
    log_step "Installing smartmontools..."
    case "${PKG_MGR}" in
        dnf|yum)  ${PKG_INSTALL} smartmontools ;;
        apt)      ${PKG_INSTALL} smartmontools ;;
        zypper)   ${PKG_INSTALL} smartmontools ;;
        pacman)   ${PKG_INSTALL} smartmontools ;;
        *)        log_err "Please install smartmontools manually"; return 1 ;;
    esac
    log_ok "smartmontools installed"
}

install_mdadm() {
    if command -v mdadm &>/dev/null; then
        log_ok "mdadm already installed ($(mdadm --version 2>&1 | head -1))"
        return 0
    fi
    log_step "Installing mdadm..."
    case "${PKG_MGR}" in
        dnf|yum)  ${PKG_INSTALL} mdadm ;;
        apt)      ${PKG_INSTALL} mdadm ;;
        zypper)   ${PKG_INSTALL} mdadm ;;
        pacman)   ${PKG_INSTALL} mdadm ;;
        *)        log_err "Please install mdadm manually"; return 1 ;;
    esac
    log_ok "mdadm installed"
}

# ==================== Go Installation ====================
version_ge() {
    # Returns 0 if $1 >= $2 (semver comparison)
    printf '%s\n%s' "$2" "$1" | sort -V -C
}

check_and_install_go() {
    local go_bin=""

    # Check if Go is already installed
    if command -v go &>/dev/null; then
        go_bin="go"
    elif [[ -x /usr/local/go/bin/go ]]; then
        go_bin="/usr/local/go/bin/go"
        export PATH="/usr/local/go/bin:${PATH}"
    fi

    if [[ -n "${go_bin}" ]]; then
        local current_version
        current_version=$("${go_bin}" version | grep -oE 'go[0-9]+\.[0-9]+(\.[0-9]+)?' | sed 's/go//')
        if version_ge "${current_version}" "${GO_MIN_VERSION}"; then
            log_ok "Go ${current_version} already installed (>= ${GO_MIN_VERSION} required)"
            return 0
        else
            log_warn "Go ${current_version} found but ${GO_MIN_VERSION}+ required. Upgrading..."
        fi
    else
        log_info "Go not found. Installing Go ${GO_INSTALL_VERSION}..."
    fi

    # Try package manager first
    case "${PKG_MGR}" in
        dnf|yum)
            ${PKG_INSTALL} golang 2>/dev/null && {
                local pkg_ver
                pkg_ver=$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | sed 's/go//' || echo "0")
                if version_ge "${pkg_ver}" "${GO_MIN_VERSION}"; then
                    log_ok "Go ${pkg_ver} installed from package manager"
                    return 0
                fi
                log_warn "Package manager Go version (${pkg_ver}) is too old. Installing from tarball..."
            } || true
            ;;
        apt)
            ${PKG_INSTALL} golang-go 2>/dev/null && {
                local pkg_ver
                pkg_ver=$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | sed 's/go//' || echo "0")
                if version_ge "${pkg_ver}" "${GO_MIN_VERSION}"; then
                    log_ok "Go ${pkg_ver} installed from package manager"
                    return 0
                fi
                log_warn "Package manager Go version (${pkg_ver}) is too old. Installing from tarball..."
            } || true
            ;;
        zypper)
            ${PKG_INSTALL} go 2>/dev/null && {
                local pkg_ver
                pkg_ver=$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | sed 's/go//' || echo "0")
                if version_ge "${pkg_ver}" "${GO_MIN_VERSION}"; then
                    log_ok "Go ${pkg_ver} installed from package manager"
                    return 0
                fi
            } || true
            ;;
        pacman)
            ${PKG_INSTALL} go 2>/dev/null && {
                log_ok "Go installed from pacman"
                return 0
            } || true
            ;;
    esac

    # Fallback: install from official Go tarball
    install_go_from_tarball
}

install_go_from_tarball() {
    local arch
    arch=$(uname -m)
    case "${arch}" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        armv7l)  arch="armv6l" ;;
        *)       log_err "Unsupported architecture: ${arch}"; exit 1 ;;
    esac

    local tarball="go${GO_INSTALL_VERSION}.linux-${arch}.tar.gz"
    local url="https://go.dev/dl/${tarball}"
    local tmpfile="/tmp/gslmon_go_install_${tarball}"

    log_step "Downloading Go ${GO_INSTALL_VERSION} for ${arch}..."

    if command -v wget &>/dev/null; then
        wget -q --show-progress -O "${tmpfile}" "${url}"
    elif command -v curl &>/dev/null; then
        curl -fSL -o "${tmpfile}" "${url}"
    else
        log_err "Neither wget nor curl found. Cannot download Go."
        exit 1
    fi

    log_step "Installing Go to /usr/local/go..."
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "${tmpfile}"
    rm -f "${tmpfile}"

    export PATH="/usr/local/go/bin:${PATH}"

    # Add to system profile for future sessions
    if [[ ! -f /etc/profile.d/golang.sh ]]; then
        echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/golang.sh
        chmod 644 /etc/profile.d/golang.sh
    fi

    local installed_ver
    installed_ver=$(go version | grep -oE 'go[0-9]+\.[0-9]+\.[0-9]+' | sed 's/go//')
    log_ok "Go ${installed_ver} installed successfully"
}

# ==================== RAID Auto-Detection ====================
detect_raid_config() {
    log_step "Auto-detecting RAID configuration..."

    RAID_ARRAY_DEVICE=""
    RAID_ARRAY_NAME=""
    RAID_LEVEL=""
    RAID_MOUNT_POINT=""
    RAID_MEMBER_DISKS=""
    RAID_TYPE="none"

    # Check for software RAID (mdadm)
    if [[ -f /proc/mdstat ]]; then
        local arrays
        arrays=$(grep '^md' /proc/mdstat | awk '{print $1}' || true)

        if [[ -n "${arrays}" ]]; then
            # Use the first active array found
            for arr in ${arrays}; do
                local detail
                detail=$(mdadm --detail "/dev/${arr}" 2>/dev/null || true)

                if [[ -n "${detail}" ]]; then
                    RAID_ARRAY_NAME="${arr}"
                    RAID_ARRAY_DEVICE="/dev/${arr}"
                    RAID_TYPE="software"

                    # Extract RAID level
                    RAID_LEVEL=$(echo "${detail}" | grep "Raid Level" | awk -F: '{print $2}' | xargs || echo "unknown")

                    # Extract mount point
                    RAID_MOUNT_POINT=$(findmnt -n -o TARGET "/dev/${arr}" 2>/dev/null || echo "")
                    if [[ -z "${RAID_MOUNT_POINT}" ]]; then
                        RAID_MOUNT_POINT=$(mount | grep "/dev/${arr}" | awk '{print $3}' || echo "/data")
                    fi

                    # Extract member disks
                    local members
                    members=$(echo "${detail}" | grep -E '^\s+/dev/' | awk '{print $NF}' || true)
                    RAID_MEMBER_DISKS=""
                    for disk in ${members}; do
                        local dname
                        dname=$(basename "${disk}")
                        if [[ -n "${RAID_MEMBER_DISKS}" ]]; then
                            RAID_MEMBER_DISKS="${RAID_MEMBER_DISKS},"
                        fi
                        RAID_MEMBER_DISKS="${RAID_MEMBER_DISKS}{\"device\": \"${disk}\", \"name\": \"${dname}\"}"
                    done

                    log_ok "Detected software RAID: ${RAID_ARRAY_DEVICE} (${RAID_LEVEL})"
                    log_info "  Mount point: ${RAID_MOUNT_POINT}"
                    log_info "  Members: ${members}"
                    break
                fi
            done
        fi
    fi

    # Check for hardware RAID (MegaRAID) if no software RAID found
    if [[ "${RAID_TYPE}" == "none" ]] && command -v smartctl &>/dev/null; then
        # Check for megaraid controller
        if smartctl --scan | grep -q "megaraid" 2>/dev/null; then
            RAID_TYPE="hardware"
            RAID_LEVEL="Hardware RAID (MegaRAID)"

            local mega_disks
            mega_disks=$(smartctl --scan | grep "megaraid" || true)
            RAID_MEMBER_DISKS=""
            local disk_num=0

            while IFS= read -r line; do
                local device type_str
                device=$(echo "${line}" | awk '{print $1}')
                type_str=$(echo "${line}" | awk '{print $3}')

                if [[ -n "${RAID_MEMBER_DISKS}" ]]; then
                    RAID_MEMBER_DISKS="${RAID_MEMBER_DISKS},"
                fi
                RAID_MEMBER_DISKS="${RAID_MEMBER_DISKS}{\"device\": \"${device}\", \"type\": \"${type_str}\", \"name\": \"Disk ${disk_num}\"}"
                disk_num=$((disk_num + 1))
            done <<< "${mega_disks}"

            log_ok "Detected hardware RAID (MegaRAID) with ${disk_num} disks"
        fi
    fi

    # Fallback: detect individual disks for SMART monitoring
    if [[ "${RAID_TYPE}" == "none" ]]; then
        log_warn "No RAID array detected. Detecting individual disks for SMART monitoring..."
        RAID_TYPE="disks_only"
        RAID_LEVEL="No RAID"

        local all_disks
        all_disks=$(lsblk -dno NAME,TYPE | grep "disk" | awk '{print $1}' || true)
        RAID_MEMBER_DISKS=""

        for dname in ${all_disks}; do
            # Skip loop, ram, and virtual devices
            [[ "${dname}" == loop* ]] && continue
            [[ "${dname}" == ram* ]] && continue
            [[ "${dname}" == sr* ]] && continue

            if [[ -n "${RAID_MEMBER_DISKS}" ]]; then
                RAID_MEMBER_DISKS="${RAID_MEMBER_DISKS},"
            fi
            RAID_MEMBER_DISKS="${RAID_MEMBER_DISKS}{\"device\": \"/dev/${dname}\", \"name\": \"${dname}\"}"
        done

        log_info "Detected disks for SMART monitoring: ${all_disks}"
    fi
}

# ==================== Email Configuration ====================
configure_email() {
    log_step "Configuring email alerts..."

    local hostname_fqdn
    hostname_fqdn=$(hostname -f 2>/dev/null || hostname)

    echo ""
    echo -e "${CYAN}========================================${NC}"
    echo -e "${CYAN}  Email Alert Configuration${NC}"
    echo -e "${CYAN}========================================${NC}"
    echo ""

    read -rp "SMTP server hostname [localhost]: " SMTP_SERVER
    SMTP_SERVER="${SMTP_SERVER:-localhost}"

    read -rp "SMTP port [25]: " SMTP_PORT
    SMTP_PORT="${SMTP_PORT:-25}"

    read -rp "Sender email (from) [gslmon@${hostname_fqdn}]: " EMAIL_FROM
    EMAIL_FROM="${EMAIL_FROM:-gslmon@${hostname_fqdn}}"

    read -rp "Alert recipient email (to): " EMAIL_TO
    while [[ -z "${EMAIL_TO}" ]]; do
        log_err "Recipient email is required"
        read -rp "Alert recipient email (to): " EMAIL_TO
    done

    read -rp "Server name for email subjects [${hostname_fqdn}]: " SERVER_NAME
    SERVER_NAME="${SERVER_NAME:-${hostname_fqdn}}"

    echo ""
    log_ok "Email configured: ${EMAIL_FROM} -> ${EMAIL_TO} via ${SMTP_SERVER}:${SMTP_PORT}"
}

# ==================== Generate Config ====================
generate_config() {
    log_step "Generating configuration file..."

    local array_device_json=""
    local array_name_json=""
    local mount_point_json=""

    if [[ "${RAID_TYPE}" == "software" ]]; then
        array_device_json="\"${RAID_ARRAY_DEVICE}\""
        array_name_json="\"${RAID_ARRAY_NAME}\""
        mount_point_json="\"${RAID_MOUNT_POINT}\""
    else
        array_device_json='""'
        array_name_json='""'
        mount_point_json='""'
    fi

    cat > "${CONFIG_DIR}/config.json" <<CONFIGEOF
{
    "email": {
        "smtp_server": "${SMTP_SERVER}",
        "smtp_port": ${SMTP_PORT},
        "from": "${EMAIL_FROM}",
        "to": "${EMAIL_TO}",
        "server_name": "${SERVER_NAME}"
    },
    "raid": {
        "array_device": ${array_device_json},
        "array_name": ${array_name_json},
        "raid_level": "${RAID_LEVEL}",
        "mount_point": ${mount_point_json},
        "member_disks": [
            ${RAID_MEMBER_DISKS}
        ]
    },
    "monitoring": {
        "log_check_interval_seconds": 30,
        "mdstat_check_interval_seconds": 3600,
        "smart_test_interval_days": 2,
        "smart_check_interval_hours": 6,
        "rebuild_check_interval_minutes": 30,
        "alert_cooldown_minutes": 15,
        "log_patterns": [
            "DID_BAD_TARGET",
            "I/O error",
            "degraded",
            "Disk failure",
            "faulty",
            "removed",
            "hardreset",
            "frozen",
            "NCQ",
            "ata[0-9].*error",
            "ata[0-9].*exception",
            "md/raid",
            "super_written",
            "journal abort",
            "EXT4-fs.*error",
            "SMART.*error",
            "Reallocated",
            "Uncorrectable",
            "Current_Pending",
            "read error",
            "write error",
            "recovering",
            "disable device",
            "FPDMA",
            "hard resetting link",
            "link is slow"
        ],
        "smart_critical_attribute_ids": [5, 10, 171, 172, 184, 187, 188, 197, 198, 199, 201]
    },
    "log_file": "${LOG_DIR}/gslmon.log",
    "state_file": "${STATE_DIR}/state.json",
    "tmp_dir": "${TMP_DIR}",
    "pid_file": "${PID_DIR}/gslmon.pid"
}
CONFIGEOF

    chmod 640 "${CONFIG_DIR}/config.json"
    log_ok "Configuration written to ${CONFIG_DIR}/config.json"
}

# ==================== Compile and Install ====================
compile_and_install() {
    log_step "Compiling gslmon..."

    # Find main.go — check current directory, parent, and repo root
    local source_file=""
    for candidate in "./main.go" "../main.go" "$(dirname "$0")/../main.go"; do
        if [[ -f "${candidate}" ]]; then
            source_file="${candidate}"
            break
        fi
    done

    if [[ -z "${source_file}" ]]; then
        log_err "Cannot find main.go source file."
        log_err "Please run this installer from the gslmon repository root or the installer/ directory."
        exit 1
    fi

    log_info "Source: ${source_file}"

    local build_dir
    build_dir=$(mktemp -d)

    cp "${source_file}" "${build_dir}/main.go"

    cd "${build_dir}"

    # Initialize Go module
    go mod init gslmon 2>/dev/null || true

    # Build with optimizations
    go build -ldflags="-s -w" -o gslmon main.go

    if [[ ! -f "gslmon" ]]; then
        log_err "Compilation failed"
        rm -rf "${build_dir}"
        exit 1
    fi

    log_ok "Compilation successful"

    # Install binary
    cp gslmon "${INSTALL_DIR}/gslmon"
    chmod 755 "${INSTALL_DIR}/gslmon"
    log_ok "Binary installed to ${INSTALL_DIR}/gslmon"

    # Cleanup
    cd /
    rm -rf "${build_dir}"
}

# ==================== Install Service ====================
install_service() {
    log_step "Installing systemd service..."

    cat > /etc/systemd/system/gslmon.service <<SERVICEEOF
# gslmon - RAID & Disk Health Monitor Daemon
# Copyright (C) 2026 GetSetLive Pvt Ltd
# Source Code Free to Distribute under GPL License
# Auto-generated by gslmon installer

[Unit]
Description=GSL RAID & Disk Health Monitor Daemon
After=network-online.target mdmonitor.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/gslmon ${CONFIG_DIR}/config.json
WorkingDirectory=${STATE_DIR}
Restart=always
RestartSec=15
StandardOutput=journal
StandardError=journal
SyslogIdentifier=gslmon
NoNewPrivileges=no
ProtectSystem=false
ProtectHome=false

[Install]
WantedBy=multi-user.target
SERVICEEOF

    systemctl daemon-reload
    log_ok "Service installed as gslmon.service"
}

# ==================== Create Directories ====================
create_directories() {
    log_step "Creating runtime directories..."
    mkdir -p "${CONFIG_DIR}"
    mkdir -p "${LOG_DIR}"
    mkdir -p "${STATE_DIR}"
    mkdir -p "${TMP_DIR}"
    mkdir -p "${PID_DIR}"
    log_ok "Directories created"
}

# ==================== Summary ====================
print_summary() {
    echo ""
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}  gslmon Installation Complete${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""
    echo -e "  Binary:      ${INSTALL_DIR}/gslmon"
    echo -e "  Config:      ${CONFIG_DIR}/config.json"
    echo -e "  Log file:    ${LOG_DIR}/gslmon.log"
    echo -e "  State file:  ${STATE_DIR}/state.json"
    echo -e "  PID file:    ${PID_DIR}/gslmon.pid"
    echo -e "  Service:     gslmon.service"
    echo ""

    if [[ "${RAID_TYPE}" == "software" ]]; then
        echo -e "  RAID:        ${RAID_ARRAY_DEVICE} (${RAID_LEVEL})"
        echo -e "  Mount:       ${RAID_MOUNT_POINT}"
    elif [[ "${RAID_TYPE}" == "hardware" ]]; then
        echo -e "  RAID:        ${RAID_LEVEL}"
    else
        echo -e "  RAID:        None detected (SMART monitoring only)"
    fi

    echo -e "  Email:       ${EMAIL_FROM} -> ${EMAIL_TO}"
    echo -e "  SMTP:        ${SMTP_SERVER}:${SMTP_PORT}"
    echo ""
    echo -e "  ${CYAN}Commands:${NC}"
    echo -e "    sudo systemctl start gslmon      # Start the daemon"
    echo -e "    sudo systemctl enable gslmon     # Enable auto-start on boot"
    echo -e "    sudo systemctl status gslmon     # Check status"
    echo -e "    tail -f ${LOG_DIR}/gslmon.log    # Follow logs"
    echo ""

    read -rp "Start gslmon now? [Y/n]: " START_NOW
    START_NOW="${START_NOW:-Y}"

    if [[ "${START_NOW}" =~ ^[Yy] ]]; then
        systemctl enable gslmon
        systemctl start gslmon
        sleep 2
        if systemctl is-active --quiet gslmon; then
            log_ok "gslmon is running! A startup health email has been sent to ${EMAIL_TO}"
        else
            log_err "gslmon failed to start. Check: journalctl -u gslmon -n 20"
        fi
    else
        log_info "To start later: sudo systemctl enable --now gslmon"
    fi
}

# ==================== Main ====================
main() {
    echo ""
    echo -e "${CYAN}========================================${NC}"
    echo -e "${CYAN}  gslmon Installer${NC}"
    echo -e "${CYAN}  RAID & Disk Health Monitor Daemon${NC}"
    echo -e "${CYAN}  Copyright (C) 2026 GetSetLive Pvt Ltd${NC}"
    echo -e "${CYAN}========================================${NC}"
    echo ""

    # Handle --uninstall flag
    if [[ "${1:-}" == "--uninstall" ]]; then
        check_root
        do_uninstall
    fi

    check_root
    detect_os
    detect_package_manager

    # Check if already installed
    if systemctl is-active --quiet gslmon 2>/dev/null; then
        log_warn "gslmon is already running!"
        read -rp "Reinstall? This will stop the current instance. [y/N]: " REINSTALL
        if [[ ! "${REINSTALL}" =~ ^[Yy] ]]; then
            log_info "Installation cancelled"
            exit 0
        fi
        systemctl stop gslmon
    fi

    # Step 1: Install dependencies
    log_step "=== Phase 1: Dependencies ==="
    install_smartmontools
    install_mdadm
    check_and_install_go

    # Step 2: Detect RAID
    log_step "=== Phase 2: RAID Detection ==="
    detect_raid_config

    # Step 3: Configure email
    log_step "=== Phase 3: Email Configuration ==="
    configure_email

    # Step 4: Create directories and config
    log_step "=== Phase 4: Installation ==="
    create_directories
    generate_config

    # Step 5: Compile and install
    compile_and_install

    # Step 6: Install service
    install_service

    # Done
    print_summary
}

main "$@"

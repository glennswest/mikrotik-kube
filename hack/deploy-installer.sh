#!/usr/bin/env bash
# deploy-installer.sh — Deploy the mkube-installer for first-time bootstrap.
#
# This creates the installer container on the device. On first run, it:
#   1. Creates the registry container
#   2. Seeds images from GHCR
#   3. Creates mkube-update (which bootstraps mkube)
#
# Usage:
#   ./hack/deploy-installer.sh <device>
#   make deploy-installer DEVICE=rose1.gw.lo

set -euo pipefail

DEVICE="${1:?Usage: deploy-installer.sh <device>}"
SSH_USER="${SSH_USER:-admin}"
SSH_OPTS="-o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
SSH="ssh ${SSH_OPTS} ${SSH_USER}@${DEVICE}"

# ── Configuration ────────────────────────────────────────────────────────────
BRIDGE_NAME="bridge-gt"
INSTALLER_VETH="veth-installer"
INSTALLER_IP="192.168.200.4/24"
INSTALLER_GW="192.168.200.1"
INSTALLER_NAME="mkube-installer"
INSTALLER_ROOT_DIR="/raid1/images/${INSTALLER_NAME}"
DNS_SERVER="192.168.200.199"
VOLUME_DIR="/raid1/volumes"
TARBALL_DIR="/raid1/tarballs"

# Check for local binary or build it
ARCH="${ARCH:-arm64}"
BINARY="dist/mkube-installer-${ARCH}"
if [ ! -f "${BINARY}" ]; then
    echo "▸ Building installer binary..."
    CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build \
        -ldflags "-s -w -X main.version=dev" \
        -o "${BINARY}" ./cmd/installer/
fi

ros() {
    ${SSH} "$1" | tr -d '\r'
}

wait_state() {
    local name="$1" target="$2" max=30 i=0
    printf "  Waiting for %s -> %s " "$name" "$target"
    while [ $i -lt $max ]; do
        local output
        output=$(ros "/container/print" 2>/dev/null || true)
        if [ "$target" = "missing" ]; then
            if ! echo "$output" | grep -q "$name"; then
                printf "done\n"; return 0
            fi
        elif echo "$output" | grep "$name" | grep -qE "^\s*[0-9]+\s+${target}\s"; then
            printf "done\n"; return 0
        fi
        printf "."; i=$((i + 1)); sleep 2
    done
    printf " timeout!\n"; return 1
}

echo "═══════════════════════════════════════════════════════════"
echo "  Deploying mkube-installer to ${DEVICE}"
echo "═══════════════════════════════════════════════════════════"
echo ""

# ── Verify connectivity ──────────────────────────────────────────────────────
echo "▸ Checking device connectivity..."
if ! ${SSH} '/system/resource/print' &>/dev/null; then
    echo "  ✗ Cannot connect to ${SSH_USER}@${DEVICE}"
    exit 1
fi
echo "  ✓ Connected to ${DEVICE}"

# ── Create directories ───────────────────────────────────────────────────────
echo ""
echo "▸ Creating volume directories..."
sftp ${SSH_OPTS} "${SSH_USER}@${DEVICE}" <<SFTP_EOF 2>/dev/null || true
-mkdir ${VOLUME_DIR}/${INSTALLER_NAME}
-mkdir ${VOLUME_DIR}/${INSTALLER_NAME}/config
-mkdir ${VOLUME_DIR}/${INSTALLER_NAME}/data
SFTP_EOF
echo "  ✓ Directories ready"

# ── Upload config ────────────────────────────────────────────────────────────
echo ""
echo "▸ Uploading installer config..."
scp ${SSH_OPTS} "deploy/installer-config.yaml" "${SSH_USER}@${DEVICE}:${VOLUME_DIR}/${INSTALLER_NAME}/config/config.yaml"
echo "  ✓ Config uploaded"

# ── Create mounts ────────────────────────────────────────────────────────────
echo ""
echo "▸ Creating mounts..."
EXISTING_CONFIG=$(ros "/container/mounts/print count-only where list=${INSTALLER_NAME}.config and dst=/etc/installer")
if [ "${EXISTING_CONFIG}" = "0" ] || [ -z "${EXISTING_CONFIG}" ]; then
    ros "/container/mounts/add list=${INSTALLER_NAME}.config src=/${VOLUME_DIR}/${INSTALLER_NAME}/config dst=/etc/installer" 2>/dev/null
    echo "  ✓ Config mount created"
else
    echo "  ✓ Config mount already exists"
fi

EXISTING_DATA=$(ros "/container/mounts/print count-only where list=${INSTALLER_NAME}.data and dst=/data")
if [ "${EXISTING_DATA}" = "0" ] || [ -z "${EXISTING_DATA}" ]; then
    ros "/container/mounts/add list=${INSTALLER_NAME}.data src=/${VOLUME_DIR}/${INSTALLER_NAME}/data dst=/data" 2>/dev/null
    echo "  ✓ Data mount created"
else
    echo "  ✓ Data mount already exists"
fi

# ── Create veth ──────────────────────────────────────────────────────────────
echo ""
echo "▸ Creating installer veth..."
ros "/interface/veth/add name=${INSTALLER_VETH} address=${INSTALLER_IP} gateway=${INSTALLER_GW}" >/dev/null 2>&1 && echo "  ✓ Veth created" || echo "  ✓ Veth already exists"
ros "/interface/bridge/port/add bridge=${BRIDGE_NAME} interface=${INSTALLER_VETH}" >/dev/null 2>&1 && echo "  ✓ Bridge port added" || echo "  ✓ Bridge port already configured"

# ── Build tarball ────────────────────────────────────────────────────────────
echo ""
echo "▸ Building installer tarball..."
bash hack/make-tarball-generic.sh "${BINARY}" "${INSTALLER_NAME}" "dist/mkube-installer.tar"

# ── Upload tarball ───────────────────────────────────────────────────────────
echo ""
echo "▸ Uploading tarball..."
REMOTE_TARBALL="${TARBALL_DIR}/${INSTALLER_NAME}.tar"
scp ${SSH_OPTS} "dist/mkube-installer.tar" "${SSH_USER}@${DEVICE}:${REMOTE_TARBALL}"
echo "  ✓ Upload complete"

# ── Stop and remove existing installer ───────────────────────────────────────
echo ""
echo "▸ Checking for existing installer..."
EXISTING=$(ros "/container/print count-only where name=${INSTALLER_NAME}")
if [ -n "${EXISTING}" ] && [ "${EXISTING}" != "0" ]; then
    echo "  Removing existing installer..."
    ros "/container/stop [find name=${INSTALLER_NAME}]" 2>/dev/null || true
    wait_state "${INSTALLER_NAME}" "S" 2>/dev/null || true
    ros "/container/remove [find name=${INSTALLER_NAME}]" 2>/dev/null || true
    wait_state "${INSTALLER_NAME}" "missing" 2>/dev/null || true
    echo "  ✓ Removed old installer"
else
    echo "  No existing installer found"
fi

# ── Create and start installer ───────────────────────────────────────────────
echo ""
echo "▸ Creating installer container..."
ros "/container/add file=${REMOTE_TARBALL} interface=${INSTALLER_VETH} root-dir=${INSTALLER_ROOT_DIR} name=${INSTALLER_NAME} start-on-boot=no logging=yes dns=${DNS_SERVER} hostname=${INSTALLER_NAME} mountlists=${INSTALLER_NAME}.config,${INSTALLER_NAME}.data"
echo "  ✓ Container created"

echo ""
echo "▸ Waiting for extraction..."
wait_state "${INSTALLER_NAME}" "S"

echo "▸ Starting installer..."
ros "/container/start [find name=${INSTALLER_NAME}]"
echo "  ✓ Installer started"

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  Installer deployed and running!"
echo ""
echo "  It will:"
echo "    1. Create registry container at 192.168.200.3:5000"
echo "    2. Seed images from GHCR"
echo "    3. Create mkube-update (which bootstraps mkube)"
echo "    4. Exit when done"
echo ""
echo "  Monitor progress:"
echo "    ssh ${SSH_USER}@${DEVICE} '/log/print follow where topics~\"container\"'"
echo "═══════════════════════════════════════════════════════════"

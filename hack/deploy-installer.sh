#!/usr/bin/env bash
# deploy-installer.sh — Bootstrap a fresh RouterOS device with mkube.
#
# This builds the mkube-installer CLI and runs it against the device.
# The installer runs locally on your workstation and uses REST API + SSH
# to configure the device. No tarballs — all images are OCI pulls.
#
# Usage:
#   ./hack/deploy-installer.sh <device>
#   make deploy-installer DEVICE=rose1.gw.lo

set -euo pipefail

DEVICE="${1:?Usage: deploy-installer.sh <device>}"

# Build the installer if not present
if [ ! -f dist/mkube-installer ]; then
    echo "▸ Building mkube-installer..."
    go build -ldflags "-s -w" -o dist/mkube-installer ./cmd/installer/
fi

echo "▸ Running mkube-installer against ${DEVICE}..."
exec dist/mkube-installer --device "${DEVICE}"

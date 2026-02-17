#!/usr/bin/env bash
set -euo pipefail

# Create a docker-save compatible tarball from a static binary.
# RouterOS expects this format (with manifest.json).
#
# Usage: make-tarball.sh <binary> <config> <output.tar>

BINARY="${1:?Usage: make-tarball.sh <binary> <config> <output.tar>}"
CONFIG="${2:?Usage: make-tarball.sh <binary> <config> <output.tar>}"
OUTPUT="${3:?Usage: make-tarball.sh <binary> <config> <output.tar>}"

WORK=$(mktemp -d)
trap "rm -rf ${WORK}" EXIT

# Build rootfs layer
LAYER_DIR="${WORK}/rootfs"
mkdir -p "${LAYER_DIR}/etc/mkube" "${LAYER_DIR}/etc/ssl/certs" "${LAYER_DIR}/usr/local/bin" "${LAYER_DIR}/data"
echo "root:x:0:0:root:/:/usr/local/bin/mkube" > "${LAYER_DIR}/etc/passwd"
echo "root:x:0:" > "${LAYER_DIR}/etc/group"
cp "${BINARY}" "${LAYER_DIR}/usr/local/bin/mkube"
chmod +x "${LAYER_DIR}/usr/local/bin/mkube"
cp "${CONFIG}" "${LAYER_DIR}/etc/mkube/config.yaml"

# Include CA certificates so Go's TLS can verify upstream registries
if [ -f /etc/ssl/cert.pem ]; then
    cp /etc/ssl/cert.pem "${LAYER_DIR}/etc/ssl/certs/ca-certificates.crt"
elif [ -f /etc/ssl/certs/ca-certificates.crt ]; then
    cp /etc/ssl/certs/ca-certificates.crt "${LAYER_DIR}/etc/ssl/certs/ca-certificates.crt"
else
    echo "WARNING: No CA certificate bundle found, TLS verification will fail" >&2
fi

# Create layer tarball
LAYER_TAR="${WORK}/layer.tar"
tar -C "${LAYER_DIR}" -cf "${LAYER_TAR}" .

# Compute layer diff ID (sha256 of uncompressed tar)
LAYER_SHA=$(shasum -a 256 "${LAYER_TAR}" | awk '{print $1}')
LAYER_ID="${LAYER_SHA}"

# Create layer directory in docker-save structure
LAYER_SAVE_DIR="${WORK}/${LAYER_ID}"
mkdir -p "${LAYER_SAVE_DIR}"
cp "${LAYER_TAR}" "${LAYER_SAVE_DIR}/layer.tar"
echo "1.0" > "${LAYER_SAVE_DIR}/VERSION"

# Create image config JSON
CONFIG_SHA=$(cat <<CFGJSON | shasum -a 256 | awk '{print $1}'
{
  "architecture": "arm64",
  "os": "linux",
  "config": {
    "Entrypoint": ["/usr/local/bin/mkube"],
    "Cmd": ["--config", "/etc/mkube/config.yaml"],
    "WorkingDir": "/"
  },
  "rootfs": {
    "type": "layers",
    "diff_ids": ["sha256:${LAYER_SHA}"]
  }
}
CFGJSON
)

cat > "${WORK}/${CONFIG_SHA}.json" <<CFGJSON
{
  "architecture": "arm64",
  "os": "linux",
  "config": {
    "Entrypoint": ["/usr/local/bin/mkube"],
    "Cmd": ["--config", "/etc/mkube/config.yaml"],
    "WorkingDir": "/"
  },
  "rootfs": {
    "type": "layers",
    "diff_ids": ["sha256:${LAYER_SHA}"]
  }
}
CFGJSON

# Create layer json (legacy docker format)
cat > "${LAYER_SAVE_DIR}/json" <<LAYERJSON
{
  "id": "${LAYER_ID}",
  "created": "1970-01-01T00:00:00Z",
  "config": {
    "Entrypoint": ["/usr/local/bin/mkube"],
    "Cmd": ["--config", "/etc/mkube/config.yaml"]
  }
}
LAYERJSON

# Create manifest.json
cat > "${WORK}/manifest.json" <<MANIFEST
[{
  "Config": "${CONFIG_SHA}.json",
  "RepoTags": ["mkube:latest"],
  "Layers": ["${LAYER_ID}/layer.tar"]
}]
MANIFEST

# Create repositories file
cat > "${WORK}/repositories" <<REPOS
{"mkube":{"latest":"${LAYER_ID}"}}
REPOS

# Build final docker-save tar
tar -C "${WORK}" -cf "${OUTPUT}" \
    manifest.json \
    repositories \
    "${CONFIG_SHA}.json" \
    "${LAYER_ID}/layer.tar" \
    "${LAYER_ID}/VERSION" \
    "${LAYER_ID}/json"

echo "Built ${OUTPUT} ($(du -h "${OUTPUT}" | cut -f1))"

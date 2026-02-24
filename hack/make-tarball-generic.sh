#!/usr/bin/env bash
set -euo pipefail

# Create a docker-save compatible tarball from a static binary.
# Generic version — binary name becomes the entrypoint.
#
# Usage: make-tarball-generic.sh <binary> <image-name> <output.tar>

BINARY="${1:?Usage: make-tarball-generic.sh <binary> <image-name> <output.tar>}"
IMAGE_NAME="${2:?Usage: make-tarball-generic.sh <binary> <image-name> <output.tar>}"
OUTPUT="${3:?Usage: make-tarball-generic.sh <binary> <image-name> <output.tar>}"

BIN_NAME=$(basename "${BINARY}")
WORK=$(mktemp -d)
trap "rm -rf ${WORK}" EXIT

# Build rootfs layer
LAYER_DIR="${WORK}/rootfs"
mkdir -p "${LAYER_DIR}/etc/ssl/certs" "${LAYER_DIR}/usr/local/bin" "${LAYER_DIR}/data"
echo "root:x:0:0:root:/:/usr/local/bin/${BIN_NAME}" > "${LAYER_DIR}/etc/passwd"
echo "root:x:0:" > "${LAYER_DIR}/etc/group"
cp "${BINARY}" "${LAYER_DIR}/usr/local/bin/${BIN_NAME}"
chmod +x "${LAYER_DIR}/usr/local/bin/${BIN_NAME}"

# Include CA certificates
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

# Compute layer diff ID
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
    "Entrypoint": ["/usr/local/bin/${BIN_NAME}"],
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
    "Entrypoint": ["/usr/local/bin/${BIN_NAME}"],
    "WorkingDir": "/"
  },
  "rootfs": {
    "type": "layers",
    "diff_ids": ["sha256:${LAYER_SHA}"]
  }
}
CFGJSON

# Create layer json
cat > "${LAYER_SAVE_DIR}/json" <<LAYERJSON
{
  "id": "${LAYER_ID}",
  "created": "1970-01-01T00:00:00Z",
  "config": {
    "Entrypoint": ["/usr/local/bin/${BIN_NAME}"]
  }
}
LAYERJSON

# Create manifest.json
cat > "${WORK}/manifest.json" <<MANIFEST
[{
  "Config": "${CONFIG_SHA}.json",
  "RepoTags": ["${IMAGE_NAME}:latest"],
  "Layers": ["${LAYER_ID}/layer.tar"]
}]
MANIFEST

# Create repositories file
cat > "${WORK}/repositories" <<REPOS
{"${IMAGE_NAME}":{"latest":"${LAYER_ID}"}}
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

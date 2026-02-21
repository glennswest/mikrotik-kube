#!/bin/bash
# Populate pxemanager with hosts discovered from DHCP leases on g11.
#
# Fetches leases from microdns at dns.g11.lo:8080, derives server names
# using the convention: port = last_octet - 10, name = server{port}
# Then registers each host in pxemanager and configures IPMI.

set -euo pipefail

DHCP_URL="${DHCP_URL:-http://192.168.11.199:8080/api/v1/leases}"
PXE_URL="${PXE_URL:-http://pxe.g10.lo:8080}"
DEFAULT_IMAGE="${DEFAULT_IMAGE:-localboot}"
DRY_RUN="${DRY_RUN:-false}"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

# Fetch leases
log "Fetching DHCP leases from $DHCP_URL"
LEASES=$(curl -sf "$DHCP_URL") || { echo "ERROR: cannot reach $DHCP_URL"; exit 1; }

COUNT=$(echo "$LEASES" | jq 'length')
log "Found $COUNT leases"

if [ "$COUNT" -eq 0 ]; then
    echo "No leases found, nothing to do."
    exit 0
fi

# Get existing hosts from pxemanager for dedup
log "Fetching existing hosts from $PXE_URL/api/hosts"
EXISTING=$(curl -sf "$PXE_URL/api/hosts" 2>/dev/null || echo "[]")

echo "$LEASES" | jq -c '.[]' | while read -r lease; do
    IP=$(echo "$lease" | jq -r '.ip // .address // .ip_address // empty')
    MAC=$(echo "$lease" | jq -r '.mac // .mac_address // .hw_address // empty')

    if [ -z "$IP" ] || [ -z "$MAC" ]; then
        continue
    fi

    # Derive server name: last octet - 10
    LAST_OCTET=$(echo "$IP" | awk -F. '{print $4}')
    PORT=$((LAST_OCTET - 9))

    if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 8 ]; then
        log "Skipping $IP (server$PORT outside server1-server8 range)"
        continue
    fi

    HOSTNAME="server${PORT}"

    # Check if already registered
    ALREADY=$(echo "$EXISTING" | jq -r --arg mac "$MAC" '.[] | select(.mac == $mac) | .hostname')
    if [ -n "$ALREADY" ]; then
        log "SKIP $HOSTNAME ($MAC) - already registered as '$ALREADY'"
        continue
    fi

    log "REGISTER $HOSTNAME  mac=$MAC  ip=$IP  image=$DEFAULT_IMAGE"

    if [ "$DRY_RUN" = "true" ]; then
        continue
    fi

    # Register host
    HTTP_CODE=$(curl -sf -o /dev/null -w '%{http_code}' \
        -X POST "$PXE_URL/api/hosts" \
        -H "Content-Type: application/json" \
        -d "{\"mac\":\"$MAC\",\"hostname\":\"$HOSTNAME\",\"current_image\":\"$DEFAULT_IMAGE\"}")

    if [ "$HTTP_CODE" -ge 200 ] && [ "$HTTP_CODE" -lt 300 ]; then
        log "  -> registered ($HTTP_CODE)"
    else
        log "  -> FAILED ($HTTP_CODE)"
        continue
    fi

    # Configure IPMI (hostname.g11.lo pattern, default ADMIN/ADMIN)
    IPMI_IP="${HOSTNAME}.g11.lo"
    HTTP_CODE=$(curl -sf -o /dev/null -w '%{http_code}' \
        -X POST "$PXE_URL/api/host/ipmi/config?host=$HOSTNAME" \
        -d "ipmi_ip=$IPMI_IP&ipmi_username=ADMIN&ipmi_password=ADMIN")

    if [ "$HTTP_CODE" -ge 200 ] && [ "$HTTP_CODE" -lt 300 ]; then
        log "  -> IPMI configured ($IPMI_IP)"
    else
        log "  -> IPMI config failed ($HTTP_CODE)"
    fi
done

log "Done. Verify with: curl $PXE_URL/api/hosts | jq ."

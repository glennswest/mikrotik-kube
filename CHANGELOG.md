# Changelog

## [Unreleased]

### 2026-02-23
- **fix:** Enable NATS monitoring port (`-m 8222`) so liveness probe works — was causing max restart failures and JetStream stream-not-found errors
- **fix:** Prevent reconciler race with image redeploy goroutine — reconciler skips pods being redeployed
- **feat:** Image auto-update: proper digest headers, stale image detection for tracked pods
- **fix:** Image update pipeline: push-triggered reconcile, robust DeletePod, orphan detection
- **feat:** Add DHCP relay support with server_ip, user=0:0, and serverNetwork routing
- **fix:** PXE boot chain: point nextServer to pxe pod and add static DNS record
- **fix:** Orphaned static IP preventing DNS container recreation

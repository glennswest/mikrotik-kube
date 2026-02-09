# mikrotik-kube

A single-binary [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider for MikroTik RouterOS containers, with integrated network management (IPAM), storage management, systemd-like boot ordering, and an embedded OCI registry.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  mikrotik-kube (single Go binary, runs as RouterOS container)      │
│                                                                   │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────────────────┐  │
│  │ Virtual       │  │ Network      │  │ Storage Manager        │  │
│  │ Kubelet +     │  │ Manager      │  │ • OCI→tarball convert  │  │
│  │ RouterOS      │  │ • IPAM pool  │  │ • Volume provisioning  │  │
│  │ Provider      │  │ • veth/bridge│  │ • Garbage collection   │  │
│  └──────┬───────┘  └──────┬───────┘  └────────────┬───────────┘  │
│         │                 │                        │              │
│  ┌──────┴─────────────────┴────────────────────────┴───────────┐  │
│  │                  RouterOS REST API Client                    │  │
│  └─────────────────────────┬───────────────────────────────────┘  │
│                            │                                      │
│  ┌─────────────────────────┴───────────────────────────────────┐  │
│  │  Systemd Manager (boot ordering, health checks, watchdog)   │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                                                   │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │  Embedded OCI Registry (:5000, pull-through cache)          │  │
│  └─────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
         │
         ▼  RouterOS REST API (/rest/container/*)
┌──────────────────────────────────────────────────────────────────┐
│  MikroTik RouterOS Container Runtime                              │
│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                                │
│  │ C1  │ │ C2  │ │ C3  │ │ C4  │  ...                           │
│  └─────┘ └─────┘ └─────┘ └─────┘                                │
└──────────────────────────────────────────────────────────────────┘
```

## Features

### Virtual Kubelet Provider
- Full `PodLifecycleHandler` implementation for RouterOS
- Translates K8s Pod specs to RouterOS container API calls
- Runs standalone (local reconciler) or connected to a K8s API server
- Custom annotations for MikroTik-specific config

### Network Manager (IPAM)
- Sequential IP allocation from a configurable CIDR pool
- Automatic veth interface creation and bridge port assignment
- Syncs with existing allocations on startup (survives restarts)
- Per-container network isolation via RouterOS bridge

### Storage Manager
- OCI image to RouterOS tarball conversion pipeline
- Local tarball cache with LRU garbage collection
- Volume provisioning with per-container directory isolation
- Orphaned volume cleanup

### Systemd Manager
- Boot ordering with dependency resolution (topological sort)
- Health checks: HTTP probes, TCP probes, status polling
- Watchdog with configurable restart policies and backoff
- Max restart limits to prevent crash loops

### Embedded Registry
- OCI Distribution Spec v2 compatible
- Pull-through cache for Docker Hub, GHCR, etc.
- Local image storage for air-gapped deployments

## Quick Start

### 1. Build

```bash
# For ARM64 MikroTik devices (hAP ax3, RB5009, etc.)
make tarball ARCH=arm64

# For x86 MikroTik CHR
make tarball ARCH=amd64

# For local development
make build-local
```

### 2. Deploy to RouterOS

```bash
# Upload the tarball
make push DEVICE=192.168.88.1 ARCH=arm64
```

Or manually:
```bash
scp dist/mikrotik-kube-dev-arm64.tar admin@192.168.88.1:/mikrotik-kube.tar
```

### 3. Configure RouterOS

```routeros
# Enable container mode
/system/device-mode/update container=yes

# Create bridge for containers
/interface/bridge add name=containers
/ip/address add address=172.20.0.1/16 interface=containers

# Create veth for the management container
/interface/veth add name=veth-mgmt address=172.20.0.2/16 gateway=172.20.0.1
/interface/bridge/port add bridge=containers interface=veth-mgmt

# NAT for container internet access
/ip/firewall/nat add chain=srcnat src-address=172.20.0.0/16 action=masquerade

# Create and start mikrotik-kube
/container add \
    file=mikrotik-kube.tar \
    interface=veth-mgmt \
    root-dir=/container-data/mikrotik-kube \
    logging=yes \
    start-on-boot=yes \
    hostname=mikrotik-kube \
    dns=8.8.8.8

/container start [find name~"mikrotik-kube"]
```

### 4. Define Your Containers

Create pod manifests in `/etc/mikrotik-kube/boot-order.yaml` (see `deploy/boot-order.yaml` for examples).

## Configuration

See `deploy/config.yaml` for all options. Key settings:

| Setting | Default | Description |
|---------|---------|-------------|
| `routeros.restUrl` | `https://192.168.88.1/rest` | RouterOS REST API endpoint |
| `network.podCIDR` | `172.20.0.0/16` | IP range for containers |
| `network.bridgeName` | `containers` | RouterOS bridge name |
| `storage.gcIntervalMinutes` | `30` | How often to run GC |
| `systemd.maxRestarts` | `5` | Max restarts before marking failed |
| `registry.enabled` | `true` | Enable embedded OCI registry |

## Custom Annotations

| Annotation | Description |
|-----------|-------------|
| `mikrotik.io/boot-priority` | Integer boot order (lower = first) |
| `mikrotik.io/depends-on` | Comma-separated container dependencies |

## Operating Modes

### Standalone Mode (default)
Reads pod manifests from a local YAML file and reconciles against RouterOS. No Kubernetes cluster required.

```bash
mikrotik-kube --standalone --config /etc/mikrotik-kube/config.yaml
```

### Virtual Kubelet Mode
Registers as a node in an existing Kubernetes cluster. Pods scheduled to this node are created on RouterOS.

```bash
mikrotik-kube --kubeconfig /path/to/kubeconfig --node-name my-mikrotik
```

Then from kubectl:
```bash
kubectl apply -f pod.yaml  # with toleration for virtual-kubelet.io/provider=mikrotik
```

## Project Structure

```
cmd/mikrotik-kube/       Entry point, CLI flags, manager initialization
pkg/
  config/              YAML configuration with CLI overrides
  routeros/            RouterOS REST API client (containers, veth, files)
  provider/            Virtual Kubelet PodLifecycleHandler implementation
  network/             IPAM allocator, veth/bridge management
  storage/             OCI-to-tarball conversion, volume provisioning, GC
  systemd/             Boot ordering, health checks, watchdog
  registry/            Embedded OCI registry with pull-through cache
deploy/                Configuration templates and boot-order examples
hack/                  Build and deployment scripts
```

## Development

```bash
make build-local    # Build for host platform
make test           # Run tests
make lint           # Lint
make clean          # Clean build artifacts
```

## License

MIT

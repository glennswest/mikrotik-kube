# Ignition Config CRD Specification

## Overview

The `IgnitionConfig` CRD stores Ignition (CoreOS) and Kickstart (Fedora/RHEL) configuration files and serves them over HTTP. Configs are matched to servers by hostname, IP address, MAC address, or role label. This enables iSCSI sanboot workflows where kernel args cannot be passed dynamically — the booted ISO fetches its config from a well-known URL.

## Motivation

Currently, PXE manager serves CoreOS and Fedora installs via kernel/initramfs PXE boot with config URLs baked into kernel args. Moving to iSCSI sanboot (via the existing `ISCSICdrom` CRD) is faster and simpler — but sanboot passes no kernel args. We need a way for the booted ISO to discover and fetch its configuration.

The solution: a generic config-serving CRD. The ISO's GRUB config is modified once to include `ignition.config.url=http://<mkube>/api/v1/ignition?ip=${ip}` (or `inst.ks=` for Fedora). mkube resolves the requesting server's IP to the correct config and returns it.

## CRD Definition

```yaml
apiVersion: v1
kind: IgnitionConfig
metadata:
  name: coreos-builder        # unique name
  namespace: g10               # network namespace
spec:
  type: ignition               # "ignition" or "kickstart"
  format: butane               # "butane" (auto-compiled), "ignition" (raw JSON), or "kickstart"
  config: |                    # inline config content
    variant: fcos
    version: "1.5.0"
    passwd:
      users:
        - name: root
          ...
  selector:                    # which servers get this config
    matchLabels:               # match BMH labels (optional)
      role: builder
    matchHostnames:            # match by hostname (optional)
      - server1
      - server2
    matchIPs:                  # match by IP (optional)
      - 192.168.10.10
      - 192.168.10.11
    matchMACs:                 # match by MAC (optional)
      - AC:1F:6B:8A:A7:9C
    default: false             # if true, serves as fallback when no other config matches
status:
  phase: Ready                 # Pending, Ready, Error
  compiledSize: 4096           # size of compiled ignition JSON (bytes)
  configHash: "sha256:abc..."  # hash of compiled config for caching
  lastCompiled: "2026-02-27T..."
  errorMessage: ""
```

## Types

```go
type IgnitionConfig struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec              IgnitionConfigSpec   `json:"spec"`
    Status            IgnitionConfigStatus `json:"status,omitempty"`
}

type IgnitionConfigSpec struct {
    Type     string                 `json:"type"`               // "ignition" or "kickstart"
    Format   string                 `json:"format"`             // "butane", "ignition", or "kickstart"
    Config   string                 `json:"config"`             // inline config content
    Selector IgnitionConfigSelector `json:"selector,omitempty"` // server matching rules
}

type IgnitionConfigSelector struct {
    MatchLabels    map[string]string `json:"matchLabels,omitempty"`
    MatchHostnames []string          `json:"matchHostnames,omitempty"`
    MatchIPs       []string          `json:"matchIPs,omitempty"`
    MatchMACs      []string          `json:"matchMACs,omitempty"`
    Default        bool              `json:"default,omitempty"`
}

type IgnitionConfigStatus struct {
    Phase         string `json:"phase"`
    CompiledSize  int64  `json:"compiledSize,omitempty"`
    ConfigHash    string `json:"configHash,omitempty"`
    LastCompiled  string `json:"lastCompiled,omitempty"`
    ErrorMessage  string `json:"errorMessage,omitempty"`
}

type IgnitionConfigList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata"`
    Items           []IgnitionConfig `json:"items"`
}
```

## API Endpoints

### CRUD (standard mkube CRD pattern)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/namespaces/{ns}/ignitionconfigs` | List all configs in namespace |
| GET | `/api/v1/namespaces/{ns}/ignitionconfigs/{name}` | Get a specific config |
| POST | `/api/v1/namespaces/{ns}/ignitionconfigs` | Create a config |
| PUT | `/api/v1/namespaces/{ns}/ignitionconfigs/{name}` | Update a config |
| DELETE | `/api/v1/namespaces/{ns}/ignitionconfigs/{name}` | Delete a config |

### Serving Endpoint (used by booting servers)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/ignition` | Serve compiled config for requesting server |

**Resolution logic** for `/api/v1/ignition`:

1. Extract client IP from request (`X-Forwarded-For` header or remote addr)
2. Look up BMH by IP across all namespaces → get hostname, MAC, labels
3. Search `IgnitionConfig` objects with this priority:
   - `matchMACs` containing this server's MAC
   - `matchIPs` containing this server's IP
   - `matchHostnames` containing this server's hostname
   - `matchLabels` matching BMH labels
   - `default: true` as fallback
4. Return the compiled config with appropriate `Content-Type`:
   - Ignition: `application/vnd.coreos.ignition+json`
   - Kickstart: `text/plain`
5. If no match: return `404 Not Found`

**Optional query parameters:**
- `?ip=<ip>` — override client IP detection (for testing or proxy scenarios)
- `?mac=<mac>` — match by MAC directly
- `?hostname=<name>` — match by hostname directly
- `?format=raw` — return raw config without compilation (for debugging)

### Kickstart Serving Endpoint

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/kickstart` | Serve kickstart config for requesting server |

Same resolution logic as `/api/v1/ignition`, filtered to `type: kickstart` configs.

## Butane Compilation

When `format: butane`, mkube compiles the Butane YAML to Ignition JSON on create/update:

1. Parse Butane YAML
2. Compile to Ignition JSON (equivalent to `butane --strict`)
3. Store compiled JSON in NATS alongside the source
4. Set `status.compiledSize`, `status.configHash`, `status.lastCompiled`
5. If compilation fails: set `status.phase: Error`, `status.errorMessage`

**Option A (preferred):** Embed the Butane Go library (`github.com/coreos/butane/config`) for native compilation.

**Option B:** Shell out to `butane` binary if library integration is too heavy.

**Option C:** Require pre-compiled Ignition JSON (`format: ignition`) and skip Butane support initially. Users compile locally with `butane --strict builder.bu > builder.ign`.

## Storage

- **Bucket:** `IGNITIONCONFIGS` in NATS KV store (same pattern as `ISCSICDROMS`, `BAREMETALHOSTS`)
- **Key format:** `{namespace}/{name}`
- **Value:** JSON-serialized `IgnitionConfig` with compiled config stored in an internal field

## Example Configs

### CoreOS Builder (installs to disk)

```yaml
apiVersion: v1
kind: IgnitionConfig
metadata:
  name: coreos-builder
  namespace: g10
spec:
  type: ignition
  format: ignition
  config: |
    {
      "ignition": { "version": "3.4.0" },
      "systemd": {
        "units": [{
          "name": "coreos-installer.service",
          "enabled": true,
          "contents": "[Unit]\nDescription=Install CoreOS to disk\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=oneshot\nExecStart=/usr/bin/coreos-installer install /dev/sda --ignition-url http://192.168.200.2:8082/api/v1/ignition?role=builder --insecure-ignition --append-karg console=tty0 --append-karg console=ttyS0,115200n8 --append-karg console=ttyS1,115200n8\nExecStartPost=/usr/bin/systemctl reboot\nStandardOutput=journal+console\nStandardError=journal+console\n\n[Install]\nWantedBy=multi-user.target\n"
        }]
      }
    }
  selector:
    matchLabels:
      role: builder
```

### CoreOS Builder Target Config (applied after install)

```yaml
apiVersion: v1
kind: IgnitionConfig
metadata:
  name: builder-target
  namespace: g10
spec:
  type: ignition
  format: butane
  config: |
    variant: fcos
    version: "1.5.0"
    passwd:
      users:
        - name: root
          password_hash: "$6$..."
          ssh_authorized_keys:
            - "ssh-rsa AAAA..."
        - name: core
          password_hash: "$6$..."
          ssh_authorized_keys:
            - "ssh-rsa AAAA..."
          groups: [wheel]
    kernel_arguments:
      should_exist:
        - console=tty0
        - console=ttyS0,115200n8
        - console=ttyS1,115200n8
    systemd:
      units:
        - name: podman.socket
          enabled: true
        - name: cockpit.socket
          enabled: true
    # ... (full builder.bu content)
  selector:
    default: false  # only served when explicitly requested via ?role=builder
```

### Fedora Server Kickstart

```yaml
apiVersion: v1
kind: IgnitionConfig
metadata:
  name: fedora-server
  namespace: g10
spec:
  type: kickstart
  format: kickstart
  config: |
    install
    url --url=https://download.fedoraproject.org/pub/fedora/linux/releases/43/Everything/x86_64/os/
    keyboard us
    lang en_US.UTF-8
    timezone UTC
    rootpw --plaintext admin
    sshkey --username=root "ssh-rsa AAAA..."
    bootloader --location=mbr --append="console=tty0 console=ttyS0,115200n8 console=ttyS1,115200n8"
    clearpart --all --initlabel
    autopart
    reboot
    %packages
    @^server-product-environment
    %end
  selector:
    matchLabels:
      role: server
```

## ISO Integration

### CoreOS Live ISO

Modify the CoreOS live ISO's GRUB config (`/EFI/fedora/grub.cfg` and `/isolinux/isolinux.cfg`) to add:

```
ignition.config.url=http://192.168.200.2:8082/api/v1/ignition
```

This way every server that sanboots this ISO fetches its own ignition config from mkube based on its IP.

### Fedora Netinstall ISO

Modify the Fedora ISO's GRUB config to add:

```
inst.ks=http://192.168.200.2:8082/api/v1/kickstart
```

### Serving the Right Config

The mkube endpoint sees the requesting IP, looks up the BMH, and returns the matching config. This means:
- One CoreOS ISO for all servers, different configs per server/role
- One Fedora ISO for all servers, different kickstarts per server/role

## Interaction with Existing CRDs

### BareMetalHost

Add optional `labels` field to BMHSpec for role-based matching:

```go
type BMHSpec struct {
    // ... existing fields ...
    Labels map[string]string `json:"labels,omitempty"` // e.g. {"role": "builder"}
}
```

### ISCSICdrom

No changes needed. The ISO images are served via `ISCSICdrom` as-is. The ignition/kickstart serving is orthogonal.

### PXE Manager

PXE manager's iPXE boot script changes from:

```
kernel http://pxe.g10.lo/files/coreos-kernel ignition.config.url=...
initrd http://pxe.g10.lo/files/coreos-initramfs
boot
```

To:

```
sanboot iscsi:192.168.10.1::::iqn.2000-02.com.mikrotik:cdrom-coreos-live
```

## Implementation Priority

1. **Phase 1:** CRUD endpoints + NATS storage (standard CRD boilerplate)
2. **Phase 2:** Serving endpoint with IP → BMH → config resolution
3. **Phase 3:** Butane compilation support (can defer — users provide pre-compiled JSON initially)

## Open Questions

1. Should the serving endpoint live on mkube (port 8082, reachable from g10 via routing) or should pxemanager proxy it (port 80, directly on the PXE network)?
2. Should we support config templating (e.g. `{{.Hostname}}`, `{{.IP}}` substitution in configs)?
3. Should the ISCSICdrom CRD be extended with an `ignitionConfig` reference to auto-link ISOs with their configs?

# CRD Reference

API reference for the Custom Resource Definitions used by the Talos Kubernetes Node Autoscaler.

## Overview

The autoscaler defines three CRDs under the `autoscaler.talos.dev/v1alpha1` API group:

| CRD | Scope | Purpose |
|-----|-------|---------|
| `MachineClass` | Namespaced | Defines a class of worker nodes (CPU, RAM, disk) |
| `MachineTemplate` | Namespaced | Tracks a specific VM instance |
| `MachineDeployment` | Namespaced | Manages a pool of machines for a class |

## MachineClass

Defines reusable machine specifications. Create one per worker tier.

### API

```yaml
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineClass
metadata:
  name: <string>           # Required. Unique name (e.g., "standard", "gpu")
  labels: <map[string]string>  # Optional. Metadata labels
spec:
  vcpu: <integer>          # Required. Number of virtual CPUs (1-64)
  memoryGiB: <integer>     # Required. Memory in GiB (1-512)
  diskGiB: <integer>       # Required. Disk size in GiB (10-4096)
  networkBridge: <string>  # Required. Proxmox bridge name (e.g., "vmbr0")
  storagePool: <string>    # Optional. Proxmox storage pool (e.g., "local-lvm")
  proxmoxPool: <string>    # Optional. Proxmox resource pool name
  macAddress: <string>     # Optional. Full MAC address for PXE boot matching
                           #   Format: "XX:XX:XX:XX:XX:XX"
                           #   If omitted, Proxmox assigns a random MAC
  serial: <string>         # Optional. SMBIOS serial number for identification
                           #   If omitted, no serial is set
  bootTimeout: <duration>  # Optional. Max time to wait for node join (default: "5m")
  diskType: <string>       # Optional. "virtio" or "scsi" (default: "virtio")
  scsihw: <string>         # Optional. SCSI controller type (default: "virtio-scsi-single")
  cpuType: <string>        # Optional. CPU type (default: "host")
  agent: <integer>         # Optional. QEMU guest agent (1=enabled, 0=disabled, default: 1)
  gpu:                     # Optional. GPU passthrough configuration
    vendor: <string>       # "nvidia" or "amd"
    model: <string>        # GPU model identifier
    pciAddress: <string>   # PCI address (e.g., "0000:01:00.0")
  labels: <map[string]string>  # Optional. Labels applied to provisioned nodes
  taints:                    # Optional. Taints applied to provisioned nodes
  - key: <string>
    value: <string>
    effect: <string>        # "NoSchedule", "PreferNoSchedule", "NoExecute"
status:
  ready: <boolean>          # Whether the class is available for provisioning
  nodeCount: <integer>      # Current number of nodes using this class
  lastProvisionTime: <time> # Timestamp of last successful provision
```

### Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `vcpu` | integer | Yes | Number of virtual CPUs (1-64) |
| `memoryGiB` | integer | Yes | Memory in GiB (1-512) |
| `diskGiB` | integer | Yes | Disk size in GiB (10-4096) |
| `networkBridge` | string | Yes | Proxmox network bridge name |
| `storagePool` | string | No | Proxmox storage pool for VM disks |
| `proxmoxPool` | string | No | Proxmox resource pool name |
| `macAddress` | string | No | Full MAC address (e.g., `52:54:00:AA:BB:CC`). Used for PXE boot config lookup. If omitted, Proxmox assigns a random MAC. |
| `serial` | string | No | SMBIOS serial number for VM identification |
| `bootTimeout` | string | No | Max wait time for node to join cluster (default: `5m`) |
| `diskType` | string | No | Disk bus type: `virtio` (default) or `scsi` |
| `scsihw` | string | No | SCSI controller type (default: `virtio-scsi-single`) |
| `cpuType` | string | No | CPU emulation type (default: `host`) |
| `agent` | integer | No | QEMU guest agent: 1=enabled (default), 0=disabled |
| `gpu` | object | No | GPU passthrough configuration |
| `gpu.vendor` | string | Yes (if gpu set) | GPU vendor: `nvidia` or `amd` |
| `gpu.model` | string | No | GPU model identifier |
| `gpu.pciAddress` | string | Yes (if gpu set) | PCI address (e.g., `0000:01:00.0`) |
| `labels` | map | No | Labels applied to Kubernetes nodes |
| `taints` | list | No | Taints applied to Kubernetes nodes |

### MAC Address and PXE Boot

The `macAddress` field is used by the PXE config server to look up the correct Talos machine configuration for each worker. When a new VM boots:

1. The VM PXE-boots and sends a DHCP request with its MAC address
2. The PXE/TFTP server provides the Talos kernel
3. Talos contacts the config server with its MAC address
4. The config server returns the machine config for that specific worker

If `macAddress` is omitted, the VM still PXE-boots but the config server must use an alternative identification method (e.g., SMBIOS serial from the `serial` field).

```yaml
# Example: explicit MAC for PXE config lookup
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineClass
metadata:
  name: standard
spec:
  vcpu: 4
  memoryGiB: 8
  diskGiB: 50
  networkBridge: vmbr0
  macAddress: "52:54:00:AA:BB:CC"
  serial: "worker-standard-001"
```

### Example

```yaml
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineClass
metadata:
  name: gpu
  labels:
    team: ml
spec:
  vcpu: 8
  memoryGiB: 32
  diskGiB: 100
  networkBridge: vmbr0
  storagePool: local-lvm
  proxmoxPool: k8s-gpu-workers
  bootTimeout: "5m"
  gpu:
    vendor: nvidia
    model: "RTX-4090"
    pciAddress: "0000:01:00.0"
  labels:
    accelerator: nvidia-gpu
  taints:
  - key: nvidia.com/gpu
    value: "true"
    effect: NoSchedule
```

### Built-in Classes

The project ships with four default machine classes:

| Name | vCPU | RAM | Disk | Use Case |
|------|------|-----|------|----------|
| `tiny` | 2 | 4 GiB | 30 GiB | Sidecars, lightweight workloads |
| `standard` | 4 | 8 GiB | 50 GiB | General workloads (default) |
| `gpu` | 8 | 32 GiB | 100 GiB | GPU passthrough workloads |
| `storage` | 4 | 16 GiB | 200 GiB | Persistent volume workloads |

---

## MachineTemplate

Represents a specific VM instance created from a MachineClass. Managed automatically by the autoscaler; do not edit directly.

### API

```yaml
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineTemplate
metadata:
  name: <string>           # Auto-generated (e.g., "standard-worker-3")
  generateName: <string>   # Prefix for auto-generated name
  labels:
    autoscaler.talos.dev/machine-class: <string>  # Required. MachineClass name
    autoscaler.talos.dev/deployment: <string>      # Required. MachineDeployment name
spec:
  machineClassName: <string>    # Required. Reference to MachineClass
  deploymentName: <string>      # Required. Reference to MachineDeployment
  index: <integer>              # Required. Unique index within deployment
  bootTimeout: <duration>       # Required. Max time to wait for node join
  config:                       # Optional. Machine-specific overrides
    networkIP: <string>         # Static IP (if needed)
    labels: <map[string]string> # Extra labels for this node
status:
  phase: <string>              # "Pending", "Provisioning", "Joining", "Ready", "Deleting", "Failed"
  nodeRef: <string>            # Kubernetes node name (set when joined)
  proxmoxVMID: <integer>       # Proxmox VM ID
  provisionStartTime: <time>   # When provisioning started
  readyTime: <time>            # When node became Ready
  conditions:                   # Standard Kubernetes conditions
  - type: <string>             # "Provisioned", "Joined", "Ready"
    status: <string>           # "True", "False", "Unknown"
    lastTransitionTime: <time>
    reason: <string>
    message: <string>
  error: <string>              # Error message if phase is "Failed"
  retries: <integer>           # Number of retry attempts
```

### Phases

```
Pending → Provisioning → Joining → Ready → Deleting → (removed)
                         ↓
                      Failed → (retry) → Provisioning
```

| Phase | Description |
|-------|-------------|
| `Pending` | Waiting for VM creation to start |
| `Provisioning` | VM being created on Proxmox |
| `Joining` | VM running, PXE-booting, Talos joining cluster |
| `Ready` | Node is Ready and accepting workloads |
| `Deleting` | Node being drained and destroyed |
| `Failed` | Provisioning or joining failed |

### Example

```yaml
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineTemplate
metadata:
  name: standard-worker-3
  labels:
    autoscaler.talos.dev/machine-class: standard
    autoscaler.talos.dev/deployment: standard-workers
spec:
  machineClassName: standard
  deploymentName: standard-workers
  index: 3
  bootTimeout: "5m"
status:
  phase: Ready
  nodeRef: worker-standard-3
  proxmoxVMID: 203
  provisionStartTime: "2025-01-15T10:30:00Z"
  readyTime: "2025-01-15T10:32:15Z"
  conditions:
  - type: Provisioned
    status: "True"
    lastTransitionTime: "2025-01-15T10:30:45Z"
    reason: VMProvisioned
    message: "VM 203 created on pve-2"
  - type: Joined
    status: "True"
    lastTransitionTime: "2025-01-15T10:31:30Z"
    reason: NodeJoined
    message: "Node worker-standard-3 joined cluster"
  - type: Ready
    status: "True"
    lastTransitionTime: "2025-01-15T10:32:15Z"
    reason: NodeReady
    message: "Node worker-standard-3 is Ready"
```

---

## MachineDeployment

Manages a pool of machines for a specific MachineClass. Controls scaling and lifecycle.

### API

```yaml
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineDeployment
metadata:
  name: <string>                    # Required. Deployment name
  namespace: <string>               # Required. Namespace
spec:
  replicas: <integer>               # Desired number of machines
  machineClassName: <string>        # Required. Reference to MachineClass
  minReplicas: <integer>            # Required. Minimum machines (0-100)
  maxReplicas: <integer>            # Required. Maximum machines (1-100)
  paused: <boolean>                 # Optional. Pause scaling (default: false)
  template:                         # Required. Machine template spec
    metadata:
      labels: <map[string]string>   # Labels applied to MachineTemplates
      annotations: <map[string]string>  # Annotations applied to MachineTemplates
    spec:
      bootTimeout: <duration>       # Optional. Override MachineClass default
      config:                       # Optional. Machine-specific config
        networkIP: <string>
        labels: <map[string]string]
  strategy:                         # Optional. Scaling strategy
    type: <string>                  # "RollingUpdate" or "Recreate" (default: "RollingUpdate")
    maxSurge: <string>              # Max extra machines during update (default: "25%")
    maxUnavailable: <string>        # Max unavailable during update (default: "0")
  scaleDown:                        # Optional. Scale-down behavior
    delay: <duration>               # Cooldown before scale-down (default: "10m")
    utilizationThreshold: <number>  # CPU utilization threshold (default: 0.2)
    drainTimeout: <duration>        # Max time to drain a node (default: "30s")
status:
  replicas: <integer>              # Current number of machines
  readyReplicas: <integer>         # Number of Ready machines
  availableReplicas: <integer>     # Number of Available machines
  unavailableReplicas: <integer>   # Number of Unavailable machines
  updatedReplicas: <integer>       # Number of up-to-date machines
  conditions:                       # Standard Kubernetes conditions
  - type: <string>                 # "Scaling", "Available", "Progressing"
    status: <string>
    lastTransitionTime: <time>
    reason: <string>
    message: <string>
  scaleStatus:                     # Current scaling state
    currentReplicas: <integer>
    desiredReplicas: <integer>
    lastScaleTime: <time>
    scaleDirection: <string>       # "up", "down", "stable"
```

### Scaling Behavior

The autoscaler follows this logic:

**Scale-up (every 30s reconcile):**
```
1. Aggregate CPU and memory requests of all unschedulable pods
2. Calculate how many workers of the MachineClass capacity are needed
3. Create that many VMs (up to maxReplicas)
```

**Scale-down (descheduler-driven):**
```
1. Watch for nodes labeled descheduler.kubernetes.io/node-probable-eviction
2. Cordon the labeled node
3. Drain the node (respect PDBs)
4. Destroy the VM via Proxmox API
```

The autoscaler does not perform its own utilization-based scale-down — that is the descheduler's job. This separation keeps the autoscaler focused on capacity (can we fit the pods?) while the descheduler handles efficiency (are we wasting resources?).

### Update Strategy

**RollingUpdate** (default):
```yaml
strategy:
  type: RollingUpdate
  maxSurge: "25%"     # Allow 25% extra machines during update
  maxUnavailable: "0" # Don't reduce available machines
```

**Recreate**:
```yaml
strategy:
  type: Recreate      # Destroy all, then recreate (use for major version changes)
```

### Example

```yaml
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineDeployment
metadata:
  name: standard-workers
  namespace: default
spec:
  replicas: 3
  machineClassName: standard
  minReplicas: 1
  maxReplicas: 20
  template:
    metadata:
      labels:
        role: worker
        class: standard
    spec:
      bootTimeout: "5m"
  strategy:
    type: RollingUpdate
    maxSurge: "25%"
    maxUnavailable: "0"
  scaleDown:
    delay: "10m"
    utilizationThreshold: 0.2
    drainTimeout: "30s"
status:
  replicas: 3
  readyReplicas: 3
  availableReplicas: 3
  unavailableReplicas: 0
  updatedReplicas: 3
  conditions:
  - type: Available
    status: "True"
    lastTransitionTime: "2025-01-15T10:35:00Z"
    reason: DeploymentAvailable
    message: "All 3 machines are available"
  scaleStatus:
    currentReplicas: 3
    desiredReplicas: 3
    lastScaleTime: "2025-01-15T10:30:00Z"
    scaleDirection: stable
```

---

## Global Defaults

These are set in the autoscaler deployment, not in CRDs:

| Variable | Default | Description |
|----------|---------|-------------|
| `MIN_WORKERS` | `1` | Global minimum workers across all deployments |
| `MAX_WORKERS` | `20` | Global maximum workers across all deployments |
| `SCALE_DOWN_DELAY` | `10m` | Cooldown before any scale-down |
| `DRAIN_TIMEOUT` | `30s` | Max time to drain a node before force-delete |
| `RECONCILE_INTERVAL` | `30s` | How often the controller checks for changes (accumulates pending pods in this window) |
| `PROVISION_TIMEOUT` | `5m` | Max time to provision and join a node |
| `MAX_RETRIES` | `3` | Max retries for failed provisioning |
| `PROXMOX_VMID_START` | `200` | Starting VMID for autoscaled VMs |
| `NODE_PREFIX` | `worker` | Prefix for node names |

## Labels and Annotations

### Standard Labels

Applied to `MachineTemplate` resources:

| Label | Description |
|-------|-------------|
| `autoscaler.talos.dev/machine-class` | MachineClass name |
| `autoscaler.talos.dev/deployment` | MachineDeployment name |
| `autoscaler.talos.dev/index` | Index within the deployment |
| `autoscaler.talos.dev/proxmox-vmid` | Proxmox VM ID |

Applied to Kubernetes nodes:

| Label | Description |
|-------|-------------|
| `autoscaler.talos.dev/machine-class` | MachineClass name |
| `autoscaler.talos.dev/deployment` | MachineDeployment name |
| `descheduler.kubernetes.io/node-probable-eviction` | Set by external descheduler; triggers autoscaler to cordon, drain, and delete the node |
| Custom labels from `MachineClass.spec.labels` | User-defined labels |

### Standard Annotations

| Annotation | Description |
|------------|-------------|
| `autoscaler.talos.dev/provision-time` | Time when provisioning started |
| `autoscaler.talos.dev/join-time` | Time when node joined cluster |
| `autoscaler.talos.dev/proxmox-node` | Proxmox node where VM is placed |

## Status Conditions

### MachineTemplate Conditions

| Type | True Reason | False Reason |
|------|-------------|--------------|
| `Provisioned` | `VMProvisioned` | `VMCreationPending` / `VMCreationFailed` |
| `Joined` | `NodeJoined` | `NodeJoinPending` / `NodeJoinTimeout` |
| `Ready` | `NodeReady` | `NodeNotReady` |

### MachineDeployment Conditions

| Type | True Reason | False Reason |
|------|-------------|--------------|
| `Scaling` | `ScaleUp` / `ScaleDown` | `Stable` |
| `Available` | `DeploymentAvailable` | `DeploymentUnavailable` |
| `Progressing` | `DeploymentProgressing` | `DeploymentStalled` |

## Validation

The autoscaler validates CRD fields at admission time:

| Field | Rule | Error |
|-------|------|-------|
| `spec.vcpu` | 1 ≤ vcpu ≤ 64 | Invalid CPU count |
| `spec.memoryGiB` | 1 ≤ memoryGiB ≤ 512 | Invalid memory size |
| `spec.diskGiB` | 10 ≤ diskGiB ≤ 4096 | Invalid disk size |
| `spec.minReplicas` | 0 ≤ min ≤ maxReplicas | Min exceeds max |
| `spec.maxReplicas` | minReplicas ≤ max ≤ 100 | Max out of range |
| `spec.replicas` | minReplicas ≤ replicas ≤ maxReplicas | Replicas out of range |
| `spec.machineClassName` | Must reference existing MachineClass | Unknown machine class |
| `spec.macAddress` | Valid MAC format if set (XX:XX:XX:XX:XX:XX) | Invalid MAC address |

## RBAC Permissions

The autoscaler service account requires:

```yaml
# ClusterRole for CRD management
- apiGroups: ["autoscaler.talos.dev"]
  resources: ["machinetemplates", "machinedeployments"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["autoscaler.talos.dev"]
  resources: ["machineclasses"]
  verbs: ["get", "list", "watch"]

# Core resources
- apiGroups: [""]
  resources: ["nodes", "pods", "events"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: [""]
  resources: ["nodes/status"]
  verbs: ["patch"]  # for cordoning nodes
- apiGroups: [""]
  resources: ["pods/eviction"]
  verbs: ["create"]
- apiGroups: ["apps"]
  resources: ["deployments", "replicasets"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["policy"]
  resources: ["poddisruptionbudgets"]
  verbs: ["get", "list", "watch"]

# KEDA integration
- apiGroups: ["keda.sh"]
  resources: ["scaledobjects"]
  verbs: ["get", "list", "watch"]

# Secrets for configuration (mounted as files)
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
```

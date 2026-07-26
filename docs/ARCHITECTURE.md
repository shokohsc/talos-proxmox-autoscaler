# Architecture

Detailed architecture of the Talos Kubernetes Node Autoscaler for Proxmox.

## System Overview

The autoscaler operates as a closed control loop that **only manages worker nodes**. Control plane nodes (3 permanent) are provisioned and managed outside this system. Configuration is read from a Kubernetes ConfigMap (`autoscaler-config` in `autoscaler-system` namespace).

```
                    ┌─────────────────────────────────┐
                    │         Kubernetes API           │
                    │  (watch unschedulable pods)      │
                    └──────────┬──────────────────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │   Go Autoscaler      │
                    │                      │
                    │  ┌────────────────┐  │
                    │  │  30s Timer     │  │
                    │  │  Loop          │  │
                    │  └───────┬────────┘  │
                    │          │           │
                    │  ┌───────▼────────┐  │
                    │  │  Scale Decision │  │
                    │  │   Engine        │  │
                    │  └───────┬────────┘  │
                    │          │           │
                    │  ┌───────▼────────┐  │
                    │  │  ConfigMap     │  │
                    │  │  Reader        │  │
                    │  └────────────────┘  │
                    └──────────┼───────────┘
                               │
                    ┌──────────▼───────────┐
                    │  Proxmox REST API    │
                    │  (direct HTTP calls) │
                    └──────────┬───────────┘
                               │
                    ┌──────────▼───────────┐
                    │    Proxmox API       │
                    │  (VM lifecycle)      │
                    └──────────────────────┘
```

## Components

### 1. Go Autoscaler Controller

The core control loop runs inside the Kubernetes cluster as a standard Go binary (no controller-runtime manager). It reads all configuration from a ConfigMap and uses `rest.InClusterConfig()` to talk to the Kubernetes API.

**Timer loop (every 30 seconds):**
1. Read config from `autoscaler-config` ConfigMap
2. List pods with `status.conditions` containing `PodScheduled=False, reason=Unschedulable`
3. Aggregate CPU and memory requests of all unschedulable pods
4. Calculate how many workers are needed and their optimal size (within min/max CPU/memory ranges)
5. Create that many VMs (up to `max_workers`), if current count is insufficient
6. Wait for nodes to register in the Kubernetes API

**Dynamic VM sizing:** The controller sums pending pod CPU/memory requests and computes optimal VM sizes within configurable min/max bounds. Instead of a single fixed VM type, it creates VMs sized to fit the actual workload.

**Key files:**
```
autoscaler/main.go              # Entry point, direct K8s client setup
autoscaler/pkg/autoscaler/controller.go  # Config struct, Reconciler struct
```

### 2. Proxmox REST API Client

Direct HTTP client that manages VM lifecycle on Proxmox. Supports two authentication methods, auto-detected from mounted secret fields:

- **API Token**: `Authorization: PVEAPIToken=USER@REALM!TOKENID=SECRET` header
- **Username/Password**: Login via `/access/ticket` endpoint, caches ticket and CSRF token

```
┌─────────────────────────────────────────────────────────┐
│           Proxmox REST API Client                       │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  Auth: API Token or Username/Password (auto-detected)   │
│  Base: https://<proxmox_api_url>/api2/json              │
│                                                         │
│  Functions:                                             │
│  ├── CreateVM (clone or from scratch)                   │
│  ├── DeleteVM                                           │
│  ├── StopVM                                             │
│  ├── GetVMStatus                                        │
│  ├── FindVMByName                                       │
│  ├── ListNodes (cluster node discovery)                 │
│  └── GetNode (configured or auto-discovered)            │
│                                                         │
│  VM Naming: {cluster}-worker-{index}                    │
│  VMID Scheme: BASE_VMID + index                         │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

**VM Provisioning Flow:**
1. Generate VM config from ConfigMap values (CPU, RAM, disk, network)
2. Call Proxmox API to clone from template or create from scratch
3. Start the VM via API call
4. VM PXE-boots Talos Linux, fetching config from remote config server
5. Talos joins cluster via bootstrap token

**VM Destruction Flow:**
1. Cordon and drain the node (Kubernetes)
2. Call Proxmox API to stop the VM
3. Call Proxmox API to delete the VM
4. Node disappears from Kubernetes

### 4. PXE Boot Flow

Workers do **not** use cloud-init or VM templates. Instead:

1. Controller calls Proxmox API to create a VM with boot order `scsi0;net0`
2. On first boot, scsi0 has no OS, so the BIOS falls through to PXE on net0
3. PXE/TFTP serves the Talos kernel and initramfs
4. The Talos installer fetches machine config from a **remote config server** matching the VM's MAC address prefix
5. Talos installs itself to scsi0, then reboots
6. On subsequent boots the VM boots directly from scsi0 — no PXE involved
7. Talos joins the cluster via bootstrap token and becomes Ready

```
First boot:                         Subsequent boots:
┌─────────┐    net0    ┌──────────┐ ┌─────────┐   scsi0   ┌─────────┐
│ New VM  │───────────▶│ PXE/TFTP │ │ Running │──────────▶│  Talos  │
│ (empty  │            │ Server   │ │   VM    │           │ on disk │
│  scsi0) │            └──────────┘ └─────────┘           └─────────┘
└─────────┘
```

### 5. Proxmox Integration

The autoscaler communicates directly with the Proxmox VE REST API using a **dedicated, least-privilege API token**. The client uses `net/http` (Go stdlib) with no external dependencies.

```
Proxmox Cluster (3 nodes)
├── Node: pve-1
│   ├── VM: cp-0 (control plane, permanent, NOT managed by autoscaler)
│   ├── VM: cp-1 (control plane, permanent, NOT managed by autoscaler)
│   └── VM: cp-2 (control plane, permanent, NOT managed by autoscaler)
├── Node: pve-2
│   ├── VM: worker-0 (autoscaled)
│   └── VM: worker-3 (autoscaled)
└── Node: pve-3
    ├── VM: worker-1 (autoscaled)
    └── VM: worker-2 (autoscaled)
```

**VM Placement Strategy:**
- Control planes: pinned to specific Proxmox nodes (managed outside autoscaler)
- Workers: placed by Proxmox HA scheduler (least-loaded)

## Go Types

```go
// AuthType distinguishes between password and token authentication
type AuthType int

const (
    AuthPassword AuthType = iota
    AuthToken
)

// Config holds all configuration read from the autoscaler-config ConfigMap
type Config struct {
    ClusterName   string
    MinWorkers    int32
    MaxWorkers    int32
    MinCPU        int       // Minimum CPUs per worker VM
    MaxCPU        int       // Maximum CPUs per worker VM
    MinMemoryGiB  int       // Minimum RAM per worker VM (GiB)
    MaxMemoryGiB  int       // Maximum RAM per worker VM (GiB)
    DiskGiB       int32
    StoragePool   string
    NetworkBridge string
    MACAddress    string
    Serial        string
    Tags          string          // Tags applied to provisioned VMs
    VLANID        int             // VLAN tag for primary network interface
    PCIDevices   []proxmox.PCIDevice  // PCI passthrough devices
}

// VMSize represents the computed size for a batch of new VMs
type VMSize struct {
    CPU       int
    MemoryGiB int
}

// PCIDevice represents a PCI passthrough device configuration
type PCIDevice struct {
    ID   string `json:"id"`
    PCIe bool   `json:"pcie"`
    GPU  bool   `json:"gpu"`
}

// Reconciler runs the 30s timer loop, reads Config, and manages VMs via Proxmox API
type Reconciler struct {
    Proxmox    *proxmox.Client
    KubeClient kubernetes.Interface
    BaseVMID   int
}
```

## Data Flow

### Scale Up

```
1. Pod created, no node can fit it
         │
         ▼
2. Go controller 30s timer fires
         │
         ▼
3. Controller aggregates CPU/memory requests of all unschedulable pods
         │
         ▼
4. Controller reads VM specs from autoscaler-config ConfigMap
         │
         ▼
5. Controller calculates how many workers are needed and their optimal size (within min/max bounds)
         │
         ▼
6. Controller calls Proxmox API to create new VM(s)
         │
         ▼
7. Proxmox API call → Proxmox creates VM(s)
         │
         ▼
8. VM PXE-boots (scsi0 empty → net0 fallback), fetches Talos
   config from config server
         │
         ▼
9. Talos installs to scsi0, reboots, boots from disk
         │
         ▼
10. Talos joins cluster using bootstrap token
         │
         ▼
11. Node becomes Ready, kubelet reports allocatable resources
         │
         ▼
12. Pending pods schedule onto new node(s)
```

### Scale Down (Descheduler-Driven)

```
1. External descheduler labels node with
   descheduler.kubernetes.io/node-probable-eviction
         │
         ▼
2. Autoscaler controller detects the label (30s reconcile loop)
         │
         ▼
3. Controller cordons the node
         │
         ▼
4. Controller drains the node (pods evicted per PDB rules)
         │
         ▼
5. Controller calls Proxmox API to stop and delete the VM
         │
         ▼
6. Proxmox API call → Proxmox deletes VM
         │
         ▼
7. Node removed from Kubernetes
```

## Networking

```
Proxmox Network Layout
───────────────────────
vmbr0 (management)
├── pve-1: 10.0.1.11
├── pve-2: 10.0.1.12
└── pve-3: 10.0.1.13

vmbr1 (kubernetes - bridge)
├── cp-0: 10.0.2.10 (static)
├── cp-1: 10.0.2.11 (static)
├── cp-2: 10.0.2.12 (static)
├── worker-0: 10.0.2.20 (DHCP from metallb or static pool)
├── worker-1: 10.0.2.21
└── ...

Pod CIDR: 10.244.0.0/16
Service CIDR: 10.96.0.0/12
```

## High Availability

- **Control planes**: 3 permanent nodes, managed outside the autoscaler
- **Workers**: scaled dynamically based on unschedulable pod resource requests, always maintain at least `min_workers` nodes
- **Node drain**: uses standard Kubernetes grace period (30s default)
- **Proxmox HA**: VMs marked with Proxmox HA group for automatic restart on node failure

## Failure Modes

| Failure | Impact | Recovery |
|---------|--------|----------|
| Proxmox API unreachable | Cannot provision/destroy VMs | Controller retries with exponential backoff |
| ConfigMap missing or invalid | Cannot read VM specs or scaling params | Controller logs error and waits for ConfigMap update |
| Descheduler not running | No scale-down occurs | Nodes stay alive; deploy/fix descheduler |
| Node fails to join cluster | VM exists but unused | Controller detects and destroys after 5m timeout |
| Drain timeout exceeded | Node stays cordoned | Force-delete after configurable timeout |
| Bootstrap token expired | New nodes can't join | Controller rotates token automatically |
| PXE/config server unreachable | VMs can't boot Talos | VMs timeout and are destroyed by controller |

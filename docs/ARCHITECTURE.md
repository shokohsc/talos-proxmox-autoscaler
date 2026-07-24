# Architecture

Detailed architecture of the Talos Kubernetes Node Autoscaler for Proxmox.

## System Overview

The autoscaler operates as a closed control loop that **only manages worker nodes**. Control plane nodes (3 permanent) are provisioned and managed outside this system.

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
                    │  │ Reconcile Loop │  │
                    │  │   (30s tick)   │  │
                    │  └───────┬────────┘  │
                    │          │           │
                    │  ┌───────▼────────┐  │
                    │  │  Scale Decision │  │
                    │  │   Engine        │  │
                    │  └───────┬────────┘  │
                    └──────────┼───────────┘
                               │
                    ┌──────────▼───────────┐
                    │   OpenTofu Exec      │
                    │  (apply/destroy)     │
                    └──────────┬───────────┘
                               │
                    ┌──────────▼───────────┐
                    │    Proxmox API       │
                    │  (VM lifecycle)      │
                    └──────────────────────┘
```

## Components

### 1. Go Autoscaler Controller

The core control loop runs inside the Kubernetes cluster.

**Reconciliation cycle (every 30 seconds):**
1. List pods with `status.conditions` containing `PodScheduled=False, reason=Unschedulable`
2. Aggregate CPU and memory requests of all unschedulable pods
3. Calculate how many workers of the MachineClass capacity are needed to fit them
4. Create that many VMs (up to `maxReplicas`), if current count is insufficient
5. Wait for nodes to register in the Kubernetes API
6. Update MachineDeployment status

**Resource-aware sizing:** The controller does not just count pods — it sums their CPU and memory requests and divides by the MachineClass capacity (vCPU, memoryGiB) to determine exactly how many new workers are needed.

**Key files:**
```
autoscaler/main.go
autoscaler/pkg/autoscaler/controller.go
```

### 2. KEDA Integration

KEDA provides external metrics that drive scaling decisions. The ScaledObject uses an external scaler trigger that communicates with the autoscaler's gRPC interface:

| ScaledObject | Metric | Purpose |
|--------------|--------|---------|
| `talos-proxmox-autoscaler` | External scaler | Scale based on unschedulable pods and node utilization |

**Scale-up trigger:**
```
unschedulable_pod_count > 0  →  scale up by ceil(count / pods_per_node)
```

**Scale-down trigger (descheduler-driven):**
```
node labeled descheduler.kubernetes.io/node-probable-eviction
  → cordon, drain, and delete the node
```

The autoscaler does not perform its own utilization-based scale-down. Instead, an external descheduler project analyzes node utilization and labels unneeded nodes with `descheduler.kubernetes.io/node-probable-eviction`. The autoscaler watches for this label and handles the actual node removal (cordon, drain, VM destroy).

### 3. OpenTofu Provider

Executes OpenTofu as a subprocess to manage VM lifecycle on Proxmox.

```
┌─────────────────────────────────────────────┐
│              OpenTofu State                 │
│         (encrypted, stored on disk)         │
├─────────────────────────────────────────────┤
│                                             │
│  terraform.tfstate                         │
│  ├── proxmox_vm.talos-worker-0             │
│  ├── proxmox_vm.talos-worker-1             │
│  └── ...                                   │
│                                             │
└─────────────────────────────────────────────┘
```

**VM Provisioning Flow:**
1. Generate OpenTofu variables from machine class spec
2. Run `tofu apply -auto-approve`
3. OpenTofu creates VM on Proxmox via `proxmox` provider
4. VM PXE-boots Talos Linux, fetching config from remote config server
5. Talos joins cluster via bootstrap token

**VM Destruction Flow:**
1. Cordon and drain the node (Kubernetes)
2. Run `tofu destroy -target=<resource>`
3. OpenTofu deletes VM from Proxmox
4. Node disappears from Kubernetes

### 4. PXE Boot Flow

Workers do **not** use cloud-init or VM templates. Instead:

1. OpenTofu creates a VM with boot order `scsi0;net0`
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

The OpenTofu Proxmox provider communicates with the Proxmox VE REST API using a **dedicated, least-privilege API token**.

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

## CRD Types

```go
// MachineClass defines a class of worker nodes
type MachineClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   MachineClassSpec   `json:"spec"`
    Status MachineClassStatus `json:"status,omitempty"`
}

// MachineTemplate tracks a specific VM instance
type MachineTemplate struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   MachineTemplateSpec   `json:"spec"`
    Status MachineTemplateStatus `json:"status,omitempty"`
}

// MachineDeployment manages a pool of machines for a class
type MachineDeployment struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   MachineDeploymentSpec   `json:"spec"`
    Status MachineDeploymentStatus `json:"status,omitempty"`
}
```

## Data Flow

### Scale Up

```
1. Pod created, no node can fit it
         │
         ▼
2. KEDA detects unschedulable pod count > 0
         │
         ▼
3. KEDA signals autoscaler via external scaler
         │
         ▼
4. Go controller reconciles (every 30s), aggregates CPU/memory
   requests of all unschedulable pods
         │
         ▼
5. Controller calculates how many workers of the MachineClass
   capacity are needed (resource-aware sizing)
         │
         ▼
6. Controller runs tofu apply (new VM resources)
         │
         ▼
7. OpenTofu API call → Proxmox creates VM(s)
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
5. Controller runs tofu destroy (target VM)
         │
         ▼
6. OpenTofu API call → Proxmox deletes VM
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
- **Workers**: scaled dynamically based on unschedulable pod resource requests, always maintain at least `minWorkers` nodes
- **Node drain**: uses standard Kubernetes grace period (30s default)
- **Proxmox HA**: VMs marked with Proxmox HA group for automatic restart on node failure
- **OpenTofu state**: stored encrypted on disk, backed up to S3

## Failure Modes

| Failure | Impact | Recovery |
|---------|--------|----------|
| Proxmox API unreachable | Cannot provision/destroy VMs | Controller retries with exponential backoff |
| OpenTofu state corruption | Cannot manage VMs | Restore from S3 backup |
| KEDA metrics unavailable | Scale-up pauses | Falls back to reconciliation-only mode |
| Descheduler not running | No scale-down occurs | Nodes stay alive; deploy/fix descheduler |
| Node fails to join cluster | VM exists but unused | Controller detects and destroys after 5m timeout |
| Drain timeout exceeded | Node stays cordoned | Force-delete after configurable timeout |
| Bootstrap token expired | New nodes can't join | Controller rotates token automatically |
| PXE/config server unreachable | VMs can't boot Talos | VMs timeout and are destroyed by controller |

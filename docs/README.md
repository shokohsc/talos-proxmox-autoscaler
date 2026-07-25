# Talos Kubernetes Node Autoscaler for Proxmox

Autoscale Talos Linux Kubernetes **worker** nodes on a Proxmox VE cluster using KEDA and a custom Go controller.

## What This Does

Watches your Kubernetes cluster for pods that can't be scheduled, provisions new Talos Linux VMs on Proxmox to handle them, and tears them down when demand drops. Supports four machine classes:

| Class | vCPU | RAM | Use Case |
|-------|------|-----|----------|
| `tiny` | 2 | 4 GB | Lightweight workloads, sidecars |
| `standard` | 4 | 8 GB | General workloads (default) |
| `gpu` | 8 | 32 GB | GPU passthrough workloads |
| `storage` | 4 | 16 GB | Persistent volume workloads |

Scale range: **1–20 workers** (configurable). Three control plane nodes run permanently and are **not managed by the autoscaler**.

## Architecture (TL;DR)

```
┌──────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                     │
│                                                          │
│  ┌──────────┐    ┌──────────────┐    ┌───────────────┐  │
│  │   KEDA   │───▶│  Go Node     │───▶│  Proxmox      │  │
│  │ Scaler   │    │  Autoscaler  │    │  REST API     │  │
│  └──────────┘    └──────────────┘    └───────┬───────┘  │
│       ▲                                       │          │
│       │ watches                              │ provisions│
│       │ unschedulable                        ▼          │
│       │ pods                            ┌───────────┐   │
│  ┌────┴─────┐                           │  Proxmox  │   │
│  │  K8s API │                           │   Cluster │   │
│  └──────────┘                           └───────────┘   │
└──────────────────────────────────────────────────────────┘
```

**Boot flow:** VMs are configured with boot order `scsi0;net0`. On first boot, scsi0 is empty so the VM falls through to PXE on net0. The PXE/TFTP server serves the Talos kernel, which fetches its machine config from a remote config server matched by the VM's MAC address prefix. Talos installs itself to scsi0. On subsequent boots the VM boots directly from scsi0 with Talos already installed. There are no VM templates or cloud-init — PXE handles first boot, then the disk takes over.

**Resource-aware scaling:** The controller runs a reconciliation loop every **30 seconds**. It aggregates CPU and memory requests of all unschedulable pods, calculates how many workers of the MachineClass capacity are needed, and creates that many VMs (up to `maxReplicas`).

**Descheduler integration:** The controller watches for nodes labeled `descheduler.kubernetes.io/node-probable-eviction` (set by an external descheduler project that marks unneeded nodes). When detected, it cordons, drains, and deletes those nodes — handling the actual removal while the descheduler handles identification.

## Quick Start

### Prerequisites

- Proxmox VE 8.x cluster (3 nodes)
- Talos Linux ISO uploaded to Proxmox
- Kubernetes cluster with 3 permanent control planes
- Go >= 1.22
- kubectl configured for target cluster
- PXE boot infrastructure with a Talos config server

### 1. Clone and Build

```bash
git clone https://github.com/your-org/talos-proxmox-autoscaler.git
cd talos-proxmox-autoscaler
make build
```

### 2. Configure

```bash
cp config/example.env config/.env
# Edit config/.env with your Proxmox credentials and cluster settings
```

### 3. Deploy

```bash
make deploy
```

This installs:
- CRDs for machine classes
- The Go autoscaler controller
- KEDA scalers for unschedulable pod detection

### 4. Verify

```bash
kubectl get machineclasses -A
kubectl get pods -n kube-system -l app=talos-node-autoscaler
```

## Project Structure

```
.
├── autoscaler/              # Go autoscaler entrypoint
│   └── pkg/
│       ├── autoscaler/      # Core autoscaling logic
│       └── proxmox/         # Proxmox REST API client
├── kubernetes/
│   ├── crds/                # CRD manifests
│   ├── rbac/                # RBAC manifests
│   ├── keda/                # KEDA ScaledObject manifests
│   └── deployment.yaml      # Controller deployment
├── examples/
│   ├── machine-classes/     # Example MachineClass CRs
│   └── 3-node-cluster/      # Full cluster example
├── docs/
│   ├── README.md            # This file
│   ├── ARCHITECTURE.md      # Detailed architecture
│   ├── DEPLOYMENT.md        # Deployment guide
│   ├── TROUBLESHOOTING.md   # Common issues
│   └── CRD_REFERENCE.md     # CRD API reference
├── Makefile
├── Dockerfile
└── .github/workflows/       # CI/CD pipelines
```

## Machine Classes (CRDs)

Define custom resources to declare worker node types:

```yaml
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineClass
metadata:
  name: standard
spec:
  vcpu: 4
  memoryGiB: 8
  diskGiB: 50
  networkBridge: vmbr0
  storagePool: local-lvm
  proxmoxPool: k8s-workers
```

See [CRD Reference](CRD_REFERENCE.md) for the full specification.

## Security

This project handles infrastructure provisioning secrets. Key security measures:

- **Dedicated Proxmox API user** — never use `root@pam` for the autoscaler
- **Secrets mounted as files**, not environment variables
- **Least-privilege RBAC** — the autoscaler service account has minimal permissions
- **Network policies** restrict autoscaler traffic to Proxmox API only

See [Deployment Guide](DEPLOYMENT.md#security-hardening) for details.

## Documentation

- [Architecture](ARCHITECTURE.md) - How it all fits together
- [Deployment Guide](DEPLOYMENT.md) - Step-by-step setup
- [Troubleshooting](TROUBLESHOOTING.md) - When things go wrong
- [CRD Reference](CRD_REFERENCE.md) - API types and fields

## License

Apache 2.0

# Talos Kubernetes Node Autoscaler for Proxmox

Autoscale Talos Linux Kubernetes **worker** nodes on a Proxmox VE cluster using a lightweight Go controller. All configuration lives in a Kubernetes ConfigMap — no CRDs, no controller-runtime manager, no KEDA.

## What This Does

Watches your Kubernetes cluster for pods that can't be scheduled, provisions new Talos Linux VMs on Proxmox to handle them, and tears them down when demand drops. Supports two worker types:

| Type | Prefix | Use Case |
|------|--------|----------|
| `worker-vm` | `worker-vm` | General workloads (CPU/memory based) |
| `worker-vm-gpu` | `worker-vm-gpu` | GPU passthrough workloads (Nvidia) |

Scale range: **1–20 workers per type** (configurable). Three control plane nodes run permanently and are **not managed by the autoscaler**.

## Architecture (TL;DR)

```
┌──────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                     │
│                                                          │
│  ┌──────────────┐    ┌───────────────┐                   │
│  │  Go Node     │───▶│  Proxmox      │                   │
│  │  Autoscaler  │    │  REST API     │                   │
│  └──────┬───────┘    └───────┬───────┘                   │
│         │                     │ provisions                │
│         │ watches             ▼                           │
│         │ unschedulable  ┌───────────┐                   │
│  ┌──────┴─────┐          │  Proxmox  │                   │
│  │  K8s API   │          │  Cluster  │                   │
│  └────────────┘          └───────────┘                   │
│                                                          │
│  ┌────────────┐                                          │
│  │ ConfigMap  │ autoscaler-config (VM specs, scaling)    │
│  └────────────┘                                          │
└──────────────────────────────────────────────────────────┘
```

**Boot flow:** VMs are configured with boot order `scsi0;net0`. On first boot, scsi0 is empty so the VM falls through to PXE on net0. The PXE/TFTP server serves the Talos kernel, which fetches its machine config from a remote config server matched by the VM's MAC address prefix. Talos installs itself to scsi0. On subsequent boots the VM boots directly from scsi0 with Talos already installed. There are no VM templates or cloud-init — PXE handles first boot, then the disk takes over.

**Resource-aware scaling:** The controller runs a timer loop every **30 seconds**. It aggregates CPU and memory requests of all unschedulable pods, calculates how many workers are needed based on the VM specs in the ConfigMap, and creates that many VMs (up to `max_workers`).

**Descheduler integration:** The controller watches for nodes labeled `descheduler.kubernetes.io/node-probable-eviction` (set by an external descheduler project that marks unneeded nodes). When detected, it cordons, drains, and deletes those nodes — handling the actual removal while the descheduler handles identification.

## Quick Start

### Prerequisites

- Proxmox VE 9.2.x cluster (3 nodes)
- Kubernetes cluster with 3 permanent control planes
- Go >= 1.26
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
- The Go autoscaler controller
- ConfigMap-based configuration (no CRDs needed)

### 4. Verify

```bash
kubectl get configmap autoscaler-config -n autoscaler-system -o yaml
kubectl get pods -n kube-system -l app=talos-node-autoscaler
```

## Project Structure

```
.
├── autoscaler/              # Go autoscaler entrypoint
│   └── pkg/
│       ├── autoscaler/      # Core controller (Config, Reconciler)
│       └── proxmox/         # Proxmox REST API client
├── kubernetes/
│   ├── rbac/                # RBAC manifests
│   ├── deployment.yaml      # Controller deployment
│   ├── configmap.yaml       # autoscaler-config ConfigMap
│   └── namespace.yaml
├── examples/
│   └── 3-node-cluster/      # Full cluster example
├── docs/
│   ├── README.md            # This file
│   ├── ARCHITECTURE.md      # Detailed architecture
│   ├── DEPLOYMENT.md        # Deployment guide
│   └── TROUBLESHOOTING.md   # Common issues
├── Makefile
├── Dockerfile
└── .github/workflows/       # CI/CD pipelines
```

## Autoscaler Configuration

### ConfigMap Fields

VM specs and scaling parameters are defined in a `ConfigMap` called `autoscaler-config` in the `autoscaler-system` namespace.

| Field | Description | Default |
|-------|-------------|---------|
| cluster_name | Name prefix for VMs | (required) |
| min_workers | Minimum worker count | 1 |
| max_workers | Maximum worker count | 10 |
| min_cpu | Minimum CPU per VM | 2 |
| max_cpu | Maximum CPU per VM | 8 |
| min_memory_gib | Minimum memory per VM | 4 |
| max_memory_gib | Maximum memory per VM | 16 |
| disk_gib | Disk size per VM | 50 |
| storage_pool | Proxmox storage pool | (required) |
| network_bridge | Network bridge | (required) |
| tags | Comma-separated tags appended to VMs (all VMs get default `talos`, GPU VMs also get `gpu`) | (optional) |
| worker_nodes | JSON array of regular worker configs | (required) |
| worker_gpu_nodes | JSON array of GPU worker configs with PCI devices | (optional) |
| base_vmid | Starting VMID for regular workers | 2000 |
| base_gpu_vmid | Starting VMID for GPU workers | 3000 |
| worker_prefix | Prefix for regular worker VM names | worker-vm |
| gpu_prefix | Prefix for GPU worker VM names | worker-vm-gpu |

### Secrets

Proxmox credentials are stored in a Kubernetes Secret. Two authentication methods are supported:

| Field | Description |
|-------|-------------|
| proxmox_username | Proxmox username (triggers password auth) |
| proxmox_password | Proxmox password (triggers password auth) |
| proxmox_api_token_id | API token ID (triggers token auth) |
| proxmox_api_token_secret | API token secret |

See [ARCHITECTURE.md](ARCHITECTURE.md) for details.

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

## License

Apache 2.0

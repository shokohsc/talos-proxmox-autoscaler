# Talos Kubernetes Node Autoscaler for Proxmox

A lightweight Go autoscaler that watches for unschedulable pods and provisions Talos Linux worker VMs on Proxmox VE via the Proxmox REST API. Configuration is driven entirely by a Kubernetes ConfigMap — no CRDs, no controller-runtime manager, no KEDA.

## How It Works

```
Unschedulable Pods → Controller (30s timer loop) → Proxmox API → VM
    → PXE Boot (scsi0 empty → net0) → Talos Config Server → Worker joins cluster
```

```
┌─────────────────────────────────────────────────────────────┐
│                     Kubernetes Cluster                       │
│                                                             │
│   Pending Pods ──▶ Go Controller ──▶ Proxmox API ──▶ Proxmox  │
│        ▲                │                                   │
│        │          ┌─────┴─────┐                             │
│        │          │  30s loop │                             │
│        │          │  aggregate│                             │
│        │          │  requests │                             │
│        │          └───────────┘                             │
│        │                                                    │
│   K8s API (watch unschedulable pods)                        │
└─────────────────────────────────────────────────────────────┘
```

## Key Features

- **Dynamic VM sizing** — autoscaler chooses CPU/RAM for each batch of pending pods between configurable min/max ranges (optimal fit)
- **Resource-aware scaling** — 30s timer loop aggregates pending pod CPU/memory requests, calculates exactly how many workers are needed
- **Dual worker types** — independent scaling for regular workers (`worker-vm`) and GPU workers (`worker-vm-gpu`) with per-GPU-type PCI passthrough configs
- **GPU scheduling** — pods requesting `nvidia.com/gpu` automatically trigger GPU worker provisioning
- **Descheduler integration** — watches for nodes labeled `descheduler.kubernetes.io/node-probable-eviction`, then cordons, drains, and destroys them
- **Workers-only** — 3 control planes run permanently, never managed by the autoscaler
- **PXE boot** — boot order `scsi0;net0`, first boot PXE-fetches Talos kernel, installs to disk, subsequent boots from scsi0
- **ConfigMap-based config** — all VM specs, cluster settings, and scaling parameters live in a single `autoscaler-config` ConfigMap (no CRDs)
- **Dual auth** — supports both Proxmox API token and username/password authentication (auto-detected from secret fields)
- **Node auto-discovery** — automatically selects an available cluster node if `proxmox_node` is not configured
- **Tag-based ownership** — every VM gets the configurable `autoscaler_tag` (default `talos`); GPU workers additionally get `gpu`; ConfigMap `tags` field appended. Scale decisions derive from the Proxmox VM list, not pod memory, so replicas are stateless
- **Stateless HA** — concurrent + idempotent: run multiple replicas (`replicas: 2`); all decode from the same ConfigMap and Proxmox truth, so racing duplicate creates/deletes log harmlessly and converge next tick
- **Hot config reload** — ConfigMap changes are detected (hash) and applied to future scale decisions within 30s
- **Optional MAC/SMBIOS** — explicit `mac_address` for PXE config lookup, `serial` for identification

## Project Structure

```
.
├── autoscaler/
│   ├── main.go                  # Entry point (direct K8s client, no controller-runtime)
│   ├── Dockerfile
│   ├── Makefile
│   └── pkg/
│       ├── autoscaler/          # Controller (Config, Reconciler)
│       │   └── controller.go
│       └── proxmox/             # Proxmox REST API client
├── kubernetes/
│   ├── rbac/                    # ServiceAccount, ClusterRole, Binding
│   ├── deployment.yaml          # Controller deployment
│   ├── configmap.yaml           # autoscaler-config ConfigMap
│   ├── namespace.yaml
│   └── networkpolicy.yaml
├── examples/
│   ├── 3-node-cluster/          # Full cluster example
│   └── load-test.sh
├── docs/
│   ├── README.md
│   ├── ARCHITECTURE.md
│   ├── DEPLOYMENT.md
│   └── TROUBLESHOOTING.md
├── .github/workflows/
│   ├── ci.yaml
│   ├── release.yaml
│   └── security.yaml
├── README.md
└── plan.md
```

## Quick Start

### Prerequisites

- Proxmox VE 9.2.x cluster (3 nodes)
- Kubernetes cluster with 3 permanent control planes
- Go >= 1.26
- PXE boot infrastructure with a Talos config server

### Install

```bash
# Create the namespace
kubectl apply -f kubernetes/namespace.yaml

# Apply the ConfigMap with VM specs, scaling, and cluster settings
kubectl apply -f kubernetes/configmap.yaml

# Apply RBAC
kubectl apply -f kubernetes/rbac/

# Create the secrets for Proxmox API access
# Option A: API token auth (recommended)
kubectl create secret generic autoscaler-secrets \
  --from-literal=proxmox_api_token_id="autoscaler@pve!autoscaler=YOUR_TOKEN_ID" \
  --from-literal=proxmox_api_token_secret="YOUR_TOKEN_SECRET" \
  -n autoscaler-system

# Option B: Username/password auth
kubectl create secret generic autoscaler-secrets \
  --from-literal=proxmox_username="root@pam" \
  --from-literal=proxmox_password="YOUR_PASSWORD" \
  -n autoscaler-system

# Deploy the autoscaler
kubectl apply -f kubernetes/deployment.yaml

# Apply the NetworkPolicy (restricts traffic to Proxmox API only)
kubectl apply -f kubernetes/networkpolicy.yaml
```

### ConfigMap Reference

The `autoscaler-config` ConfigMap drives all behavior. The key fields you'll need to customize:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: autoscaler-config
  namespace: autoscaler-system
data:
  cluster_name: "talos"          # Talos cluster name
  autoscaler_tag: "talos"          # Ownership tag for VMs this autoscaler manages
  min_workers: "1"               # Minimum worker nodes per type
  max_workers: "20"              # Maximum worker nodes per type
  min_cpu: "2"                   # Minimum CPUs per worker VM
  max_cpu: "8"                   # Maximum CPUs per worker VM
  min_memory_gib: "4"            # Minimum RAM per worker VM (GiB)
  max_memory_gib: "16"           # Maximum RAM per worker VM (GiB)
  disk_gib: "100"                # Disk per worker VM (GiB)
  storage_pool: "local-lvm"      # Proxmox storage for VM disks
  network_bridge: "vmbr0"        # Proxmox bridge for VM NICs
  proxmox_api_url: "https://pve.example.com:8006"  # Proxmox API endpoint
  base_vmid: "2000"              # Starting VMID for regular workers
  base_gpu_vmid: "3000"          # Starting VMID for GPU workers
  tags: "autoscaler,worker"      # Extra tags appended to provisioned VMs
  vlan_id: "0"                   # VLAN tag for primary interface (0 = no tag)
  cpu_type: "host"               # VM CPU type (default: "host")

  # Worker types
  worker_prefix: "worker-vm"       # Prefix for regular worker VM names
  gpu_prefix: "worker-vm-gpu"      # Prefix for GPU worker VM names
  worker_nodes: '[{"name":"worker-vm"}]'
  worker_gpu_nodes: '[{"type":"tesla-p4","nodes":["pve1","pve2"],"pci_devices":[{"id":"0000:01:00.0","pcie":true,"gpu":true}]},{"type":"tesla-p40","nodes":["pve3"],"pci_devices":[{"id":"0000:41:00.0","pcie":true,"gpu":true}]}]'
```

Optional fields: `mac_address`, `serial`, `cpu_type`, `proxmox_insecure`, `proxmox_node` (auto-discovers if omitted). See the full `kubernetes/configmap.yaml` for all keys.

### Ownership Tag Caveats

- `autoscaler_tag` applies to future VMs only — changing it orphans existing VMs (re-tag manually), and every autoscaler pointed at the same Proxmox must use a distinct tag.
- The Proxmox API token needs the `Audit` privilege on `/vm/` for `ListVMs` to see guest tags.

### Verify

```bash
kubectl get configmap autoscaler-config -n autoscaler-system
kubectl get pods -n autoscaler-system
```

## Documentation

- [Architecture](docs/ARCHITECTURE.md) — Components, data flow, failure modes
- [Deployment Guide](docs/DEPLOYMENT.md) — Step-by-step setup including secrets, Proxmox API user, ConfigMap, descheduler
- [Troubleshooting](docs/TROUBLESHOOTING.md) — Common issues and solutions

## CI/CD

GitHub Actions workflows (`.github/workflows/`) handle:
- **ci.yaml** — Lint, test, build on PRs; build + push to `ghcr.io` on main push
- **release.yaml** — On `v*` tags: multi-arch build, push to GHCR, cosign sign, SBOM, GitHub Release
- **security.yaml** — Grype image scans, CodeQL, dependency review, kubeconform

## License

Apache 2.0

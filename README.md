# Talos Kubernetes Node Autoscaler for Proxmox

A controller-runtime based autoscaler that watches for unschedulable pods and provisions Talos Linux worker VMs on Proxmox VE via the Proxmox REST API.

## How It Works

```
Unschedulable Pods → Controller (30s reconcile) → Proxmox API → VM
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

- **Resource-aware sizing** — 30s reconciliation window aggregates pending pod CPU/memory requests, calculates exactly how many workers are needed based on MachineClass capacity
- **Descheduler integration** — watches for nodes labeled `descheduler.kubernetes.io/node-probable-eviction`, then cordons, drains, and destroys them
- **Workers-only** — 3 control planes run permanently, never managed by the autoscaler
- **PXE boot** — boot order `scsi0;net0`, first boot PXE-fetches Talos kernel, installs to disk, subsequent boots from scsi0
- **Machine classes** — define worker tiers (tiny/standard/gpu/storage) as CRDs
- **Optional MAC/SMBIOS** — explicit `macAddress` for PXE config lookup, `serial` for identification

## Project Structure

```
.
├── autoscaler/
│   ├── main.go
│   ├── Dockerfile
│   ├── Makefile
│   └── pkg/
│       ├── autoscaler/          # Controller, types
│       └── proxmox/             # Proxmox REST API client
├── kubernetes/
│   ├── crds/                    # MachineClass, MachineDeployment CRDs
│   ├── rbac/                    # ServiceAccount, ClusterRole, Binding
│   ├── keda/                    # ScaledObject manifests
│   ├── deployment.yaml          # Controller deployment
│   ├── configmap.yaml
│   ├── namespace.yaml
│   └── networkpolicy.yaml
├── examples/
│   ├── machine-classes/         # Example MachineClass CRs
│   ├── 3-node-cluster/          # Full cluster example
│   ├── test-pod.yaml
│   └── load-test.sh
├── docs/
│   ├── README.md
│   ├── ARCHITECTURE.md
│   ├── DEPLOYMENT.md
│   ├── TROUBLESHOOTING.md
│   └── CRD_REFERENCE.md
├── .github/workflows/
│   ├── ci.yaml
│   ├── release.yaml
│   └── security.yaml
├── README.md
└── plan.md
```

## Quick Start

### Prerequisites

- Proxmox VE 8.x cluster (3 nodes)
- Talos Linux ISO uploaded to Proxmox
- Kubernetes cluster with 3 permanent control planes
- Go >= 1.22
- PXE boot infrastructure with a Talos config server

### Install

```bash
# Apply CRDs
kubectl apply -f kubernetes/crds/

# Apply RBAC
kubectl apply -f kubernetes/rbac/

# Create MachineClasses (tiny, standard, gpu, storage)
kubectl apply -f examples/machine-classes/

# Create a MachineDeployment
cat <<EOF | kubectl apply -f -
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineDeployment
metadata:
  name: standard-workers
spec:
  replicas: 1
  machineClassName: standard
  minReplicas: 1
  maxReplicas: 20
  template:
    spec:
      bootTimeout: "5m"
EOF

# Build and deploy
cd autoscaler && make build
```

### Verify

```bash
kubectl get machineclasses
kubectl get machinedeployment
kubectl get pods -n autoscaler-system
```

## Documentation

- [Architecture](docs/ARCHITECTURE.md) — Components, data flow, failure modes
- [Deployment Guide](docs/DEPLOYMENT.md) — Step-by-step setup including secrets, Proxmox API user, KEDA, descheduler
- [CRD Reference](docs/CRD_REFERENCE.md) — MachineClass, MachineDeployment fields
- [Troubleshooting](docs/TROUBLESHOOTING.md) — Common issues and solutions

## CI/CD

GitHub Actions workflows (`.github/workflows/`) handle:
- **ci.yaml** — Lint, test, build on PRs; build + push to `ghcr.io` on main push
- **release.yaml** — On `v*` tags: multi-arch build, push to GHCR, cosign sign, SBOM, GitHub Release
- **security.yaml** — Grype image scans, CodeQL, dependency review

## License

Apache 2.0

# Talos Kubernetes Node Autoscaler for Proxmox

A controller-runtime based autoscaler that watches for unschedulable pods and provisions Talos Linux worker VMs on Proxmox VE via OpenTofu.

## How It Works

```
Unschedulable Pods → Controller (30s reconcile) → OpenTofu apply → Proxmox VM
    → PXE Boot (scsi0 empty → net0) → Talos Config Server → Worker joins cluster
```

```
┌─────────────────────────────────────────────────────────────┐
│                     Kubernetes Cluster                       │
│                                                             │
│   Pending Pods ──▶ Go Controller ──▶ OpenTofu ──▶ Proxmox  │
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
├── autoscaler/                  # Go autoscaler
│   ├── main.go
│   └── pkg/
│       ├── autoscaler/          # Controller, types
│       └── tofu/                # OpenTofu subprocess wrapper
├── terraform/                   # OpenTofu modules
│   ├── main.tf                  # Root module (proxmox provider)
│   └── modules/proxmox-vm/      # VM provisioning module
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
│   ├── ARCHITECTURE.md
│   ├── DEPLOYMENT.md
│   ├── CRD_REFERENCE.md
│   └── README.md
├── Makefile
├── Dockerfile
└── plan.md
```

## Quick Start

### Prerequisites

- Proxmox VE 8.x cluster (3 nodes)
- Talos Linux ISO uploaded to Proxmox
- Kubernetes cluster with 3 permanent control planes
- OpenTofu >= 1.6, Go >= 1.22
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
make docker-build
make deploy
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
- [CRD Reference](docs/CRD_REFERENCE.md) — MachineClass, MachineTemplate, MachineDeployment fields
- [Troubleshooting](docs/README.md) — Common issues and solutions

## License

Apache 2.0

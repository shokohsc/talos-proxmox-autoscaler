# Deployment Guide

Step-by-step guide for deploying the Talos Kubernetes Node Autoscaler on a 3-node Proxmox cluster.

## Prerequisites

### Infrastructure

- **Proxmox VE 8.x** cluster with 3 nodes
- **Network**: bridged interface for Kubernetes traffic (e.g., `vmbr1`)
- **Storage**: shared storage (ZFS, Ceph, or NFS) for VM disks
- **ISO**: Talos Linux ISO uploaded to Proxmox (`pvesm download iso ...`)
- **PXE boot infrastructure**: TFTP server and Talos config server for worker boot

### Software

| Tool | Version | Purpose |
|------|---------|---------|
| `kubectl` | >= 1.29 | Cluster management |
| `tofu` | >= 1.6 | Infrastructure provisioning |
| `talosctl` | matching Talos version | Talos cluster management |
| `go` | >= 1.22 | Building the autoscaler |
| `make` | any | Build automation |

### Cluster State

You need a running Talos Kubernetes cluster with:
- 3 control plane nodes (already joined and healthy, **not managed by this autoscaler**)
- At least 1 worker node (to verify the cluster works before autoscaling)

Verify:
```bash
kubectl get nodes
# NAME       STATUS   ROLES           AGE    VERSION
# cp-0       Ready    control-plane   24h    v1.29.3
# cp-1       Ready    control-plane   24h    v1.29.3
# cp-2       Ready    control-plane   24h    v1.29.3
# worker-0   Ready    <none>          20h    v1.29.3
```

## Step 1: Clone the Repository

```bash
git clone https://github.com/your-org/talos-proxmox-autoscaler.git
cd talos-proxmox-autoscaler
```

## Step 2: Generate Required Secrets

The autoscaler needs several secrets. Generate them once:

```bash
# Create secrets directory
mkdir -p config/secrets

# Control plane endpoint (your LB or round-robin DNS)
export CONTROL_PLANE_ENDPOINT="10.0.2.10:6443"

# Bootstrap token from existing cluster
export CLUSTER_TOKEN="16o86q.[a-z0-9]{6}.[a-z0-9]{16}"

# CA certificate (base64 encoded)
export CA_CERT=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

# Proxmox API token (see Step 4 for creating a dedicated user)
export PROXMOX_API_TOKEN_ID="autoscaler@pve!autoscaler=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
export PROXMOX_API_TOKEN_SECRET="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"

# Proxmox node IPs
export PVE_NODE1="10.0.1.11"
export PVE_NODE2="10.0.1.12"
export PVE_NODE3="10.0.1.13"
```

## Step 3: Create the Environment Config

```bash
cp config/example.env config/.env
```

Edit `config/.env`:

```bash
# Proxmox
PROXMOX_API_URL="https://10.0.1.10:8006"
PROXMOX_INSECURE=true  # only if using self-signed certs

# Cluster
CONTROL_PLANE_ENDPOINT="https://10.0.2.10:6443"
CLUSTER_TOKEN="16o86q.xxxxxx.xxxxxxxxxxxxxxxx"
CA_CERT_B64="LS0tLS1CRUdJTi..."

# Provisioning
TALOS_VERSION="v1.7.0"
TALOS_INSTALLER="factory.talos.dev/installer/xxxxx:v1.7.0"
TALOS_DISK="/dev/sda"

# Autoscaler
MIN_WORKERS=1
MAX_WORKERS=20
SCALE_DOWN_DELAY=10m
DRAIN_TIMEOUT=30s

# Reconcile interval (seconds)
RECONCILE_INTERVAL=30

# Node naming
PROXMOX_VMID_START=200
NODE_PREFIX="worker"
MACHINE_CLASS_DEFAULT="standard"

# Network
K8S_BRIDGE="vmbr1"
K8S_SUBNET="10.0.2.0/24"
POD_CIDR="10.244.0.0/16"
SERVICE_CIDR="10.96.0.0/12"

# OpenTofu state (encrypted at rest)
TOFU_STATE_DIR="/var/lib/tofu"
TOFU_STATE_ENCRYPTION="aes256"
```

## Step 4: Create a Dedicated Proxmox API User

**Do not use `root@pam` for the autoscaler API token.** Create a dedicated user with minimal permissions:

1. Go to **Datacenter → Permissions → Users → Add**
2. Create a new user:
   - User name: `autoscaler`
   - Realm: `PVE` (not PAM)
   - Full name: `Talos Autoscaler`
   - Password: generate a strong, unique password

3. Go to **Datacenter → Permissions → API Tokens → Add**
4. Select user: `autoscaler@pve`
5. Token ID: `autoscaler`
6. Check **"Separate privileges"** (recommended for audit)
7. Add permissions:
   - `/vms` — `PVEVMAdmin` (create, start, stop, delete VMs)
   - `/storage` — `PVEStorageAdmin` (only for VM disk storage)
   - `/nodes` — `PVEAuditor` (read-only node info)

```bash
# Test the token
curl -s -H "Authorization: PVEAPIToken=autoscaler@pve!autoscaler=${PROXMOX_API_TOKEN_SECRET}" \
  "https://10.0.1.10:8006/api2/json/nodes" | jq '.data[].node'
```

## Step 5: Create Secrets as Mounted Files (Not Env Vars)

Secrets should be mounted as files, not passed as environment variables. Environment variables are visible in `kubectl describe pod` output and can leak into logs.

```bash
# Create a secret with individual keys
kubectl create secret generic autoscaler-secrets \
  --from-literal=PROXMOX_API_TOKEN_ID="${PROXMOX_API_TOKEN_ID}" \
  --from-literal=PROXMOX_API_TOKEN_SECRET="${PROXMOX_API_TOKEN_SECRET}" \
  --from-literal=CLUSTER_TOKEN="${CLUSTER_TOKEN}" \
  --from-literal=CA_CERT_B64="${CA_CERT_B64}" \
  -n autoscaler-system
```

The deployment mounts these as files at `/etc/secrets/`. See `kubernetes/deployment.yaml` for the volume mount configuration.

## Step 6: Create the Machine Class CRDs

```bash
kubectl apply -f kubernetes/crds/
```

Apply the machine class definitions:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineClass
metadata:
  name: tiny
spec:
  vcpu: 2
  memoryGiB: 4
  diskGiB: 30
  networkBridge: vmbr0
  storagePool: local-lvm
  proxmoxPool: k8s-workers
  labels:
    tier: lightweight
---
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
  labels:
    tier: general
---
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineClass
metadata:
  name: gpu
spec:
  vcpu: 8
  memoryGiB: 32
  diskGiB: 100
  networkBridge: vmbr0
  storagePool: local-lvm
  proxmoxPool: k8s-workers
  gpu:
    vendor: nvidia
    model: "RTX-4090"
    pciAddress: "0000:01:00.0"
  labels:
    tier: gpu
---
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineClass
metadata:
  name: storage
spec:
  vcpu: 4
  memoryGiB: 16
  diskGiB: 200
  networkBridge: vmbr0
  storagePool: local-lvm
  proxmoxPool: k8s-workers
  labels:
    tier: storage
EOF
```

Verify:
```bash
kubectl get machineclasses
# NAME       VCPU   MEMORY   DISK     MAC       SERIAL
# tiny       2      4        30
# standard   4      8        50
# gpu        8      32       100
# storage    4      16       200
```

## Step 7: Create MachineDeployments

Create a deployment for each machine class you want to autoscale:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineDeployment
metadata:
  name: standard-workers
  namespace: default
spec:
  replicas: 1
  machineClassName: standard
  minReplicas: 1
  maxReplicas: 20
  template:
    metadata:
      labels:
        role: worker
        class: standard
    spec:
      bootTimeout: 300s
---
apiVersion: autoscaler.talos.dev/v1alpha1
kind: MachineDeployment
metadata:
  name: gpu-workers
  namespace: default
spec:
  replicas: 0
  machineClassName: gpu
  minReplicas: 0
  maxReplicas: 5
  template:
    metadata:
      labels:
        role: worker
        class: gpu
    spec:
      bootTimeout: 300s
EOF
```

## Step 8: Install RBAC and Service Accounts

```bash
kubectl apply -f kubernetes/rbac/
```

This creates:
- `ServiceAccount: talos-proxmox-autoscaler`
- `ClusterRole: talos-proxmox-autoscaler` with permissions for nodes, pods, machines, events
- `ClusterRoleBinding: talos-proxmox-autoscaler`

## Step 9: Deploy KEDA Scalers

```bash
# Install KEDA (if not already installed)
kubectl apply -f https://github.com/kedacore/keda/releases/download/v2.13.1/keda-operator.yaml
kubectl apply -f https://github.com/kedacore/keda/releases/download/v2.13.1/keda-operator-metrics.yaml
kubectl apply -f https://github.com/kedacore/keda/releases/download/v2.13.1/keda-auth-webhook.yaml

# Apply the autoscaler scalers
kubectl apply -f kubernetes/keda/
```

## Step 9b: Deploy the Descheduler (Optional but Recommended)

The autoscaler does **not** perform its own utilization-based scale-down. Instead, an external descheduler watches for underutilized nodes and labels them with `descheduler.kubernetes.io/node-probable-eviction`. The autoscaler then cordons, drains, and deletes those nodes.

Deploy the descheduler separately (it is a standalone project):

```bash
# Example: deploy a descheduler that marks nodes for removal
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/descheduler/master/kubernetes/descheduler.yaml

# The descheduler labels nodes like:
#   descheduler.kubernetes.io/node-probable-eviction: "true"
# The autoscaler watches for this label and handles the actual removal.
```

If the descheduler is not deployed, no automatic scale-down will occur — workers will only be added, never removed.

## Step 10: Build and Deploy the Autoscaler

```bash
# Build the container image
make docker-build

# Push to your registry (or use local registry)
docker tag autoscaler:latest registry.internal/talos-autoscaler:v1.0.0
docker push registry.internal/talos-autoscaler:v1.0.0
```

Deploy:
```bash
# Update image reference in manifest
export IMAGE="registry.internal/talos-autoscaler:v1.0.0"

envsubst < kubernetes/deployment.yaml | kubectl apply -f -

# Or use make
make deploy
```

## Step 11: Verify Deployment

```bash
# Check pods are running
kubectl get pods -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler
# NAME                                        READY   STATUS    RESTARTS   AGE
# talos-proxmox-autoscaler-xxxx-xxxx        1/1     Running   0          30s

# Check logs
kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler -f

# Check KEDA scalers
kubectl get scaledobject
# NAME                    SCALETARGETKIND   SCALETARGETNAME         MIN   MAX
# talos-proxmox-autoscaler                     talos-proxmox-autoscaler 0     1

# Check machine deployments
kubectl get machinedeployment
# NAME                DESIRED   CURRENT   UP-TO-DATE   AVAILABLE   CLASS
# standard-workers    1         1         1            1           standard
```

## Step 12: Test Autoscaling

```bash
# Create a deployment that exceeds current capacity
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: scale-test
  namespace: default
spec:
  replicas: 15
  selector:
    matchLabels:
      app: scale-test
  template:
    metadata:
      labels:
        app: scale-test
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.9
        resources:
          requests:
            cpu: "1"
            memory: "2Gi"
          limits:
            cpu: "1"
            memory: "2Gi"
EOF

# Watch as new nodes are provisioned
watch kubectl get nodes
watch kubectl get machinedeployment
watch kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler --tail=5

# Clean up
kubectl delete deploy scale-test
```

## Step 13: Configure CI/CD

The project includes GitHub Actions workflows for:

1. **Lint and Test** — on every push
2. **Build and Push** — on main branch
3. **Deploy** — on release tags

See `.github/workflows/` for the full workflow definitions.

Required GitHub Secrets:
- `PROXMOX_API_TOKEN_ID`
- `PROXMOX_API_TOKEN_SECRET`
- `REGISTRY_URL`
- `REGISTRY_USERNAME`
- `REGISTRY_PASSWORD`

## Step 14: Monitoring

The autoscaler exposes Prometheus metrics on `:8080/metrics`:

```bash
# Key metrics to watch
autoscaler_nodes_total{class="standard"}       # Total workers by class
autoscaler_scale_ups_total                     # Scale-up events
autoscaler_scale_downs_total                   # Scale-down events (descheduler-triggered)
autoscaler_provision_duration_seconds          # VM provisioning time
autoscaler_drain_duration_seconds              # Node drain time
autoscaler_pending_pods_count                  # Unscheduleable pods
autoscaler_descheduler_evictions_total         # Nodes removed via descheduler label
```

Example Prometheus alert rules:

```yaml
groups:
- name: autoscaler
  rules:
  - alert: AutoscalerProvisioningSlow
    expr: autoscaler_provision_duration_seconds > 300
    for: 5m
    annotations:
      summary: "Autoscaler taking >5m to provision node"

  - alert: AutoscalerFailedProvisioning
    expr: rate(autoscaler_provision_failures_total[5m]) > 0
    annotations:
      summary: "Autoscaler failing to provision nodes"

  - alert: AutoscalerMaxCapacity
    expr: autoscaler_nodes_total >= autoscaler_max_workers
    for: 10m
    annotations:
      summary: "Autoscaler at max worker capacity"
```

## Security Hardening

### Proxmox API User

- **Never use `root@pam`** — create a dedicated `autoscaler@pve` user
- Grant only the minimum permissions needed (VM management, storage, node read)
- Rotate the API token periodically

### Secret Management

- Mount secrets as files, not environment variables
- Environment variables are visible via `kubectl describe pod` and can leak into logs
- The deployment mounts secrets at `/etc/secrets/` with `readOnlyRootFilesystem: true`

### OpenTofu State

- **Never store state in an unencrypted ConfigMap** — ConfigMaps are base64-encoded, not encrypted
- Store state on an encrypted volume or use an encrypted backend (S3 with SSE, GCS with CMEK)
- The deployment uses an `emptyDir` volume with encryption at rest (if the cluster supports it)
- For production, configure S3 backend with server-side encryption

### Network Security

- Deploy a `NetworkPolicy` to restrict autoscaler traffic to Proxmox API only
- Ensure Proxmox API is not exposed to the public internet
- Use TLS for all API communication

### Container Security

The deployment already includes:
- `runAsNonRoot: true`
- `readOnlyRootFilesystem: true`
- `allowPrivilegeEscalation: false`
- `capabilities.drop: [ALL]`
- `seccompProfile: RuntimeDefault`

### RBAC

- The autoscaler service account has minimal permissions
- CRD access is restricted to machineclasses (read-only), machinetemplates and machinedeployments (full access)
- Core resource access is limited to nodes, pods, and events

## Post-Deployment Checklist

- [ ] Dedicated Proxmox API user created (not root)
- [ ] API token tested and working
- [ ] Talos ISO uploaded and available
- [ ] PXE boot infrastructure configured
- [ ] Machine classes created and visible via `kubectl get machineclasses`
- [ ] MachineDeployments created
- [ ] Autoscaler pod is running and healthy
- [ ] KEDA scalers are active
- [ ] Descheduler deployed (if scale-down is desired)
- [ ] Secrets mounted as files (not env vars)
- [ ] OpenTofu state stored securely
- [ ] Test scale-up produces a new worker node
- [ ] Test scale-down destroys a worker node (requires descheduler label)
- [ ] Prometheus metrics endpoint responds
- [ ] Logs show no errors for 30 minutes

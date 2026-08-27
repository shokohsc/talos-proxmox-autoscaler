# Deployment Guide

Step-by-step guide for deploying the Talos Kubernetes Node Autoscaler on a 3-node Proxmox cluster.

## Prerequisites

### Infrastructure

- **Proxmox VE 9.2.x** cluster with 3 nodes
- **Network**: bridged interface for Kubernetes traffic (e.g., `vmbr1`)
- **Storage**: shared storage (ZFS, Ceph, or NFS) for VM disks
- **PXE boot infrastructure**: TFTP server and Talos config server for worker boot

### Software

| Tool | Version | Purpose |
|------|---------|---------|
| `kubectl` | >= 1.29 | Cluster management |
| `talosctl` | matching Talos version | Talos cluster management |
| `go` | >= 1.26 | Building the autoscaler |
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
PROXMOX_NODE="pve-1"
BASE_VMID=200

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
   - `/vm` — `PVEAudit` (required so `ListVMs` returns guests with their tags)

```bash
# Test the token
curl -s -H "Authorization: PVEAPIToken=autoscaler@pve!autoscaler=${PROXMOX_API_TOKEN_SECRET}" \
  "https://10.0.1.10:8006/api2/json/nodes" | jq '.data[].node'

# ListVMs probe (requires Audit on /vm/)
curl -s -H "Authorization: PVEAPIToken=autoscaler@pve!autoscaler=${PROXMOX_API_TOKEN_SECRET}" \
  "https://10.0.1.10:8006/api2/json/cluster/resources?type=vm" | jq '.data[].name'
```

## Step 5: Create Secrets as Mounted Files (Not Env Vars)

Secrets should be mounted as files, not passed as environment variables. Environment variables are visible in `kubectl describe pod` output and can leak into logs.

**Option A: API Token Auth (recommended)**

```bash
# Create a secret with API token credentials
kubectl create secret generic autoscaler-secrets \
  --from-literal=PROXMOX_API_TOKEN_ID="${PROXMOX_API_TOKEN_ID}" \
  --from-literal=PROXMOX_API_TOKEN_SECRET="${PROXMOX_API_TOKEN_SECRET}" \
  --from-literal=CLUSTER_TOKEN="${CLUSTER_TOKEN}" \
  --from-literal=CA_CERT_B64="${CA_CERT_B64}" \
  -n autoscaler-system
```

**Option B: Username/Password Auth**

```bash
# Create a secret with username and password
kubectl create secret generic autoscaler-secrets \
  --from-literal=PROXMOX_USERNAME="root@pam" \
  --from-literal=PROXMOX_PASSWORD="${PROXMOX_PASSWORD}" \
  --from-literal=CLUSTER_TOKEN="${CLUSTER_TOKEN}" \
  --from-literal=CA_CERT_B64="${CA_CERT_B64}" \
  -n autoscaler-system
```

The autoscaler auto-detects which auth method to use based on which secret fields are present. If `proxmox_password` exists, it uses username/password auth; otherwise it uses API token auth.

The deployment mounts these as files at `/etc/secrets/`. See `kubernetes/deployment.yaml` for the volume mount configuration.

The autoscaler reads its own namespace from the `NAMESPACE` env var, which is automatically set via the Kubernetes downward API (`metadata.namespace`). If you deploy in a custom namespace, just change the `namespace:` field in the manifests — no code configuration needed.

## Step 6: Create the ConfigMap

All VM specs and scaling parameters live in a single ConfigMap. Apply it:

```bash
kubectl apply -f kubernetes/configmap.yaml
```

Or create it directly:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: autoscaler-config
  namespace: autoscaler-system
data:
  # VM specs (dynamic sizing)
  min_cpu: "2"
  max_cpu: "8"
  min_memory_gib: "4"
  max_memory_gib: "16"
  disk_gib: "50"
  storage_pool: "local-lvm"
  network_bridge: "vmbr0"

  # Optional: explicit MAC for PXE config lookup
  # mac_address: "52:54:00:AA:BB:CC"
  # serial: "worker-standard-001"
  # cpu_type: "host"

  # Optional: extra tags appended to provisioned VMs (all VMs get autoscaler_tag, GPU VMs also get "gpu")
  tags: "autoscaler,worker"

  # Optional: VLAN tag for primary network interface
  vlan_id: "0"

  # Scaling
  cluster_name: "k8s"

  # Ownership tag for VMs this autoscaler manages (default "talos")
  autoscaler_tag: "talos"
  min_workers: "1"
  max_workers: "20"

  # Worker types
  base_vmid: "2000"
  base_gpu_vmid: "3000"
  worker_prefix: "worker-vm"
  gpu_prefix: "worker-vm-gpu"

  # Regular workers
  worker_nodes: '[{"name":"worker-vm"}]'

  # GPU workers (optional - only if you need GPU passthrough)
  # Each entry defines a GPU type with PCI devices for passthrough
  worker_gpu_nodes: '[{"type":"tesla-p4","nodes":["pve1","pve2"],"pci_devices":[{"id":"0000:01:00.0","pcie":true,"gpu":true}]},{"type":"tesla-p40","nodes":["pve3"],"pci_devices":[{"id":"0000:41:00.0","pcie":true,"gpu":true}]}]'
EOF
```

Verify:
```bash
kubectl get configmap autoscaler-config -n autoscaler-system -o yaml
```

**Important:** Environment variables sourced from the ConfigMap are set at pod creation time and don't update when the ConfigMap changes. After modifying the ConfigMap, restart the autoscaler to apply:

```bash
kubectl rollout restart deployment/talos-proxmox-autoscaler -n autoscaler-system
```

## Step 7: Install RBAC and Service Accounts

```bash
kubectl apply -f kubernetes/rbac/
```

This creates:
- `ServiceAccount: talos-proxmox-autoscaler`
- `ClusterRole: talos-proxmox-autoscaler` with permissions for nodes, pods, configmaps, events
- `ClusterRoleBinding: talos-proxmox-autoscaler`

## Step 8: Deploy the Descheduler (Optional but Recommended)

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

### Stuck / permanently-unregistered VMs

Before scale-down deletes a worker, it skips any VM whose node has **not** joined the Kubernetes cluster (it waits for the node to finish provisioning first). If a provisioned VM never joins the cluster, this guard protects it forever: it can never be scale-down-deleted, still counts toward `autoscaler_nodes_total` / worker capacity indefinitely, and must be removed manually.

To recover, delete the VM manually (for example `qm destroy <vmid>` on the Proxmox host) and, if it created a stale Kubernetes `Node` object, delete that too (`kubectl delete node <name>`). Until you do, it will keep counting toward current worker capacity and may block future scale-ups.

## Step 9: Build and Deploy the Autoscaler

```bash
# Build the container image
make docker-build

# Push to GitHub Container Registry
docker tag autoscaler:latest ghcr.io/your-org/talos-proxmox-autoscaler:v1.0.0
docker push ghcr.io/your-org/talos-proxmox-autoscaler:v1.0.0
```

Deploy:
```bash
# Update image reference in manifest
export IMAGE="ghcr.io/your-org/talos-proxmox-autoscaler:v1.0.0"

envsubst < kubernetes/deployment.yaml | kubectl apply -f -

# Or use make
make deploy
```

## Step 10: Verify Deployment

```bash
# Check pods are running
kubectl get pods -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler
# NAME                                        READY   STATUS    RESTARTS   AGE
# talos-proxmox-autoscaler-xxxx-xxxx        1/1     Running   0          30s

# Check logs
kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler -f

# Check configmap
kubectl get configmap autoscaler-config -n autoscaler-system -o yaml

# Check logs
kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler -f

# Check pending pods
kubectl get pods --field-selector=status.phase=Pending
```

## Step 11: Test Autoscaling

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
watch kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler --tail=5

# Clean up
kubectl delete deploy scale-test
```

## Step 12: Configure CI/CD

The project includes GitHub Actions workflows for:

1. **Lint and Test** — on every push
2. **Build and Push** — on main branch
3. **Deploy** — on release tags

See `.github/workflows/` for the full workflow definitions.

Required GitHub Secrets:
- `PROXMOX_API_TOKEN_ID`
- `PROXMOX_API_TOKEN_SECRET`

## Step 13: Monitoring

The autoscaler exposes Prometheus metrics on `:8080/metrics`:

```bash
# Key metrics to watch
autoscaler_nodes_total{type="worker-vm"}         # Regular workers
autoscaler_nodes_total{type="worker-vm-gpu"}     # GPU workers
autoscaler_scale_ups_total                       # Scale-up events
autoscaler_scale_downs_total                     # Scale-down events
autoscaler_provision_duration_seconds            # VM provisioning time
autoscaler_drain_duration_seconds                # Node drain time
autoscaler_pending_pods_count                    # Unscheduleable pods
autoscaler_pending_gpu_count                     # Pending GPU requests
autoscaler_descheduler_evictions_total           # Nodes removed via descheduler label
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
- ConfigMap access for reading `autoscaler-config`
- Core resource access is limited to nodes, pods, and events

## Post-Deployment Checklist

- [ ] Dedicated Proxmox API user created (not root)
- [ ] API token tested and working
- [ ] PXE boot infrastructure configured
- [ ] ConfigMap `autoscaler-config` created with correct VM specs
- [ ] Autoscaler pod is running and healthy
- [ ] Descheduler deployed (if scale-down is desired)
- [ ] Secrets mounted as files (not env vars)
- [ ] Test scale-up produces a new worker node
- [ ] Test scale-down destroys a worker node (requires descheduler label)
- [ ] Prometheus metrics endpoint responds
- [ ] Logs show no errors for 30 minutes

# Worker Autoscaler Test

Testing the Talos worker autoscaler on a Proxmox cluster.

**Note:** This autoscaler only manages worker nodes. The 3 control-plane nodes are permanent.

## Prerequisites

- Proxmox VE 8.x with API access
- 3 Talos control-plane nodes already running
- PXE server configured with Talos config server
- Talos autoscaler deployed (see `kubernetes/` directory)
- Dedicated Proxmox API user with minimal permissions (not `root@pam`)

## How It Works

### Boot Flow

VMs are created with boot order `scsi0;net0`. On first boot, scsi0 is empty (no OS), so the BIOS falls through to PXE on net0. The PXE/TFTP server serves the Talos kernel, which fetches its machine config from a remote config server matched by the VM's MAC address prefix. Talos installs itself to scsi0 and reboots. On subsequent boots the VM boots directly from scsi0 — no PXE involved.

### Scaling Flow

1. **Scale-up**: Pods are created that exceed current capacity. Their CPU/memory requests are unschedulable.
2. **Reconciliation (every 30s)**: The controller aggregates requests of all unschedulable pods, calculates how many workers are needed based on the machine specs in the ConfigMap, and provisions that many VMs (up to `max_workers`).
3. **Node readiness**: 2-5 minutes per node (PXE boot + Talos install + reboot + kubelet registration).
4. **Pod scheduling**: Pending pods schedule to new workers as they become ready.

### Scale-Down (Descheduler-Driven)

An external descheduler labels underutilized nodes with `descheduler.kubernetes.io/node-probable-eviction`. The autoscaler watches for this label and handles the actual removal (cordon, drain, VM destroy).

## Setup

### 1. Create autoscaler config

```bash
kubectl apply -f manifests/configmap.yaml
```

### 2. Deploy autoscaler

```bash
kubectl apply -f ../../kubernetes/
```

### 3. Create Proxmox API secret

```bash
kubectl create secret generic autoscaler-secrets \
  --from-literal=proxmox_api_token_id="autoscaler@pve!mytoken=<token-id>" \
  --from-literal=proxmox_api_token_secret="<secret>" \
  -n autoscaler-system
```

### 4. Deploy descheduler (for scale-down)

```bash
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/descheduler/master/kubernetes/descheduler.yaml
```

### 5. Verify

```bash
kubectl get pods -n autoscaler-system
kubectl get nodes
```

## Triggering Scale-Up

### Option A: Load test script

```bash
chmod +x ../load-test.sh
../load-test.sh default
```

### Option B: Manual test

```bash
kubectl apply -f ../test-pod.yaml
kubectl get nodes -w
```

## Expected Behavior

1. **Scale-up**: Pending pods trigger the controller (within 30s). It aggregates CPU/memory requests of all unschedulable pods and provisions enough new worker VMs to fit them. Workers PXE-boot, get Talos config from the config server, install to scsi0, and join the cluster.

2. **Node readiness**: 2-5 minutes per node.

3. **Pod scheduling**: Pending pods schedule to new workers as they become ready.

4. **Scale-down**: The external descheduler labels underutilized nodes. The autoscaler detects this label (within 30s), cordons the node, drains it, and destroys the VM.

## Security Notes

- Secrets are mounted as files, not environment variables
- Proxmox API token uses a dedicated user, not root
- RBAC is least-privilege (no CRD permissions)

## Cleanup

```bash
kubectl delete -f ../test-pod.yaml --ignore-not-found
kubectl delete deployment load-test -n default --ignore-not-found
kubectl delete -f manifests/configmap.yaml
```

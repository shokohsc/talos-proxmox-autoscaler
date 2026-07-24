# Worker Autoscaler Test

Testing the Talos **worker** autoscaler with KEDA and descheduler on a Proxmox cluster.

**Note:** This autoscaler only manages worker nodes. The 3 control-plane nodes are permanent and managed outside this system.

## Prerequisites

- Proxmox VE 8.x with API access
- 3 Talos control-plane nodes already running
- PXE server configured with Talos config server
- KEDA installed (`kubectl apply -f https://github.com/kedacore/keda/releases/download/v2.14.0/keda-operator.yaml`)
- Talos autoscaler deployed (see `kubernetes/` directory)
- Dedicated Proxmox API user with minimal permissions (not `root@pam`)

## How It Works

### Boot Flow

VMs are created with boot order `scsi0;net0`. On first boot, scsi0 is empty (no OS), so the BIOS falls through to PXE on net0. The PXE/TFTP server serves the Talos kernel, which fetches its machine config from a remote config server matched by the VM's MAC address prefix. Talos installs itself to scsi0 and reboots. On subsequent boots the VM boots directly from scsi0 — no PXE involved.

### Scaling Flow

1. **Scale-up**: Pods are created that exceed current capacity. Their CPU/memory requests are unschedulable.
2. **Reconciliation (every 30s)**: The controller aggregates requests of all unschedulable pods, calculates how many workers of the MachineClass capacity are needed, and provisions that many VMs (up to `maxReplicas`).
3. **Node readiness**: 2-5 minutes per node (PXE boot + Talos install + reboot + kubelet registration).
4. **Pod scheduling**: Pending pods schedule to new workers as they become ready.

### Scale-Down (Descheduler-Driven)

The autoscaler does **not** perform its own utilization-based scale-down. Instead, an external descheduler project analyzes node utilization and labels unneeded nodes with `descheduler.kubernetes.io/node-probable-eviction`. The autoscaler watches for this label and handles the actual removal (cordon, drain, VM destroy).

## Setup

### 1. Create machine class

```bash
kubectl apply -f ../machine-classes/standard.yaml
```

### 2. Create worker deployment

```bash
kubectl apply -f manifests/workers.yaml
```

### 3. Deploy KEDA scaler

```bash
kubectl apply -f manifests/keda-scaledobject.yaml
```

### 4. Deploy the descheduler (for scale-down)

```bash
# Deploy the external descheduler that marks nodes for eviction
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/descheduler/master/kubernetes/descheduler.yaml
```

### 5. Verify

```bash
kubectl get machinedeployments -n autoscaler-system
kubectl get scaledobject -n autoscaler-system
kubectl get nodes
```

You should see 3 control-plane nodes (permanent, not managed by autoscaler) and 1 worker node.

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
kubectl get machinedeployments -n autoscaler-system -w
```

## Expected Behavior

1. **Scale-up**: Pending pods trigger KEDA, which signals the autoscaler. The controller aggregates CPU/memory requests of all unschedulable pods and provisions enough new worker VMs to fit them. Workers PXE-boot (scsi0 empty → net0), get Talos config from the config server, install to scsi0, and join the cluster.

2. **Node readiness**: 2-5 minutes per node (PXE boot + Talos install + reboot + kubelet registration).

3. **Pod scheduling**: Pending pods schedule to new workers as they become ready.

4. **Scale-down**: The external descheduler labels underutilized nodes with `descheduler.kubernetes.io/node-probable-eviction`. The autoscaler detects this label (within 30s), cordons the node, drains it, and destroys the VM.

## Security Notes

- Secrets are mounted as files, not environment variables
- Proxmox API token uses a dedicated user, not root
- OpenTofu state should be stored encrypted

## Cleanup

```bash
kubectl delete -f ../test-pod.yaml --ignore-not-found
kubectl delete deployment test-batch -n default --ignore-not-found
kubectl patch machinedeployment workers -n autoscaler-system -p '{"spec":{"replicas":1}}'
kubectl delete -f manifests/keda-scaledobject.yaml
kubectl delete -f manifests/workers.yaml
kubectl delete -f ../machine-classes/standard.yaml
```

# Troubleshooting

Common issues and solutions for the Talos Kubernetes Node Autoscaler.

## Quick Diagnostics

Run this first to get an overview:

```bash
# Autoscaler health
kubectl get pods -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler
kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler --tail=50

# Machine state
kubectl get machineclasses
kubectl get machinedeployment

# KEDA state
kubectl get scaledobject

# Pending pods
kubectl get pods --field-selector=status.phase=Pending
kubectl get events --field-selector reason=FailedScheduling --sort-by='.lastTimestamp' | tail -20
```

## Issues and Solutions

### 1. Autoscaler Pod Not Starting

**Symptoms:**
```
NAME                                        READY   STATUS             RESTARTS   AGE
talos-proxmox-autoscaler-xxxx-xxxx         0/1     CrashLoopBackOff   5          2m
```

**Check logs:**
```bash
kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler --previous
```

**Common causes:**

| Log Message | Cause | Fix |
|-------------|-------|-----|
| `connection refused` to Proxmox API | Wrong URL or port | Verify `PROXMOX_API_URL` in secret |
| `unauthorized` | Invalid API token | Regenerate token, check secret |
| `certificate verify failed` | Self-signed cert | Set `PROXMOX_INSECURE=true` |
| `context deadline exceeded` | Network issue | Check firewall rules to Proxmox |
| `namespace not found` | Missing namespace | Create namespace or fix deployment manifest |
| `secret not found` | Missing secrets | Create the autoscaler-secrets secret |

**Fix:**
```bash
# Verify the secret exists and has correct keys
kubectl get secret autoscaler-secrets -n autoscaler-system -o yaml

# Recreate if needed
kubectl delete secret autoscaler-secrets -n autoscaler-system
kubectl create secret generic autoscaler-secrets \
  --from-literal=PROXMOX_API_TOKEN_ID="autoscaler@pve!autoscaler=..." \
  --from-literal=PROXMOX_API_TOKEN_SECRET="..." \
  --from-literal=CLUSTER_TOKEN="..." \
  --from-literal=CA_CERT_B64="..." \
  -n autoscaler-system
```

### 2. VMs Not Provisioning

**Symptoms:**
- Pods stuck in `Pending` with `FailedScheduling`
- Autoscaler logs show `creating VM` but no new VMs appear
- MachineDeployment status shows `replicas: 1, readyReplicas: 1` but pods remain pending

**Diagnosis:**
```bash
# Check autoscaler events
kubectl get events -n autoscaler-system --field-selector involvedObject.kind=MachineDeployment

# Check Proxmox for new VMs
ssh root@10.0.1.11 "qm list | grep worker"
```

**Common causes:**

| Cause | Symptom | Fix |
|-------|---------|-----|
| Disk space full | API call fails with "no space on device" | Free Proxmox storage or increase disk |
| VMID conflict | API call fails with "already exists" | Check `BASE_VMID`, clear stale VMIDs |
| Bridge misconfigured | VM created but no network | Verify `K8S_BRIDGE` matches Proxmox bridge name |
| Token expired | Nodes can't join cluster | Rotate bootstrap token |
| Storage pool wrong | API call fails with pool error | Verify `storagePool` in MachineClass matches Proxmox |
| API auth failure | 401 Unauthorized from Proxmox | Verify API token ID and secret |

**Fix:**
```bash
# Test Proxmox API connectivity
curl -s -H "Authorization: PVEAPIToken=autoscaler@pve!autoscaler=${PROXMOX_API_TOKEN_SECRET}" \
  "https://10.0.1.10:8006/api2/json/nodes" | jq '.data[].node'

# List VMs on a specific node
ssh root@10.0.1.11 "qm list"

# Manually destroy a stuck VM
ssh root@10.0.1.11 "qm destroy <vmid> --purge"
```

### 3. Nodes Provisioned But Not Joining Cluster

**Symptoms:**
- VMs exist in Proxmox and are running
- `kubectl get nodes` doesn't show the new node
- Autoscaler logs show `waiting for node to register`

**Diagnosis:**
```bash
# Check if VM is running
ssh root@10.0.1.11 "qm status 203"

# Check Talos console (serial console)
ssh root@10.0.1.11 "qm terminal 203"

# Check Kubernetes for join attempts
kubectl get events --field-selector reason=NodeNotReady --sort-by='.lastTimestamp' | tail -5
```

**Common causes:**

| Cause | Fix |
|-------|-----|
| Wrong cluster token | Regenerate and update secret |
| CA cert mismatch | Extract correct cert from existing cluster |
| Network unreachable | Check firewall allows 6443 from VMs to control planes |
| Bootstrap token expired | Generate new token with `talosctl` |
| PXE/config server unreachable | Verify config server is serving Talos configs |
| Control plane LB down | Verify HAProxy/keepalived or round-robin DNS |

**Fix:**
```bash
# Get correct bootstrap token
talosctl --nodes 10.0.2.10 config tnet list

# Update the secret with new token
kubectl patch secret autoscaler-secrets -n autoscaler-system \
  -p '{"data":{"CLUSTER_TOKEN":"'$(echo -n '16o86q.newtoken.newsecret' | base64)'"}}'

# Restart autoscaler to pick up new config
kubectl rollout restart deployment/talos-proxmox-autoscaler -n autoscaler-system
```

### 4. PXE Boot Issues

**Symptoms:**
- VMs created but don't boot from network
- PXE timeout errors in Proxmox console
- VMs boot but fail to get Talos config

**Diagnosis:**
```bash
# Check VM boot order in Proxmox
ssh root@10.0.1.11 "qm config 203 | grep boot"

# Check PXE server logs
ssh root@10.0.1.11 "journalctl -u tftpd-hpa | tail -20"

# Verify MAC address matches config server
ssh root@10.0.1.11 "qm config 203 | grep net0"
```

**Common causes:**

| Cause | Fix |
|-------|-----|
| Boot order wrong | Set `scsi0;net0` as boot order — scsi0 first (empty on first boot falls through to PXE) |
| PXE server not responding | Restart TFTP/DHCP services |
| MAC address mismatch | Verify MachineClass `macAddress` matches what Proxmox assigns |
| Config server doesn't have entry for MAC | Add MAC to config server's mapping |
| Network bridge disconnected | Verify VM is on correct bridge |

**Fix:**
```bash
# Set boot order to scsi0;net0 (scsi0 first, PXE fallback)
ssh root@10.0.1.11 "qm set 203 --boot order=scsi0;net0"

# Verify MAC address in Proxmox matches MachineClass
ssh root@10.0.1.11 "qm config 203 | grep net0"
# net0: virtio=52:54:00:AA:BB:CC,bridge=vmbr0
```

### 5. Scale-Down Not Working

**Symptoms:**
- Cluster has many nodes but utilization is low
- MachineDeployment replicas don't decrease
- Autoscaler logs show no eviction activity

**Diagnosis:**
```bash
# Check if descheduler is running
kubectl get pods -n kube-system -l component=descheduler

# Check for eviction labels on nodes
kubectl get nodes -l descheduler.kubernetes.io/node-probable-eviction

# Check node utilization
kubectl top nodes

# Check if nodes have pods with PodDisruptionBudgets
kubectl get pdb -A
```

**Common causes:**

| Cause | Fix |
|-------|-----|
| Descheduler not deployed | Deploy the descheduler — without it, no scale-down occurs |
| Descheduler not labeling nodes | Check descheduler config/logs |
| PodDisruptionBudget blocks drain | Adjust PDB or reduce `maxUnavailable` |
| Pods with `cluster-autoscaler.kubernetes.io/scale-down-disabled` | Remove annotation |
| Node has local storage PVs | Migrate PVCs or use shared storage |

**Fix:**
```bash
# Check PDBs blocking drain
kubectl get pdb -A
kubectl describe pdb <name>

# Temporarily disable PDB (only if safe)
kubectl delete pdb <name>

# Force scale down (emergency only)
kubectl patch machinedeployment standard-workers \
  -p '{"spec":{"replicas":1}}'
```

### 6. High Latency During Scale-Up

**Symptoms:**
- Scale-up takes > 5 minutes
- Autoscaler logs show long API call times
- Pods remain pending for extended periods

**Diagnosis:**
```bash
# Check provisioning time metric
curl -s localhost:8080/metrics | grep provision_duration

# Check autoscaler logs for API call times
kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler | grep "API call"

# Check network latency to Proxmox
curl -w "@curl-format.txt" -o /dev/null -s https://10.0.1.10:8006
```

**Optimizations:**

| Approach | Impact |
|----------|--------|
| Use local storage for VM disks | Faster than NFS/CIFS |
| Reduce VM disk size | Faster disk allocation |
| Use SSDs on Proxmox nodes | 2-3x faster than HDD |
| Pre-allocate a warm pool | Keep 1-2 extra nodes ready |
| Increase `MIN_WORKERS` | Reduces cold starts |

### 7. GPU Passthrough Not Working

**Symptoms:**
- GPU worker node joins cluster but GPU not available
- `nvidia-smi` not found in node
- Pods requesting GPU resource can't schedule

**Diagnosis:**
```bash
# Check node labels and resources
kubectl describe node <gpu-worker> | grep -A 10 "Allocatable"

# Check PCI passthrough in Proxmox
ssh root@10.0.1.12 "qm config 205 | grep hostpci"

# Check IOMMU group
ssh root@10.0.1.12 "dmesg | grep -i iommu"
```

**Fix:**
```bash
# 1. Enable IOMMU on Proxmox host (GRUB)
# Edit /etc/default/grub:
# GRUB_CMDLINE_LINUX="intel_iommu=on iommu=pt"
# update-grub && reboot

# 2. Blacklist GPU driver on host
cat >> /etc/modprobe.d/blacklist-nvidia.conf <<EOF
blacklist nouveau
blacklist nvidia
blacklist nvidiafb
blacklist snd_hda_intel
EOF

# 3. Add VFIO modules
echo -e "vfio\nvfio_iommu_type1\nvfio_pci" > /etc/modules-load.d/vfio.conf

# 4. Verify PCI passthrough in VM config
qm set <vmid> --hostpci0 0000:01:00,pcie=1,x-vga=1

# 5. Install NVIDIA device plugin in Kubernetes
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.3/nvidia-device-plugin.yml
```

### 8. Proxmox API Timeout or Connection Issues

**Symptoms:**
- Autoscaler logs show API call timeouts
- VM operations take excessively long or fail
- Inconsistent replica count

**Diagnosis:**
```bash
# Test API latency from inside the cluster
kubectl run debug --rm -it --image=alpine -- sh
apk add curl
time curl -sk -H "Authorization: PVEAPIToken=autoscaler@pve!autoscaler=${PROXMOX_API_TOKEN_SECRET}" \
  "https://10.0.1.10:8006/api2/json/nodes"

# Check Proxmox API health
ssh root@10.0.1.11 "pveproxy status"
```

**Fix:**
```bash
# Restart Proxmox API service if unresponsive
ssh root@10.0.1.11 "systemctl restart pveproxy"

# Check for API rate limiting
ssh root@10.0.1.11 "journalctl -u pveproxy | tail -20"

# Verify network connectivity
ping -c 5 10.0.1.10
```

### 9. RBAC Permission Errors

**Symptoms:**
```
Error from server (Forbidden): autoscaler.talos.dev "standard" is forbidden:
  User "system:serviceaccount:autoscaler-system:talos-proxmox-autoscaler" cannot list resource "machinetemplates"
```

**Fix:**
```bash
# Re-apply RBAC
kubectl apply -f kubernetes/rbac/

# Verify permissions
kubectl auth can-i list machinetemplates --as=system:serviceaccount:autoscaler-system:talos-proxmox-autoscaler
# yes

kubectl auth can-i create events --as=system:serviceaccount:autoscaler-system:talos-proxmox-autoscaler
# yes
```

### 10. Network Partition Between Kubernetes and Proxmox

**Symptoms:**
- All operations timeout
- New nodes can't join cluster
- Autoscaler logs show `connection reset by peer`

**Diagnosis:**
```bash
# Test Proxmox API from inside the cluster
kubectl run debug --rm -it --image=alpine -- sh
apk add curl
curl -sk https://10.0.1.10:8006/api2/json/version

# Test Kubernetes API from Proxmox node
ssh root@10.0.1.11 "curl -sk https://10.0.2.10:6443/healthz"

# Check firewall rules
ssh root@10.0.1.11 "iptables -L -n | grep -E '(6443|8006)'"
```

**Fix:**
```bash
# On Proxmox host - allow Kubernetes API port
ufw allow from 10.0.2.0/24 to any port 6443

# On Kubernetes nodes - allow Proxmox API port
ufw allow from 10.0.1.0/24 to any port 8006

# Check for dropped packets
tcpdump -i vmbr0 port 6443 -n
```

## Log Levels

Set log verbosity in the autoscaler deployment:

```yaml
env:
- name: LOG_LEVEL
  value: "info"  # trace, debug, info, warn, error
- name: LOG_FORMAT
  value: "json"  # json, text
```

Debug mode:
```bash
kubectl set env deployment/talos-proxmox-autoscaler -n autoscaler-system LOG_LEVEL=debug
```

## Health Checks

The autoscaler exposes health and readiness endpoints:

```bash
# Liveness
kubectl exec -n autoscaler-system deploy/talos-proxmox-autoscaler -- \
  wget -qO- http://localhost:8081/healthz

# Readiness
kubectl exec -n autoscaler-system deploy/talos-proxmox-autoscaler -- \
  wget -qO- http://localhost:8081/readyz
```

## Getting Help

If the above doesn't resolve your issue:

1. Check the [GitHub Issues](https://github.com/your-org/talos-proxmox-autoscaler/issues)
2. Collect diagnostics:
   ```bash
   # Autoscaler logs
   kubectl logs -n autoscaler-system -l app.kubernetes.io/name=talos-proxmox-autoscaler --all-containers > /tmp/autoscaler-logs.txt

   # Machine state
   kubectl get machineclasses -o yaml > /tmp/machineclasses.yaml
   kubectl get machinedeployment -o yaml > /tmp/machinedeployment.yaml

   # Proxmox status
   ssh root@10.0.1.11 "qm list" > /tmp/proxmox-vms.txt
   ```
3. Open an issue with the diagnostic output

## Emergency Procedures

### Kill Switch

To completely stop autoscaling without destroying existing nodes:

```bash
# Pause the MachineDeployment
kubectl patch machinedeployment standard-workers \
  -p '{"spec":{"paused":true}}'

# Or scale to 0 replicas (this will drain and destroy workers)
kubectl patch machinedeployment standard-workers \
  -p '{"spec":{"replicas":0}}'
```

### Destroy All Workers

```bash
# Scale all deployments to 0
kubectl get machinedeployment -o name | xargs -I {} \
  kubectl patch {} -p '{"spec":{"replicas":0}}'

# Wait for drain to complete, then verify
kubectl get nodes
```

### Force Delete Stuck Nodes

```bash
# Cordon the node
kubectl cordon <node-name>

# Force delete
kubectl delete node <node-name> --ignore-not-found

# Destroy in Proxmox
ssh root@10.0.1.11 "qm destroy <vmid> --purge"
```

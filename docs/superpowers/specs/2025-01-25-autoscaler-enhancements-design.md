# Talos Autoscaler Enhancements Design

## Overview

Enhance the Talos Kubernetes Node Autoscaler with flexible authentication, dynamic VM sizing, node discovery, and optional VM configuration (tags, PCI passthrough).

## 1. Authentication & Secrets

### Current State
Only API token auth via mounted secret files (`proxmox_api_token_id`, `proxmox_api_token_secret`).

### Proposed Change
- Add `proxmox_username` and `proxmox_password` to Kubernetes Secret
- Auto-detect auth method: if `proxmox_password` exists → username/password auth, else → API token
- Both auth types optional - whichever fields are present determine the method

### Secret Format
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: autoscaler-secrets
data:
  proxmox_username: <base64>
  proxmox_password: <base64>          # Optional - triggers password auth
  proxmox_api_token_id: <base64>      # Optional - triggers token auth
  proxmox_api_token_secret: <base64>  # Required if using token auth
```

### Implementation
- `main.go`: Read both secret fields, pass to `NewClient()`
- `Client` struct: Add `authType` field (`"password"` or `"token"`)
- `do()` method: Use correct Authorization header format based on `authType`
  - Token: `Authorization: PVEAPIToken=USER@REALM!TOKENID=UUID`
  - Password: Login via `/access/ticket`, cache ticket, use `Cookie: PVEAuthCookie=...`

---

## 2. Node Discovery & Selection

### Current State
Hardcoded `PROXMOX_NODE` env var required.

### Proposed Change
- Add `ListNodes()` method to Proxmox client: `GET /nodes`
- Return sorted list of active node names
- If `PROXMOX_NODE` env var set → use that node (backward compatible)
- If not set → use first active node from cluster

### Node Selection Logic
```go
func (c *Client) GetNode() string {
    if c.node != "" { return c.node }
    nodes, _ := c.ListNodes()
    return nodes[0]  // first active node
}
```

### Benefits
- Removes requirement to know node names upfront
- Works across multi-node clusters
- Env var override still available for testing/specific needs

---

## 3. Dynamic VM Sizing

### Current State
Fixed VM size (VCPU=4, MemoryGiB=8) for all workers.

### Proposed Change
- Replace `VCPU` and `MemoryGiB` with min/max ranges: `MinCPU`, `MaxCPU`, `MinMemoryGiB`, `MaxMemoryGiB`
- `calculateNeeded()` returns both count AND size per VM
- New `VMSize` struct: `{CPU int, MemoryGiB int}`

### ConfigMap Fields
```yaml
min_workers: 1
max_workers: 10
min_cpu: 2          # per VM
max_cpu: 8          # per VM
min_memory_gib: 4   # per VM
max_memory_gib: 16  # per VM
```

### Optimal Fit Algorithm
1. Sum pending pod resource requests (CPU/memory)
2. Divide by max worker size to get minimum VM count
3. If count < minWorkers → use minWorkers, adjust VM size down
4. If count > maxWorkers → use maxWorkers, adjust VM size up
5. Clamp each VM size to [min, max] range

### Implementation
```go
type PendingResources struct {
    TotalCPU    int
    TotalMemory int
}

type VMSize struct {
    CPU       int
    MemoryGiB int
}

func calculateVMSize(pending PendingResources, count int) VMSize {
    // Optimal fit calculation
}
```

Pass VMSize to `createVMFromScratch()` instead of fixed values.

---

## 4. Tags & PCI Configuration

### Current State
No VM tagging or PCI passthrough support.

### Proposed Change

**Tags:**
- ConfigMap field: `tags` (string, space-separated)
- Example: `"autoscaler,worker,v1"`
- Passed to Proxmox API with `tags` parameter
- Optional field, omit if empty

**PCI Express Devices:**
- ConfigMap field: `pci_devices` (JSON string)
- Format: `[{"id": "0000:01:00.0", "pcie": true, "gpu": true}]`
- Parsed and passed to Proxmox API with `hostpci0` parameter
- Optional field, omit if empty

### Config Additions
```go
type Config struct {
    // ... existing fields
    Tags        string      // space-separated tags
    PCIDevices  []PCIDevice // PCI passthrough devices
}

type PCIDevice struct {
    ID    string `json:"id"`
    PCIe  bool   `json:"pcie"`
    GPU   bool   `json:"gpu"`
}
```

### Proxmox API Parameters
- Tags: `tags=autoscaler,worker`
- PCI: `hostpci0=host=0000:01:00.0,pcie=1,gpu=1`

---

## 5. Testing Strategy

### Unit Tests
- Auth detection logic
- Node discovery and selection
- VM sizing calculation
- Config parsing (tags, PCI devices)
- Target: ≥80% coverage

### Integration Points
- Proxmox API client methods
- Kubernetes ConfigMap parsing
- Reconciler loop with new config

---

## 6. Files to Modify

| File | Changes |
|------|---------|
| `autoscaler/pkg/proxmox/client.go` | Auth detection, ListNodes(), do() header logic |
| `autoscaler/pkg/autoscaler/controller.go` | Config struct, calculateNeeded(), VM sizing |
| `autoscaler/main.go` | Read both secret types |
| `kubernetes/configmap.yaml` | New fields (min/max CPU/memory, tags, pci_devices) |
| `kubernetes/deployment.yaml` | Remove PROXMOX_NODE env var |

### Files to Delete
- `autoscaler/config/sample-config.yaml` (unused)

---

## 7. Backward Compatibility

- `PROXMOX_NODE` env var still works (optional)
- API token auth still works (default if no password)
- Fixed VM sizing preserved via min=max=desired value

---

## 8. Success Criteria

- [ ] Both auth methods work (token and password)
- [ ] Node discovery works on multi-node clusters
- [ ] Dynamic sizing creates appropriately-sized VMs
- [ ] Tags appear on created VMs
- [ ] PCI devices passthrough correctly
- [ ] Unit tests ≥80% coverage
- [ ] Documentation updated

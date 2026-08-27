# Stateless HA + Tag-Based VM Tracking + Hot Config Reload

Date: 2026-08-27

## Goal

The autoscaler must run as multiple pods in a Kubernetes cluster without coordination (stateless HA), track the VMs it owns by Proxmox tags instead of in-memory state, and hot-reload configuration changes.

## Decisions (confirmed with user)

1. **HA model:** concurrent + idempotent. All replicas reconcile every 30s; Proxmox is the shared source of truth. No leader election.
2. **Ownership marker:** configurable tag, new `autoscaler_tag` config key (default `talos`), applied to every VM we create. GPU type marker stays the fixed `gpu` tag.
3. **Config reload:** keep the 30s poll; detect ConfigMap content changes via hash and log/reconcile. No informer, no fsnotify.
4. **Config changes apply to future decisions only.** No re-tagging or reconfiguring existing VMs.

## Current State (problems)

- `Reconciler.InFlight map[string]bool` + `inFlightMu` (controller.go:66-70, 284-298) is per-pod memory — two replicas each maintain their own map, so nothing prevents duplicate VM creation across pods.
- `countWorkers` counts K8s Nodes by name prefix (controller.go:265-282). Not lagging (provisioning VMs invisible) and adds in-flight counts on top.
- `scaleUp` (controller.go:320-410) uses `count` as both quantity and VM index, double pre-flights with `FindVMByName`, and writes the InFlight map.
- `scaleDown` (controller.go:412-418) drives VM index purely off counts; with VM gaps it can compute VMIDs that are already taken.
- VM tags hardcoded: `tags := "talos"` (controller.go:364), so ownership is not distinguishable per autoscaler/cluster.
- Config is re-read every tick, but there is no change detection/logging.

## Design

### 1. Stateless source of truth

- Delete `InFlight`, `inFlightMu`, `countInFlight`.
- Each reconcile computes decisions from exactly two shared sources: the ConfigMap and a fresh Proxmox VM list. No pod-local memory affects decisions.
- Idempotency through deterministic naming: all replicas compute the same next index/VMID/name from the shared VM list. A racing duplicate create hits `VMID already exists` → Proxmox rejects the loser → logged, harmless → next tick converges. A racing duplicate delete hits a 404/not-running VM → logged, harmless.
- `waitForNodeReady` goroutines are read-only; duplicated across replicas safely.
- Deployment `replicas: 1 → 2`.

### 2. Tag-based tracking

Add to `pkg/proxmox/client.go`:

```go
type VM struct {
    VMID     int    `json:"vmid"`
    Name     string `json:"name"`
    Node     string `json:"node"`
    Status   string `json:"status"`
    Type     string `json:"type"` // "qemu" | "lxc"
    Template int    `json:"template"`
    Tags     string `json:"tags"` // "," or ";" separated string
}

func (c *Client) ListVMs(ctx context.Context) ([]VM, error)
```

- One call per reconcile: `GET /api2/json/cluster/resources?type=vm`.
- **Verified** against Proxmox VE API docs: response items include `vmid`, `name`, `node`, `status`, `type` (`qemu`/`lxc`), `template` (0/1), `tags` (string, `,`- or `;`-separated). The API token needs `Audit` privilege on the `/vm/` ACL path to see guests — document in DEPLOYMENT.md.
- Controller filters the list: owned = tags (split on `,` and `;`) contain `cfg.AutoScalerTag`; regular vs GPU by presence of `gpu` tag; exclude `Template == 1`.
- `currentWorkers` / `currentGPUWorkers` = `len(owned)` per type. Replaces `countWorkers` + `countInFlight`. Provisioning VMs (in Proxmox, not yet in K8s) now count, preventing over-provisioning while nodes join.
- VM tags at creation become: `cfg.AutoScalerTag` + (`gpu` for GPU) + `cfg.Tags`.

### 3. Config hot reload

- New `Config.AutoScalerTag string` field, read from `autoscaler_tag`, default `"talos"` when empty.
- Reconciler gains `configHash string`. After each `readConfig`, compute a stable hash of `cm.Data`; if it differs from the previous value, log `Config reloaded` and proceed (values apply immediately to the same reconcile). No new dependencies.

### 4. Scale logic

`scaleUp(ctx, desired int32, size VMSize, cfg *Config, workerType string, owned []proxmox.VM)`:
- nextIndex = `max(index of owned VMs of type, -1) + 1` where index is parsed from the name suffix after `{cluster}-{prefix}-`.
- For each new VM: name `{cluster}-{prefix}-{index}`, VMID `base + index`.
- Skip if name already present in `owned` (covers list→create race partially; the VMID conflict is the real backstop).
- Keep non-blocking goroutine create + `waitForNodeReady`. Remove InFlight guards and `FindVMByName` calls.

`scaleDown(ctx, desired int32, clusterName string, owned []proxmox.VM, baseVMID int)`:
- Sort owned by index desc; delete VMs until `len == desired`.
- **Guard:** skip a VM whose name has no matching K8s Node yet (still provisioning). Never delete a VM that hasn't joined the cluster.
- Descheduler cascade (`findEvictableNodes` → `drainAndDelete`) unchanged and already idempotent across replicas.

### 5. Files touched

| File | Change |
|---|---|
| `autoscaler/pkg/proxmox/client.go` | `VM` struct, `ListVMs` |
| `autoscaler/pkg/proxmox/client_test.go` | `TestListVMs` (parse tags/template/type; endpoint hit) |
| `autoscaler/pkg/autoscaler/controller.go` | remove InFlight/countInFlight/countWorkers; add owned-split, maxIndex, config hash, `AutoScalerTag` |
| `autoscaler/pkg/autoscaler/controller_test.go` | mocks gain `/api2/json/cluster/resources`; rework ScaleUp/ScaleDown/Reconcile tests; add tag-filter + hash tests |
| `kubernetes/deployment.yaml` | `replicas: 2` |
| `kubernetes/configmap.yaml` | `autoscaler_tag` key |
| `README.md`, `docs/ARCHITECTURE.md`, `docs/DEPLOYMENT.md` | document `autoscaler_tag`, HA, `Audit /vm/` requirement |

## Known Trade-offs

- Changing `autoscaler_tag` orphans existing VMs (apply-to-future-only). Operator re-tags manually. Documented in README.
- Two autoscalers pointed at the same Proxmox with the same `autoscaler_tag` will fight — tag must be unique per autoscaler.
- Concurrent replicas produce transient duplicate-create/delete API errors that are logged and self-heal next tick.

## Testing

- TDD: `TestListVMs` first (tags `,`/`;` split, template/type filtering, query param), then controller tests.
- Controller tests use `fake` clientset + httptest Proxmox mocks; mocks must serve `/api2/json/cluster/resources?type=vm` with tagged VM lists so owned counts drive scale decisions.
- No Go toolchain/golangci/gofmt available in this environment; verification relies on careful reading + `go vet` if the user provides a toolchain, plus CI (golangci-lint/gofmt) on push.
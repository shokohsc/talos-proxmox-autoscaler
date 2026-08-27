# Stateless HA + Tag-Based VM Tracking + Hot Config Reload Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the autoscaler run as two stateless replicas that make all scale decisions from the Proxmox VM tag list plus the ConfigMap, and hot-reload config changes.

**Architecture:** Replace per-pod in-memory in-flight tracking with Proxmox as the single shared source of truth. Every reconcile lists all cluster VMs (`/cluster/resources?type=vm`) once, filters VMs this autoscaler owns (exact tag match on the configurable `autoscaler_tag`, default `talos`), and scales up/down against that count with deterministic index/VMID/name generation. Config changes are detected by hashing the ConfigMap data and logged on change.

**Tech Stack:** Go 1.23 (no new dependencies), `k8s.io/client-go` fake clientset for tests, `net/http/httptest` for Proxmox mocks, GitHub Actions CI.

**Spec:** `docs/superpowers/specs/2026-08-27-stateless-ha-tag-tracking-design.md` (approved). The plan argues from this spec.

## Global Constraints

- Go toolchain / gofmt / golangci-lint are NOT installed in this working environment. Every "Run" step must be executed on a Go-capable machine (or CI via push — `.github/workflows/ci.yaml` runs `go vet`, `go test`, `golangci-lint`). Steps still spell the exact commands to run.
- Tag matching must be EXACT token match after splitting on `,` and `;` (and whitespace) — never substring (`hasTag("talosgpu", "gpu")` must be false).
- `autoscaler_tag` config key, default `"talos"` when empty. GPU marker tag stays fixed `"gpu"`.
- Config changes apply to future decisions only — never re-tag or reconfigure existing VMs.
- No new dependencies, no leader election, no informer/fsnotify. Keep the 30s poll.
- Exclude VMs with `template == 1` from ownership counts.
- Deployment replicas: 1 → 2.
- Commit after every task. Commit messages short, imperative (repo style: `docs:`, `feat:`).

---

### Task 1: Add `VM` struct and `ListVMs` to the Proxmox client

**Files:**
- Modify: `autoscaler/pkg/proxmox/client.go` (add struct + method near `ListNodes`, ~line 425)
- Test: `autoscaler/pkg/proxmox/client_test.go`

**Interfaces:**
- Produces: `type VM struct { VMID int; Name string; Node string; Status string; Type string; Template int; Tags string }` (json-tagged: `vmid`, `name`, `node`, `status`, `type`, `template`, `tags`) and `func (c *Client) ListVMs(ctx context.Context) ([]VM, error)` — returns all qemu+lxc VMs cluster-wide from `GET /api2/json/cluster/resources?type=vm`.

- [ ] **Step 1: Write the failing test**

Append to `autoscaler/pkg/proxmox/client_test.go`:

```go
func TestListVMs(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{"vmid": 100, "name": "vm-a", "node": "pve1", "status": "running", "type": "qemu", "template": 0, "tags": "talos,worker"},
				{"vmid": 200, "name": "vm-b", "node": "pve2", "status": "stopped", "type": "lxc", "template": 1, "tags": "talos;gpu"},
				{"vmid": 300, "name": "vm-c", "node": "pve3", "status": "running", "type": "qemu", "template": 0, "tags": ""},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "", true)
	require.NoError(t, err)

	vms, err := c.ListVMs(context.Background())
	require.NoError(t, err)
	require.Len(t, vms, 3)
	assert.Equal(t, "/api2/json/cluster/resources?type=vm", gotPath)
	assert.Equal(t, 100, vms[0].VMID)
	assert.Equal(t, "vm-a", vms[0].Name)
	assert.Equal(t, "pve1", vms[0].Node)
	assert.Equal(t, "qemu", vms[0].Type)
	assert.Equal(t, 0, vms[0].Template)
	assert.Equal(t, "talos,worker", vms[0].Tags)
	assert.Equal(t, "talos;gpu", vms[1].Tags)
	assert.Equal(t, 1, vms[1].Template)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/proxmox/ -run TestListVMs -v`
Expected: FAIL (compile error: `undefined: c.ListVMs`).

- [ ] **Step 3: Write minimal implementation**

Add to `autoscaler/pkg/proxmox/client.go` immediately before `func (c *Client) ListNodes`:

```go
// VM is a virtual machine or container visible cluster-wide (qemu + lxc).
type VM struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	Template int    `json:"template"`
	Tags     string `json:"tags"` // "," or ";" separated string
}

// ListVMs returns every VM in the cluster. The API token needs the Audit
// privilege on the /vm/ ACL path for tags to be visible.
func (c *Client) ListVMs(ctx context.Context) ([]VM, error) {
	data, err := c.do(ctx, "GET", "/api2/json/cluster/resources?type=vm", nil)
	if err != nil {
		return nil, err
	}
	var vms []VM
	if err := json.Unmarshal(data, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/proxmox/ -run TestListVMs -v`
Expected: PASS.

- [ ] **Step 5: Run the full package test suite**

Run: `go test ./pkg/...`
Expected: all PASS (existing tests untouched by this addition).

- [ ] **Step 6: Commit**

```bash
git add autoscaler/pkg/proxmox/client.go autoscaler/pkg/proxmox/client_test.go
git commit -m "feat(proxmox): add cluster-wide ListVMs"
```

---

### Task 2: `autoscaler_tag` config + ConfigMap reload detection

**Files:**
- Modify: `autoscaler/pkg/autoscaler/controller.go` (Config struct ~line 37, Reconciler struct ~line 57, readConfig ~line 163)
- Test: `autoscaler/pkg/autoscaler/controller_test.go`

**Interfaces:**
- Produces: `Config.AutoScalerTag string` (default `"talos"`); package func `configHash(data map[string]string) string` (deterministic — `json.Marshal` sorts map keys) used by readConfig, which logs `Config reloaded` when the hash changes. Reconciler gains private field `configHash string` (also set inside readConfig so tests can assert reload detection).

- [ ] **Step 1: Write the failing tests**

Append to `autoscaler/pkg/autoscaler/controller_test.go`:

```go
func TestReadConfigAutoScalerTag(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name":   "test",
			"autoscaler_tag": "mycluster",
		},
	}
	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "mycluster", cfg.AutoScalerTag)
}

func TestReadConfigAutoScalerTagDefault(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data:       map[string]string{"cluster_name": "test"},
	}
	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "talos", cfg.AutoScalerTag)
}

func TestConfigHashStable(t *testing.T) {
	a := configHash(map[string]string{"b": "2", "a": "1"})
	b := configHash(map[string]string{"a": "1", "b": "2"})
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, configHash(map[string]string{"a": "1"}))
}

func TestConfigReloadDetected(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data:       map[string]string{"cluster_name": "test", "min_workers": "1"},
	}
	client := fake.NewSimpleClientset(cm)
	r := &Reconciler{KubeClient: client, Namespace: "autoscaler-system"}
	ctx := context.Background()

	_, err := r.readConfig(ctx)
	require.NoError(t, err)
	firstHash := r.configHash

	cm.Data["min_workers"] = "2"
	_, err = client.CoreV1().ConfigMaps("autoscaler-system").Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = r.readConfig(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, firstHash, r.configHash)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/autoscaler/ -run 'TestReadConfigAutoScalerTag|TestConfigHash|TestConfigReloadDetected' -v`
Expected: FAIL (compile error: `Config has no field AutoScalerTag`, `undefined: configHash`).

- [ ] **Step 3: Write minimal implementation**

In `autoscaler/pkg/autoscaler/controller.go`:

Add field to `Config` struct (after `Tags string`):
```go
	AutoScalerTag string
```

Add private field to `Reconciler` struct (after `GPUPrefix string`):
```go
	configHash string
```

In `readConfig`, replace the `d := cm.Data` line with:
```go
	d := cm.Data

	tag := d["autoscaler_tag"]
	if tag == "" {
		tag = "talos"
	}

	if h := configHash(d); h != r.configHash {
		r.configHash = h
		zap.S().Infow("Config reloaded", "cluster", d["cluster_name"])
	}
```

In the `Config{...}` literal inside readConfig, add after `ClusterName`:
```go
		ClusterName:    d["cluster_name"],
		AutoScalerTag:  tag,
```

Add the `configHash` package function (near `atoiDefault` at file end):
```go
// configHash is stable across map iteration order: encoding/json sorts keys.
func configHash(data map[string]string) string {
	b, _ := json.Marshal(data)
	return string(b)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/autoscaler/ -run 'TestReadConfigAutoScalerTag|TestConfigHash|TestConfigReloadDetected' -v`
Expected: PASS.

- [ ] **Step 5: Run the existing ReadConfig test group**

Run: `go test ./pkg/autoscaler/ -run TestReadConfig -v`
Expected: PASS (existing tests unaffected).

- [ ] **Step 6: Commit**

```bash
git add autoscaler/pkg/autoscaler/controller.go autoscaler/pkg/autoscaler/controller_test.go
git commit -m "feat(autoscaler): autoscaler_tag config with reload detection"
```

---

### Task 3: Tag-based ownership filter helpers

**Files:**
- Modify: `autoscaler/pkg/autoscaler/controller.go`
- Test: `autoscaler/pkg/autoscaler/controller_test.go`

**Interfaces:**
- Produces (all package-level, no `r` receiver):
  - `func hasTag(tags, target string) bool` — exact token match, splits on `,`, `;`, and whitespace.
  - `func vmIndex(name, clusterName, prefix string) int` — suffix index after `{cluster}-{prefix}-`, or -1 if it doesn't match.
  - `func vmMatchesPrefix(name, clusterName, prefix string) bool` — prefix match with digit-boundary check (so `worker-vm-gpu-0` does NOT match prefix `worker-vm`).
  - `func filterOwned(vms []proxmox.VM, clusterName, prefix, autoscalerTag string, gpu bool) []proxmox.VM` — excludes `Template == 1`, keeps only VMs carrying `autoscalerTag` whose name matches `{cluster}-{prefix}-`, and whose GPU-ness (`hasTag(vm.Tags, "gpu")`) equals the `gpu` argument.

- [ ] **Step 1: Write the failing tests**

Append to `autoscaler/pkg/autoscaler/controller_test.go`:

```go
func TestHasTag(t *testing.T) {
	assert.True(t, hasTag("talos,gpu,worker", "gpu"))
	assert.True(t, hasTag("talos;gpu;worker", "gpu"))
	assert.True(t, hasTag("talos, gpu", "gpu"))
	assert.True(t, hasTag("gpu", "gpu"))
	assert.False(t, hasTag("talos,worker", "gpu"))
	assert.False(t, hasTag("talosgpu", "gpu"))  // no substring false positives
	assert.False(t, hasTag("", "talos"))
}

func TestVMIndex(t *testing.T) {
	assert.Equal(t, 0, vmIndex("test-worker-vm-0", "test", "worker-vm"))
	assert.Equal(t, 7, vmIndex("test-worker-vm-7", "test", "worker-vm"))
	assert.Equal(t, -1, vmIndex("test-worker-vm-gpu-0", "test", "worker-vm"))
	assert.Equal(t, -1, vmIndex("other-vm-0", "test", "worker-vm"))
	assert.Equal(t, -1, vmIndex("test-worker-vm-x", "test", "worker-vm"))
}

func TestFilterOwned(t *testing.T) {
	vms := []proxmox.VM{
		{VMID: 2000, Name: "test-worker-vm-0", Tags: "talos,worker"},
		{VMID: 2001, Name: "test-worker-vm-1", Tags: "talos;gpu"},
		{VMID: 3000, Name: "test-worker-vm-gpu-0", Tags: "talos,gpu"},
		{VMID: 2100, Name: "test-worker-vm-2", Tags: "talos", Template: 1},
		{VMID: 2500, Name: "test-worker-vm-3", Tags: "other"},
		{VMID: 2600, Name: "other-worker-vm-0", Tags: "talos"},
	}

	regular := filterOwned(vms, "test", "worker-vm", "talos", false)
	gpu := filterOwned(vms, "test", "worker-vm-gpu", "talos", true)

	assert.Len(t, regular, 1)
	assert.Equal(t, 2000, regular[0].VMID)
	assert.Len(t, gpu, 1)
	assert.Equal(t, 3000, gpu[0].VMID)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/autoscaler/ -run 'TestHasTag|TestVMIndex|TestFilterOwned' -v`
Expected: FAIL (compile error: `undefined: hasTag`, etc.).

- [ ] **Step 3: Write minimal implementation**

Add to `autoscaler/pkg/autoscaler/controller.go` (place near `findEvictableNodes`):

```go
func hasTag(tags, target string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	}) {
		if t == target {
			return true
		}
	}
	return false
}

func vmMatchesPrefix(name, clusterName, prefix string) bool {
	fullPrefix := clusterName + "-" + prefix + "-"
	if !strings.HasPrefix(name, fullPrefix) {
		return false
	}
	idx := len(fullPrefix)
	return idx == len(name) || (name[idx] >= '0' && name[idx] <= '9')
}

func vmIndex(name, clusterName, prefix string) int {
	suffix, ok := strings.CutPrefix(name, clusterName+"-"+prefix+"-")
	if !ok {
		return -1
	}
	var idx int
	if _, err := fmt.Sscanf(suffix, "%d", &idx); err != nil {
		return -1
	}
	return idx
}

func filterOwned(vms []proxmox.VM, clusterName, prefix, autoscalerTag string, gpu bool) []proxmox.VM {
	var owned []proxmox.VM
	for _, vm := range vms {
		if vm.Template == 1 {
			continue
		}
		if !hasTag(vm.Tags, autoscalerTag) || !vmMatchesPrefix(vm.Name, clusterName, prefix) {
			continue
		}
		if hasTag(vm.Tags, "gpu") != gpu {
			continue
		}
		owned = append(owned, vm)
	}
	return owned
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/autoscaler/ -run 'TestHasTag|TestVMIndex|TestFilterOwned' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add autoscaler/pkg/autoscaler/controller.go autoscaler/pkg/autoscaler/controller_test.go
git commit -m "feat(autoscaler): tag-based ownership filter helpers"
```

---

### Task 4: Stateless tag-driven reconcile + scale up/down

**Files:**
- Modify: `autoscaler/pkg/autoscaler/controller.go`
- Test: `autoscaler/pkg/autoscaler/controller_test.go`

**Interfaces:**
- Consumes: `proxmox.VM`, `proxmox.Client.ListVMs` (Task 1); `Config.AutoScalerTag`, `configHash` (Task 2); `filterOwned`, `hasTag`, `vmIndex` (Task 3).
- Produces:
  - `func (r *Reconciler) scaleUp(ctx context.Context, desired int32, size VMSize, cfg *Config, workerType string, owned []proxmox.VM)` — creates `desired - len(owned)` VMs at indices `maxIndex(owned)+1 ...`, names `{cluster}-{prefix}-{index}`, VMIDs `base+index`, tags `cfg.AutoScalerTag` (+`,gpu` for GPU) (+`,cfg.Tags`). Non-blocking goroutine: `CreateVM` + `waitForNodeReady`. No in-flight map, no `FindVMByName`.
  - `func (r *Reconciler) scaleDown(ctx context.Context, desired int32, clusterName, prefix string, baseVMID int, owned []proxmox.VM)` — deletes highest-index VMs first via `drainAndDelete`, skipping any whose name has no matching K8s node yet.
  - List helpers `vmMatchesPrefix`, `filterOwned`, `countWorkers` AND `countInFlight` are DELETED (`countWorkers`/`countInFlight`); `Reconciler.InFlight` + `inFlightMu` and the `sync` import are DELETED; `sort` import is ADDED.

- [ ] **Step 1: Rewrite the tests (compile-failing state = the red)**

In `autoscaler/pkg/autoscaler/controller_test.go`:

(a) DELETE `TestCountWorkers` and `TestCountWorkers_None` entirely (count now derives from Proxmox).

(b) REWRITE `TestScaleUp` — new signature, tag capture, no `InFlight`:

```go
func TestScaleUp(t *testing.T) {
	var createdVMCount atomic.Int32
	var tagValues []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agent/network-get-interfaces") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "eth0", "ip-addresses": []map[string]interface{}{
						{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
					}},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/status/start") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
			return
		}
		if r.Method == "POST" {
			_ = r.ParseForm()
			if r.FormValue("vmid") != "" {
				createdVMCount.Add(1)
				tagValues = append(tagValues, r.FormValue("tags"))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	readyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}

	r := &Reconciler{
		KubeClient:   fake.NewSimpleClientset(readyNode),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	cfg := &Config{
		ClusterName:   "test",
		AutoScalerTag: "talos",
		DiskGiB:       50,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		MACAddress:    "AA:BB:CC:DD:EE:FF",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r.scaleUp(ctx, 3, VMSize{CPU: 4, MemoryGiB: 8}, cfg, "vm", nil)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if int(createdVMCount.Load()) >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Equal(t, int32(3), createdVMCount.Load())
	require.Len(t, tagValues, 3)
	for _, tv := range tagValues {
		assert.Equal(t, "talos", tv)
	}
}
```

(c) Add `TestScaleUp_GPU` — exercises GPU tag plus PCI passthrough selection:

```go
func TestScaleUp_GPU(t *testing.T) {
	var createdVMCount atomic.Int32
	var tagValues []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agent/network-get-interfaces") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "eth0", "ip-addresses": []map[string]interface{}{
						{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
					}},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/status/start") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
			return
		}
		if r.Method == "POST" {
			_ = r.ParseForm()
			if r.FormValue("vmid") != "" {
				createdVMCount.Add(1)
				tagValues = append(tagValues, r.FormValue("tags"))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	readyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}

	r := &Reconciler{
		KubeClient:   fake.NewSimpleClientset(readyNode),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		BaseGPUVMID:  3000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	cfg := &Config{
		ClusterName:   "test",
		AutoScalerTag: "talos",
		DiskGiB:       50,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		GPUNodes:      []GPUNodeConfig{{PCIDevices: []proxmox.PCIDevice{{ID: "0000:01:00.0", PCIe: true, GPU: true}}}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r.scaleUp(ctx, 2, VMSize{CPU: 4, MemoryGiB: 8}, cfg, "gpu", nil)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if int(createdVMCount.Load()) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Equal(t, int32(2), createdVMCount.Load())
	require.Len(t, tagValues, 2)
	assert.Equal(t, "talos,gpu", tagValues[0])
}
```

(d) REWRITE `TestScaleDown` — owned list argument, plus a new guard-skip test:

```go
func TestScaleDown(t *testing.T) {
	var deletedVMIDs []int
	srv := newMockProxmoxServerBatch(t, &deletedVMIDs)
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-vm-2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-vm-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-vm-0"}},
	}

	r := &Reconciler{
		KubeClient:   fake.NewSimpleClientset(&corev1.NodeList{Items: nodes}),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	owned := []proxmox.VM{
		{VMID: 1002, Name: "test-cluster-worker-vm-2", Tags: "talos"},
		{VMID: 1001, Name: "test-cluster-worker-vm-1", Tags: "talos"},
		{VMID: 1000, Name: "test-cluster-worker-vm-0", Tags: "talos"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	r.scaleDown(ctx, 1, "test-cluster", "worker-vm", 1000, owned)

	assert.Len(t, deletedVMIDs, 2)
	assert.Contains(t, deletedVMIDs, 1002)
	assert.Contains(t, deletedVMIDs, 1001)
}

func TestScaleDown_SkipsUnregisteredNode(t *testing.T) {
	var deletedVMIDs []int
	srv := newMockProxmoxServerBatch(t, &deletedVMIDs)
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-vm-0"}},
	}

	r := &Reconciler{
		KubeClient:   fake.NewSimpleClientset(&corev1.NodeList{Items: nodes}),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	owned := []proxmox.VM{
		{VMID: 1002, Name: "test-cluster-worker-vm-2", Tags: "talos"},
		{VMID: 1001, Name: "test-cluster-worker-vm-1", Tags: "talos"},
		{VMID: 1000, Name: "test-cluster-worker-vm-0", Tags: "talos"},
	}

	r.scaleDown(context.Background(), 1, "test-cluster", "worker-vm", 1000, owned)

	assert.Equal(t, []int{1000}, deletedVMIDs)
}
```

(e) REWRITE `TestReconcile_ScaleUp` — add `/api2/json/cluster/resources` to the inline mock, drop `InFlight`, assert tags `talos`:

```go
func TestReconcile_ScaleUp(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name":     "test",
			"min_workers":      "1",
			"max_workers":      "5",
			"min_cpu":          "2",
			"max_cpu":          "4",
			"min_memory_gib":   "4",
			"max_memory_gib":   "8",
			"disk_gib":         "50",
			"storage_pool":     "local-lvm",
			"network_bridge":   "vmbr0",
			"mac_address":      "AA:BB:CC:DD:EE:FF",
			"worker_gpu_nodes": `[{"type":"p4","nodes":["worker-vm-gpu"],"pci_devices":[{"id":"0000:01:00.0","pcie":true,"gpu":true}]}]`,
		},
	}

	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			}}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Reason: "Unschedulable", Status: corev1.ConditionTrue}},
		},
	}

	var createdVMCount atomic.Int32
	var tagValues []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/cluster/resources" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}})
			return
		}
		if r.URL.Path == "/api2/json/nodes" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{{"node": "pve", "status": "online"}},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/agent/network-get-interfaces") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "eth0", "ip-addresses": []map[string]interface{}{
						{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
					}},
				},
			})
			return
		}
		if r.Method == "POST" {
			_ = r.ParseForm()
			if r.FormValue("vmid") != "" {
				createdVMCount.Add(1)
				tagValues = append(tagValues, r.FormValue("tags"))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	readyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}

	r := &Reconciler{
		KubeClient:   fake.NewSimpleClientset(cm, pendingPod, readyNode),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = r.reconcile(ctx)
	assert.NoError(t, err)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if createdVMCount.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, int(createdVMCount.Load()), 1)
	require.NotEmpty(t, tagValues)
	assert.Equal(t, "talos", tagValues[0])
}
```

(f) REWRITE `TestReconcile_ScaleDown` — mock now serves `/cluster/resources` with 3 tagged VMs; drop `InFlight`:

```go
func TestReconcile_ScaleDown(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name":   "test",
			"min_workers":    "1",
			"max_workers":    "5",
			"min_cpu":        "2",
			"max_cpu":        "4",
			"min_memory_gib": "4",
			"max_memory_gib": "8",
			"disk_gib":       "50",
			"storage_pool":   "local-lvm",
			"network_bridge": "vmbr0",
		},
	}

	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-2"}},
	}

	vms := []map[string]interface{}{
		vmRes(1000, "test-worker-vm-0", "talos", 0),
		vmRes(1001, "test-worker-vm-1", "talos", 0),
		vmRes(1002, "test-worker-vm-2", "talos", 0),
	}

	var deletedVMIDs []int
	srv := newMockProxmoxServerWithVMs(t, vms, &deletedVMIDs)
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	r := &Reconciler{
		KubeClient:   fake.NewSimpleClientset(cm, &corev1.NodeList{Items: nodes}),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = r.reconcile(ctx)
	assert.NoError(t, err)
	assert.Len(t, deletedVMIDs, 2)
	assert.Contains(t, deletedVMIDs, 1002)
	assert.Contains(t, deletedVMIDs, 1001)
}
```

(g) REWRITE `TestReconcile_NoAction` — mock serves `/nodes` + `/cluster/resources` (2 tagged VMs so current == min == 2) and fails on anything else; drop `InFlight`:

```go
func TestReconcile_NoAction(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name":   "test",
			"min_workers":    "2",
			"max_workers":    "5",
			"min_cpu":        "2",
			"max_cpu":        "4",
			"min_memory_gib": "4",
			"max_memory_gib": "8",
		},
	}

	vms := []map[string]interface{}{
		vmRes(1000, "test-worker-vm-0", "talos", 0),
		vmRes(1001, "test-worker-vm-1", "talos", 0),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/cluster/resources" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": vms})
			return
		}
		if r.URL.Path == "/api2/json/nodes" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{{"node": "pve", "status": "online"}},
			})
			return
		}
		t.Fatal("no proxmox calls expected beyond node resolution and VM listing")
	}))
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	r := &Reconciler{
		KubeClient:   fake.NewSimpleClientset(cm),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	err = r.reconcile(context.Background())
	assert.NoError(t, err)
}
```

(h) Add the shared mock helpers at the bottom of the file (next to `newMockProxmoxServerBatch`):

```go
func vmRes(vmid int, name, tags string, template int) map[string]interface{} {
	return map[string]interface{}{
		"vmid": vmid, "name": name, "node": "pve", "status": "running",
		"type": "qemu", "template": template, "tags": tags,
	}
}

func newMockProxmoxServerWithVMs(t *testing.T, vms []map[string]interface{}, deletedVMIDs *[]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/cluster/resources" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": vms})
			return
		}
		if r.URL.Path == "/api2/json/nodes" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{{"node": "pve", "status": "online"}},
			})
			return
		}
		if r.URL.Path == "/api2/json/access/ticket" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{"ticket": "t", "CSRFPreventionToken": "c"},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/status/stop") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
			return
		}
		if r.Method == "DELETE" && strings.Contains(r.URL.Path, "/qemu/") {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) >= 6 {
				var vmid int
				_, _ = fmt.Sscanf(parts[len(parts)-1], "%d", &vmid)
				*deletedVMIDs = append(*deletedVMIDs, vmid)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
}
```

- [ ] **Step 2: Run the package tests to verify they fail (compilation)**

Run: `go test ./pkg/autoscaler/`
Expected: FAIL to build (signature mismatch: `scaleUp`/`scaleDown` take new args, `InFlight` field removed, `filterOwned` still missing from impl).

- [ ] **Step 3: Implement the controller changes**

In `autoscaler/pkg/autoscaler/controller.go`:

(i) Imports: remove `"sync"`, add `"sort"` (keep `context`, `encoding/json`, `fmt`, `strconv`, `strings`, `time`).

(ii) `Reconciler` struct: delete the `InFlight`/`inFlightMu` block (lines ~66-70), leaving `configHash string` private field.

(iii) In `reconcile`, replace the block from `currentWorkers := r.countWorkers(...)` through `currentGPUWorkers += inFlightGPU` with:

```go
	vms, err := r.Proxmox.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("list vms: %w", err)
	}
	ownedRegular := filterOwned(vms, cfg.ClusterName, r.WorkerPrefix, cfg.AutoScalerTag, false)
	ownedGPU := filterOwned(vms, cfg.ClusterName, r.GPUPrefix, cfg.AutoScalerTag, true)
	currentWorkers := int32(len(ownedRegular))
	currentGPUWorkers := int32(len(ownedGPU))
```

(iv) Update the four scale call sites in `reconcile`. GPU branch: replace `r.scaleUp(ctx, currentGPUWorkers, gpuWorkersNeeded, gpuVMSize, cfg, "gpu")` with `r.scaleUp(ctx, gpuWorkersNeeded, gpuVMSize, cfg, "gpu", ownedGPU)` and `r.scaleDown(ctx, currentGPUWorkers, gpuWorkersNeeded, cfg.ClusterName, r.GPUPrefix, r.BaseGPUVMID)` with `r.scaleDown(ctx, gpuWorkersNeeded, cfg.ClusterName, r.GPUPrefix, r.BaseGPUVMID, ownedGPU)`, keeping the surrounding logging/intent untouched:

```go
		zap.S().Debugw("GPU Scale decision", "current", currentGPUWorkers, "needed", gpuWorkersNeeded, "vm_size", gpuVMSize)

		if gpuWorkersNeeded > currentGPUWorkers {
			zap.S().Infow("Scaling up GPU workers", "current", currentGPUWorkers, "desired", gpuWorkersNeeded, "size", gpuVMSize)
			r.scaleUp(ctx, gpuWorkersNeeded, gpuVMSize, cfg, "gpu", ownedGPU)
		} else if gpuWorkersNeeded < currentGPUWorkers && unschedulableCount == 0 {
			zap.S().Infow("Scaling down GPU workers", "current", currentGPUWorkers, "desired", gpuWorkersNeeded)
			r.scaleDown(ctx, gpuWorkersNeeded, cfg.ClusterName, r.GPUPrefix, r.BaseGPUVMID, ownedGPU)
		}
```

Regular branch: replace `r.scaleUp(ctx, currentWorkers, workersNeeded, vmSize, cfg, "vm")` with `r.scaleUp(ctx, workersNeeded, vmSize, cfg, "vm", ownedRegular)` and `r.scaleDown(ctx, currentWorkers, workersNeeded, cfg.ClusterName, r.WorkerPrefix, r.BaseVMID)` with `r.scaleDown(ctx, workersNeeded, cfg.ClusterName, r.WorkerPrefix, r.BaseVMID, ownedRegular)`:

```go
	zap.S().Debugw("Scale decision", "current", currentWorkers, "needed", workersNeeded, "vm_size", vmSize)

	if workersNeeded > currentWorkers {
		zap.S().Infow("Scaling up", "current", currentWorkers, "desired", workersNeeded, "size", vmSize)
		r.scaleUp(ctx, workersNeeded, vmSize, cfg, "vm", ownedRegular)
	} else if workersNeeded < currentWorkers && unschedulableCount == 0 {
		zap.S().Infow("Scaling down", "current", currentWorkers, "desired", workersNeeded)
		r.scaleDown(ctx, workersNeeded, cfg.ClusterName, r.WorkerPrefix, r.BaseVMID, ownedRegular)
	}
```

(v) REPLACE `scaleUp` entirely with:

```go
func (r *Reconciler) scaleUp(ctx context.Context, desired int32, size VMSize, cfg *Config, workerType string, owned []proxmox.VM) {
	prefix := r.WorkerPrefix
	baseVMID := r.BaseVMID
	if workerType == "gpu" {
		prefix = r.GPUPrefix
		baseVMID = r.BaseGPUVMID
	}

	existing := make(map[string]bool, len(owned))
	nextIndex := -1
	for _, vm := range owned {
		existing[vm.Name] = true
		if idx := vmIndex(vm.Name, cfg.ClusterName, prefix); idx > nextIndex {
			nextIndex = idx
		}
	}

	createCount := len(owned)
	for i := 0; createCount < int(desired); i++ {
		index := nextIndex + 1 + i
		vmid := baseVMID + index
		vmName := fmt.Sprintf("%s-%s-%d", cfg.ClusterName, prefix, index)
		if existing[vmName] {
			continue
		}
		createCount++
		zap.S().Infow("Creating worker VM", "name", vmName, "vmid", vmid, "type", workerType)

		var pciDevices []proxmox.PCIDevice
		if workerType == "gpu" {
			for _, gpuNode := range cfg.GPUNodes {
				if len(gpuNode.PCIDevices) > 0 {
					pciDevices = gpuNode.PCIDevices
					break
				}
			}
		}

		tags := cfg.AutoScalerTag
		if workerType == "gpu" {
			tags += ",gpu"
		}
		if cfg.Tags != "" {
			tags += "," + cfg.Tags
		}

		go func(vmName string, vmid int, pciDevices []proxmox.PCIDevice) {
			ip, err := r.Proxmox.CreateVM(ctx, proxmox.VMConfig{
				Name:          vmName,
				VMID:          vmid,
				VCPU:          int32(size.CPU),
				MemoryMiB:     int32(size.MemoryGiB) * 1024,
				DiskGiB:       cfg.DiskGiB,
				StoragePool:   cfg.StoragePool,
				NetworkBridge: cfg.NetworkBridge,
				MACAddress:    cfg.MACAddress,
				Serial:        cfg.Serial,
				CPUType:       cfg.CPUType,
				Tags:          tags,
				VLANID:        cfg.VLANID,
				PCIDevices:    pciDevices,
			})
			if err != nil {
				zap.S().Errorw("Failed to create VM", "error", err, "vmid", vmid)
				return
			}
			if err := r.waitForNodeReady(ctx, ip); err != nil {
				zap.S().Errorw("Node not ready after provisioning", "error", err, "ip", ip)
			}
		}(vmName, vmid, pciDevices)
	}
}
```

(vi) REPLACE `scaleDown` entirely with:

```go
func (r *Reconciler) scaleDown(ctx context.Context, desired int32, clusterName, prefix string, baseVMID int, owned []proxmox.VM) {
	sort.Slice(owned, func(i, j int) bool {
		return vmIndex(owned[i].Name, clusterName, prefix) > vmIndex(owned[j].Name, clusterName, prefix)
	})

	nodeList, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		zap.S().Errorw("Failed to list nodes for scale-down", "error", err)
		return
	}
	nodeNames := make(map[string]bool, len(nodeList.Items))
	for _, n := range nodeList.Items {
		nodeNames[n.Name] = true
	}

	need := int32(len(owned)) - desired
	deleted := int32(0)
	for _, vm := range owned {
		if deleted >= need {
			break
		}
		if !nodeNames[vm.Name] {
			zap.S().Infow("Skipping scale-down, node not registered yet", "vm", vm.Name)
			continue
		}
		zap.S().Infow("Removing worker node", "node", vm.Name)
		r.drainAndDelete(ctx, vm.Name, clusterName, prefix, baseVMID)
		deleted++
	}
}
```

(vii) DELETE the `countWorkers` and `countInFlight` functions entirely.

- [ ] **Step 4: Run the package tests to verify they pass**

Run: `go test ./pkg/autoscaler/`
Expected: PASS (all reworked and previously passing tests green).

- [ ] **Step 5: Run the full test suite**

Run: `go test ./pkg/...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add autoscaler/pkg/autoscaler/controller.go autoscaler/pkg/autoscaler/controller_test.go
git commit -m "feat(autoscaler): stateless tag-driven scale decisions"
```

---

### Task 5: Two replicas + `autoscaler_tag` in manifests

**Files:**
- Modify: `autoscaler/main.go` (line ~80: `InFlight: make(map[string]bool),` — DELETE)
- Modify: `kubernetes/deployment.yaml` (`replicas: 1` → `2`)
- Modify: `kubernetes/configmap.yaml` (add `autoscaler_tag` key)

**Interfaces:**
- Consumes: `Reconciler` no longer has the `InFlight` field (Task 4). `Config.AutoScalerTag` is the source of the ownership tag.

- [ ] **Step 1: Remove the InFlight field from the Reconciler literal**

In `autoscaler/main.go`, delete the line:
```go
		InFlight:     make(map[string]bool),
```

- [ ] **Step 2: Set replicas to 2**

In `kubernetes/deployment.yaml`, change:
```yaml
  replicas: 1
```
to:
```yaml
  replicas: 2
```

- [ ] **Step 3: Add the ownership-tag key**

In `kubernetes/configmap.yaml`, add beneath `cluster_name: "talos"`:
```yaml
  autoscaler_tag: "talos"
```

- [ ] **Step 4: Verify the Go code still builds**

Run: `go build ./...`
Expected: PASS (no new symbols referenced; `InFlight` removal compiles).

- [ ] **Step 5: Sanity-check YAML**

Run: `kubectl kubeconform -strict -summary -ignore-missing-schemas kubernetes/ 2>/dev/null || python3 -c "import yaml,sys; [yaml.safe_load(open(f)) for f in ['kubernetes/deployment.yaml','kubernetes/configmap.yaml']]; print('yaml ok')"`
Expected: prints `yaml ok` (kubeconform runs in CI via security.yaml if unavailable locally).

- [ ] **Step 6: Commit**

```bash
git add autoscaler/main.go kubernetes/deployment.yaml kubernetes/configmap.yaml
git commit -m "chore: run two stateless replicas with autoscaler_tag config"
```

---

### Task 6: Docs — stateless HA, `autoscaler_tag`, `Audit /vm/` permission

**Files:**
- Modify: `README.md` (features list ~line 40, ConfigMap reference ~line 129, add HA note)
- Modify: `docs/ARCHITECTURE.md` (Go types ~line 175, client function list ~line 83, tags paragraph ~line 101, High Availability ~line 302)
- Modify: `docs/DEPLOYMENT.md` (API user permissions ~line 134, ConfigMap snippet ~line 211)

**Interfaces:**
- Consumes: the code/behavior produced by Tasks 1-5 (verbatim naming and values).

- [ ] **Step 1: README.md — features list**

Replace the line:
```markdown
- **VM tags** — every VM gets a `talos` tag; GPU workers additionally get `gpu`; ConfigMap `tags` field appended
```
with:
```markdown
- **Tag-based ownership** — every VM gets the configurable `autoscaler_tag` (default `talos`); GPU workers additionally get `gpu`; ConfigMap `tags` field appended. Scale decisions derive from the Proxmox VM list, not pod memory, so replicas are stateless
- **Stateless HA** — concurrent + idempotent: run multiple replicas (`replicas: 2`); all decode from the same ConfigMap and Proxmox truth, so racing duplicate creates/deletes log harmlessly and converge next tick
- **Hot config reload** — ConfigMap changes are detected (hash) and applied to future scale decisions within 30s
```

- [ ] **Step 2: README.md — ConfigMap reference**

In the `data:` block of the ConfigMap reference, add beneath `cluster_name:`:
```yaml
  autoscaler_tag: "talos"          # Ownership tag for VMs this autoscaler manages
```
and change the `tags:` comment so it reads:
```yaml
  tags: "autoscaler,worker"      # Extra tags appended to provisioned VMs
```

- [ ] **Step 3: README.md — trade-off note**

Add a short subsection after the ConfigMap reference block (before `## Verify`):
```markdown
### Ownership Tag Caveats

- `autoscaler_tag` applies to future VMs only — changing it orphans existing VMs (re-tag manually), and every autoscaler pointed at the same Proxmox must use a distinct tag.
- The Proxmox API token needs the `Audit` privilege on `/vm/` for `ListVMs` to see guest tags.
```

- [ ] **Step 4: docs/ARCHITECTURE.md — Go types, client functions, tags, HA**

(a) In the `Config` struct listing add `AutoScalerTag` after `Tags`:
```go
    Tags          string
    AutoScalerTag string
```
(b) In the client function list add after `FindVMByName`:
```
  │  ├── ListVMs  (cluster-wide VM list, tag-based ownership)
```
(c) Replace the tags sentence in the VM Provisioning Flow (line ~101):
> `ConfigMap values (CPU type, CPU count, RAM, disk, network, and tags — all VMs get a default \`talos\` tag, GPU workers additionally get \`gpu\`, and ConfigMap tags are appended)`
with:
> `ConfigMap values (CPU type, CPU count, RAM, disk, network, and tags — all VMs get the \`autoscaler_tag\` (default \`talos\`), GPU workers additionally get \`gpu\`, and ConfigMap \`tags\` are appended)`
(d) In the High Availability section add a bullet:
```markdown
- **Stateless replicas** — 2+ concurrent pods reconcile every 30s with no leader election; ownership counts come from the Proxmox VM tag list so all replicas agree
```

- [ ] **Step 5: docs/DEPLOYMENT.md — API user permissions**

In the permission list (Step 4, bullet 7) add:
```markdown
   - `/vm` — `PVEAudit` (required so `ListVMs` returns guests with their tags)
```
And update the token test curl by appending a `ListVMs` probe:
```bash
# Test the token
curl -s -H "Authorization: PVEAPIToken=autoscaler@pve!autoscaler=${PROXMOX_API_TOKEN_SECRET}" \
  "https://10.0.1.10:8006/api2/json/cluster/resources?type=vm" | jq '.data[].name'
```

- [ ] **Step 6: docs/DEPLOYMENT.md — ConfigMap snippet**

In the Step 6 ConfigMap example, beneath `cluster_name:` add:
```yaml
  # Ownership tag for VMs this autoscaler manages (default "talos")
  autoscaler_tag: "talos"
```
and change the `tags:` comment to:
```yaml
  # Optional: extra tags appended to provisioned VMs (all VMs get autoscaler_tag, GPU VMs also get "gpu")
  tags: "autoscaler,worker"
```

- [ ] **Step 7: Review the full diff for accuracy**

Run: `git diff --stat && git diff | grep -E '^[+-].*(talos|replicas|AutoscalerTag|/vm)'`
Expected: all changed hunks reference the new `autoscaler_tag`, `replicas: 2`, `AutoScalerTag`, or `/vm` only (no stray edits).

- [ ] **Step 8: Commit**

```bash
git add README.md docs/ARCHITECTURE.md docs/DEPLOYMENT.md
git commit -m "docs: stateless HA, autoscaler_tag, Audit /vm/ requirement"
```

---

## Post-Plan Verification

After all tasks: push the branch and confirm `.github/workflows/ci.yaml` goes green (`go fmt`, `go vet`, `go test ./...`, `golangci-lint`, build). No Go toolchain exists in this environment, so green CI is the completion gate.
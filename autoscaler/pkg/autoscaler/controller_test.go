package autoscaler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/talos-proxmox-autoscaler/pkg/proxmox"
)

func TestAtoiDefault(t *testing.T) {
	assert.Equal(t, 5, atoiDefault("5", 0))
	assert.Equal(t, 0, atoiDefault("", 0))
	assert.Equal(t, 3, atoiDefault("abc", 3))
}

func TestClamp(t *testing.T) {
	assert.Equal(t, int32(5), clamp(5, 1, 10))
	assert.Equal(t, int32(1), clamp(0, 1, 10))
	assert.Equal(t, int32(10), clamp(15, 1, 10))
	assert.Equal(t, int32(5), clamp(5, 5, 5))
}

func TestInt64Ptr(t *testing.T) {
	p := int64Ptr(42)
	require.NotNil(t, p)
	assert.Equal(t, int64(42), *p)
}

func TestCalculateNeeded(t *testing.T) {
	r := &Reconciler{}
	cfg := &Config{MinWorkers: 1, MaxCPU: 4, MaxMemoryGiB: 8}

	t.Run("no pending pods returns min workers with min size", func(t *testing.T) {
		got, size := r.calculateNeeded(resource.Quantity{}, resource.Quantity{}, 0, cfg)
		assert.Equal(t, int32(1), got)
		assert.Equal(t, VMSize{CPU: 0, MemoryGiB: 0}, size)
	})

	t.Run("no pending pods uses min config values", func(t *testing.T) {
		cfgMin := &Config{MinWorkers: 1, MinCPU: 2, MaxCPU: 4, MinMemoryGiB: 4, MaxMemoryGiB: 8}
		got, size := r.calculateNeeded(resource.Quantity{}, resource.Quantity{}, 0, cfgMin)
		assert.Equal(t, int32(1), got)
		assert.Equal(t, VMSize{CPU: 2, MemoryGiB: 4}, size)
	})

	t.Run("cpu-driven scaling", func(t *testing.T) {
		pendingCPU := resource.MustParse("20m")
		got, _ := r.calculateNeeded(pendingCPU, resource.Quantity{}, 1, cfg)
		assert.Equal(t, int32(1), got)
	})

	t.Run("mem-driven scaling", func(t *testing.T) {
		pendingMem := resource.MustParse("20Gi")
		got, _ := r.calculateNeeded(resource.Quantity{}, pendingMem, 1, cfg)
		assert.Equal(t, int32(3), got)
	})

	t.Run("takes max of cpu and mem", func(t *testing.T) {
		pendingCPU := resource.MustParse("32")
		pendingMem := resource.MustParse("64Gi")
		got, _ := r.calculateNeeded(pendingCPU, pendingMem, 10, cfg)
		assert.Equal(t, int32(8), got)
	})
}

func TestCalculateVMSize(t *testing.T) {
	config := &Config{
		MinWorkers:   1,
		MaxWorkers:   10,
		MinCPU:       2,
		MaxCPU:       8,
		MinMemoryGiB: 4,
		MaxMemoryGiB: 16,
	}

	size := calculateVMSize(20, 40, 3, config)
	assert.Equal(t, 7, size.CPU)
	assert.Equal(t, 14, size.MemoryGiB)
}

func TestCalculateVMSize_Clamps(t *testing.T) {
	config := &Config{
		MinWorkers:   1,
		MaxWorkers:   10,
		MinCPU:       2,
		MaxCPU:       8,
		MinMemoryGiB: 4,
		MaxMemoryGiB: 16,
	}

	size := calculateVMSize(100, 100, 1, config)
	assert.Equal(t, 8, size.CPU)
	assert.Equal(t, 16, size.MemoryGiB)
}

func TestCalculateVMSize_ClampsToMin(t *testing.T) {
	config := &Config{
		MinCPU:       4,
		MaxCPU:       8,
		MinMemoryGiB: 8,
		MaxMemoryGiB: 16,
	}

	size := calculateVMSize(2, 2, 10, config)
	assert.Equal(t, 4, size.CPU)
	assert.Equal(t, 8, size.MemoryGiB)
}

func TestReadConfig(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name":     "test-cluster",
			"min_workers":      "2",
			"max_workers":      "5",
			"min_cpu":          "2",
			"max_cpu":          "16",
			"min_memory_gib":   "4",
			"max_memory_gib":   "32",
			"disk_gib":         "100",
			"storage_pool":     "local-lvm",
			"network_bridge":   "vmbr0",
			"mac_address":      "AA:BB:CC:DD:EE:FF",
			"serial":           "socket",
			"worker_gpu_nodes": `[{"pci_devices":[{"id":"0000:01:00.0","pcie":true,"gpu":true}]},{"pci_devices":[{"id":"0000:41:00.0","pcie":true,"gpu":true}]}]`,
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "test-cluster", cfg.ClusterName)
	assert.Equal(t, 16, cfg.MaxCPU)
	assert.Equal(t, 32, cfg.MaxMemoryGiB)
	assert.Equal(t, "vmbr0", cfg.NetworkBridge)
	require.Len(t, cfg.GPUNodes, 2)
	require.Len(t, cfg.GPUNodes[0].PCIDevices, 1)
	assert.Equal(t, "0000:01:00.0", cfg.GPUNodes[0].PCIDevices[0].ID)
	assert.Equal(t, "0000:41:00.0", cfg.GPUNodes[1].PCIDevices[0].ID)
}

func TestReadConfigTags(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "test",
			"tags":         "autoscaler,worker,v1",
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "autoscaler,worker,v1", cfg.Tags)
}

func TestReadConfigPCIDevicesMalformed(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name":     "test",
			"worker_gpu_nodes": `not valid json`,
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	_, err := r.readConfig(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker_gpu_nodes")
}

func TestReadConfigMissingClusterName(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"max_cpu": "8",
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	_, err := r.readConfig(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster_name")
}

func TestReadConfigVLANID(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "test",
			"vlan_id":      "100",
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 100, cfg.VLANID)
}

func TestReadConfigVLANIDDefault(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "test",
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.VLANID)
}

func TestAggregatePending(t *testing.T) {
	pendingUnschedulable := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Reason: "Unschedulable", Status: corev1.ConditionTrue},
			},
		},
	}
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pendingScheduled := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod3", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(pendingUnschedulable, runningPod, pendingScheduled)}
	cpu, mem, gpu, count, err := r.aggregatePending(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, 0, gpu)
	assert.Equal(t, "500m", cpu.String())
	assert.Equal(t, "512Mi", mem.String())
}

func TestAggregatePending_WithGPU(t *testing.T) {
	gpuPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "gpu-app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
							"nvidia.com/gpu":      resource.MustParse("1"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Reason: "Unschedulable", Status: corev1.ConditionTrue},
			},
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(gpuPod)}
	cpu, mem, gpu, count, err := r.aggregatePending(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, 1, gpu)
	assert.Equal(t, "2", cpu.String())
	assert.Equal(t, "4Gi", mem.String())
}

func TestAggregatePending_NoPods(t *testing.T) {
	r := &Reconciler{KubeClient: fake.NewSimpleClientset()}
	cpu, mem, gpu, count, err := r.aggregatePending(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Equal(t, 0, gpu)
	assert.True(t, cpu.IsZero())
	assert.True(t, mem.IsZero())
}

func TestFindEvictableNodes(t *testing.T) {
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-cluster-worker-vm-0",
				Labels: map[string]string{
					deschedulerLabel: "true",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "test-cluster-worker-vm-1",
				Labels: map[string]string{},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-cluster-worker-vm-gpu-0",
				Labels: map[string]string{
					deschedulerLabel: "true",
				},
			},
		},
	}

	r := &Reconciler{WorkerPrefix: "worker-vm", GPUPrefix: "worker-vm-gpu"}
	evictable := r.findEvictableNodes(nodes, "test-cluster")
	assert.Len(t, evictable, 2)
	assert.Equal(t, "test-cluster-worker-vm-0", evictable[0].Name)
	assert.Equal(t, "test-cluster-worker-vm-gpu-0", evictable[1].Name)
}

func TestFindEvictableNodes_None(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-vm-0", Labels: map[string]string{}}},
	}

	r := &Reconciler{WorkerPrefix: "worker-vm", GPUPrefix: "worker-vm-gpu"}
	evictable := r.findEvictableNodes(nodes, "test-cluster")
	assert.Empty(t, evictable)
}

func TestDrainNode(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
		Spec:       corev1.NodeSpec{Unschedulable: false},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "worker-0"},
	}
	kubeSystemPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
		Spec:       corev1.PodSpec{NodeName: "worker-0"},
	}

	client := fake.NewSimpleClientset(node, pod, kubeSystemPod)
	r := &Reconciler{KubeClient: client}
	r.drainNode(context.Background(), "worker-0")

	updatedNode, err := client.CoreV1().Nodes().Get(context.Background(), "worker-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, updatedNode.Spec.Unschedulable)

	podList, err := client.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, podList.Items)

	systemPodList, err := client.CoreV1().Pods("kube-system").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, systemPodList.Items, 1)
}

func TestDrainAndDelete(t *testing.T) {
	var deletedVMID int
	srv := newMockProxmoxServer(t, &deletedVMID)
	defer srv.Close()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-vm-0"},
	}
	kubeClient := fake.NewSimpleClientset(node)

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	r := &Reconciler{
		KubeClient:   kubeClient,
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	r.drainAndDelete(context.Background(), "test-cluster-worker-vm-0", 1000)

	assert.Equal(t, 1000, deletedVMID)
	updatedNode, err := kubeClient.CoreV1().Nodes().Get(context.Background(), "test-cluster-worker-vm-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, updatedNode.Spec.Unschedulable)
}

func TestScaleUp(t *testing.T) {
	var createdVMCount atomic.Int32
	var tagValues []string
	var tagMu sync.Mutex
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
				tagMu.Lock()
				tagValues = append(tagValues, r.FormValue("tags"))
				tagMu.Unlock()
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
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
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
	tagMu.Lock()
	defer tagMu.Unlock()
	require.Len(t, tagValues, 3)
	for _, tv := range tagValues {
		assert.Equal(t, "talos", tv)
	}
}

func TestScaleUp_GPU(t *testing.T) {
	var createdVMCount atomic.Int32
	var tagValues []string
	var tagMu sync.Mutex
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
				tagMu.Lock()
				tagValues = append(tagValues, r.FormValue("tags"))
				tagMu.Unlock()
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
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
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
	tagMu.Lock()
	defer tagMu.Unlock()
	require.Len(t, tagValues, 2)
	assert.Equal(t, "talos,gpu", tagValues[0])
}

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

func TestWaitForNodeReady_AlreadyReady(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "new-worker"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(node)}
	err := r.waitForNodeReady(context.Background(), "10.0.0.5")
	assert.NoError(t, err)
}

func TestWaitForNodeReady_Timeout(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "new-worker"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(node)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := r.waitForNodeReady(ctx, "10.0.0.5")
	assert.Error(t, err)
}

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
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Reason: "Unschedulable", Status: corev1.ConditionTrue}},
		},
	}

	var createdVMCount atomic.Int32
	var tagValues []string
	var tagMu sync.Mutex
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
				tagMu.Lock()
				tagValues = append(tagValues, r.FormValue("tags"))
				tagMu.Unlock()
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
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
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
	tagMu.Lock()
	defer tagMu.Unlock()
	require.NotEmpty(t, tagValues)
	assert.Equal(t, "talos", tagValues[0])
}

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

	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-1"}},
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
		KubeClient:   fake.NewSimpleClientset(cm, &corev1.NodeList{Items: nodes}),
		Proxmox:      proxmoxClient,
		BaseVMID:     1000,
		Namespace:    "autoscaler-system",
		WorkerPrefix: "worker-vm",
		GPUPrefix:    "worker-vm-gpu",
	}

	err = r.reconcile(context.Background())
	assert.NoError(t, err)
}

// --- helpers ---

func newTestProxmoxClient(baseURL string) (*proxmox.Client, error) {
	return proxmox.NewClient(baseURL, "", "", "user@realm!tok", "secret", "pve", true)
}

func newMockProxmoxServer(t *testing.T, deletedVMID *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				_, _ = fmt.Sscanf(parts[len(parts)-1], "%d", deletedVMID)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
}

func newMockProxmoxServerBatch(t *testing.T, deletedVMIDs *[]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestReadConfigRejectsGPUTag(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name":   "test",
			"autoscaler_tag": "gpu",
		},
	}
	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	_, err := r.readConfig(context.Background())
	require.Error(t, err)
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

func TestHasTag(t *testing.T) {
	assert.True(t, hasTag("talos,gpu,worker", "gpu"))
	assert.True(t, hasTag("talos;gpu;worker", "gpu"))
	assert.True(t, hasTag("talos, gpu", "gpu"))
	assert.True(t, hasTag("gpu", "gpu"))
	assert.False(t, hasTag("talos,worker", "gpu"))
	assert.False(t, hasTag("talosgpu", "gpu")) // no substring false positives
	assert.False(t, hasTag("", "talos"))
}

func TestCountK8sNodes(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-vm-gpu-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "other-worker-vm-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "control-plane-0"}},
	}

	assert.Equal(t, int32(2), countK8sNodes(nodes, "test", "worker-vm"))
	assert.Equal(t, int32(1), countK8sNodes(nodes, "test", "worker-vm-gpu"))
	assert.Equal(t, int32(0), countK8sNodes(nodes, "test", "nonexistent"))
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

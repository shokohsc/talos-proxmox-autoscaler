package autoscaler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

	t.Run("no pending pods returns min workers", func(t *testing.T) {
		got, _ := r.calculateNeeded(resource.Quantity{}, resource.Quantity{}, 0, cfg)
		assert.Equal(t, int32(1), got)
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
	assert.Equal(t, 7, size.CPU)        // 20/3 → ceiling = 7
	assert.Equal(t, 14, size.MemoryGiB) // 40/3 → ceiling = 14
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

func TestReadConfigNewKeys(t *testing.T) {
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
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "test-cluster", cfg.ClusterName)
	assert.Equal(t, 16, cfg.MaxCPU)
	assert.Equal(t, 32, cfg.MaxMemoryGiB)
	assert.Equal(t, "vmbr0", cfg.NetworkBridge)
}

func TestReadConfigBackwardCompat(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "legacy",
			"vcpu":         "4",
			"memory_gib":   "8",
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "legacy", cfg.ClusterName)
	assert.Equal(t, 4, cfg.MaxCPU)
	assert.Equal(t, 8, cfg.MaxMemoryGiB)
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

func TestReadConfigPCIDevices(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "test",
			"pci_devices":  `[{"id":"0000:01:00.0","pcie":true,"gpu":true}]`,
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)
	require.Len(t, cfg.PCIDevices, 1)
	assert.Equal(t, "0000:01:00.0", cfg.PCIDevices[0].ID)
	assert.True(t, cfg.PCIDevices[0].PCIe)
	assert.True(t, cfg.PCIDevices[0].GPU)
}

func TestReadConfigPCIDevicesMalformed(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "test",
			"pci_devices":  `not valid json`,
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	_, err := r.readConfig(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pci_devices")
}

func TestReadConfigPCIDevicesEmpty(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "test",
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cfg.PCIDevices)
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

func TestReadConfigNewKeysOverrideLegacy(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-config", Namespace: "autoscaler-system"},
		Data: map[string]string{
			"cluster_name": "mixed",
			"vcpu":         "2",
			"max_cpu":      "16",
			"memory_gib":   "4",
			"max_memory_gib": "32",
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm), Namespace: "autoscaler-system"}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 16, cfg.MaxCPU)
	assert.Equal(t, 32, cfg.MaxMemoryGiB)
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
	cpu, mem, count, err := r.aggregatePending(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, count) // only the unschedulable pod
	assert.Equal(t, "500m", cpu.String())
	assert.Equal(t, "512Mi", mem.String())
}

func TestAggregatePending_NoPods(t *testing.T) {
	r := &Reconciler{KubeClient: fake.NewSimpleClientset()}
	cpu, mem, count, err := r.aggregatePending(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.True(t, cpu.IsZero())
	assert.True(t, mem.IsZero())
}

func TestCountWorkers(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-control-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "other-worker-0"}},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(&corev1.NodeList{Items: nodes})}
	count := r.countWorkers(context.Background(), "test-cluster")
	assert.Equal(t, int32(2), count)
}

func TestCountWorkers_None(t *testing.T) {
	r := &Reconciler{KubeClient: fake.NewSimpleClientset(&corev1.NodeList{})}
	count := r.countWorkers(context.Background(), "test-cluster")
	assert.Equal(t, int32(0), count)
}

func TestFindEvictableNodes(t *testing.T) {
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-cluster-worker-0",
				Labels: map[string]string{
					deschedulerLabel: "true",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "test-cluster-worker-1",
				Labels: map[string]string{},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-cluster-worker-2",
				Labels: map[string]string{
					deschedulerLabel: "true",
				},
			},
		},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(&corev1.NodeList{Items: nodes})}
	evictable, err := r.findEvictableNodes(context.Background(), "test-cluster")
	require.NoError(t, err)
	assert.Len(t, evictable, 2)
	assert.Equal(t, "test-cluster-worker-0", evictable[0].Name)
	assert.Equal(t, "test-cluster-worker-2", evictable[1].Name)
}

func TestFindEvictableNodes_None(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-0", Labels: map[string]string{}}},
	}

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(&corev1.NodeList{Items: nodes})}
	evictable, err := r.findEvictableNodes(context.Background(), "test-cluster")
	require.NoError(t, err)
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

	// Verify node is cordoned
	updatedNode, err := client.CoreV1().Nodes().Get(context.Background(), "worker-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, updatedNode.Spec.Unschedulable)

	// Verify app pod was deleted
	podList, err := client.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, podList.Items)

	// Verify kube-system pod was NOT deleted
	systemPodList, err := client.CoreV1().Pods("kube-system").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, systemPodList.Items, 1)
}

func TestDrainAndDelete(t *testing.T) {
	var deletedVMID int
	// Start a fake Proxmox API server
	srv := newMockProxmoxServer(t, &deletedVMID)
	defer srv.Close()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-0"},
	}
	kubeClient := fake.NewSimpleClientset(node)

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	r := &Reconciler{
		KubeClient: kubeClient,
		Proxmox:    proxmoxClient,
		BaseVMID:   1000,
		Namespace:  "autoscaler-system",
	}

	r.drainAndDelete(context.Background(), "test-cluster-worker-0", "test-cluster")

	// VM index 0 → VMID 1000 + 0 = 1000
	assert.Equal(t, 1000, deletedVMID)
	// Node should be cordoned
	updatedNode, err := kubeClient.CoreV1().Nodes().Get(context.Background(), "test-cluster-worker-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, updatedNode.Spec.Unschedulable)
}

func TestScaleUp(t *testing.T) {
	var createdVMs []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/access/ticket" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{"ticket": "t", "CSRFPreventionToken": "c"},
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
		if strings.Contains(r.URL.Path, "/status/start") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
			return
		}
		_ = r.ParseForm()
		if vmid := r.FormValue("vmid"); vmid != "" {
			var v int
			_, _ = fmt.Sscanf(vmid, "%d", &v)
			createdVMs = append(createdVMs, v)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	// Pre-create a node with IP 10.0.0.5 and Ready status so waitForNodeReady returns immediately
	readyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	r := &Reconciler{
		KubeClient: fake.NewSimpleClientset(readyNode),
		Proxmox:    proxmoxClient,
		BaseVMID:   1000,
		Namespace:  "autoscaler-system",
	}

	cfg := &Config{
		ClusterName:   "test",
		DiskGiB:       50,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		MACAddress:    "AA:BB:CC:DD:EE:FF",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r.scaleUp(ctx, 0, 3, VMSize{CPU: 4, MemoryGiB: 8}, cfg)

	require.Len(t, createdVMs, 3)
	assert.Equal(t, 1000, createdVMs[0])
	assert.Equal(t, 1001, createdVMs[1])
	assert.Equal(t, 1002, createdVMs[2])
}

func TestScaleDown(t *testing.T) {
	var deletedVMIDs []int
	srv := newMockProxmoxServerBatch(t, &deletedVMIDs)
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	// Create nodes that will be drained
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-worker-0"}},
	}

	r := &Reconciler{
		KubeClient: fake.NewSimpleClientset(&corev1.NodeList{Items: nodes}),
		Proxmox:    proxmoxClient,
		BaseVMID:   1000,
		Namespace:  "autoscaler-system",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Scale from 3 to 1: should drain worker-2 and worker-1
	r.scaleDown(ctx, 3, 1, "test-cluster")

	// VMID for worker-2: 1000 + 2 = 1002, worker-1: 1000 + 1 = 1001
	assert.Len(t, deletedVMIDs, 2)
	assert.Contains(t, deletedVMIDs, 1002)
	assert.Contains(t, deletedVMIDs, 1001)
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
	// Node exists but is NOT ready
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
		},
	}

	// Pending unschedulable pod
	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Reason: "Unschedulable", Status: corev1.ConditionTrue},
			},
		},
	}

	var createdVMs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/access/ticket" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{"ticket": "t", "CSRFPreventionToken": "c"},
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
		if r.Method == "POST" && r.URL.Path != "" {
			_ = r.ParseForm()
			if vmid := r.FormValue("vmid"); vmid != "" {
				createdVMs++
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
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	r := &Reconciler{
		KubeClient: fake.NewSimpleClientset(cm, pendingPod, readyNode),
		Proxmox:    proxmoxClient,
		BaseVMID:   1000,
		Namespace:  "autoscaler-system",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = r.reconcile(ctx)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, createdVMs, 1)
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

	// 3 worker nodes, no pending pods → should scale down to min_workers=1
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-2"}},
	}

	var deletedVMIDs []int
	srv := newMockProxmoxServerBatch(t, &deletedVMIDs)
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	r := &Reconciler{
		KubeClient: fake.NewSimpleClientset(cm, &corev1.NodeList{Items: nodes}),
		Proxmox:    proxmoxClient,
		BaseVMID:   1000,
		Namespace:  "autoscaler-system",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = r.reconcile(ctx)
	assert.NoError(t, err)
	// Should have deleted 2 VMs (worker-2 and worker-1)
	assert.Len(t, deletedVMIDs, 2)
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

	// 2 workers, no pending pods → at min, no action needed
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-worker-1"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no proxmox calls expected")
	}))
	defer srv.Close()

	proxmoxClient, err := newTestProxmoxClient(srv.URL)
	require.NoError(t, err)

	r := &Reconciler{
		KubeClient: fake.NewSimpleClientset(cm, &corev1.NodeList{Items: nodes}),
		Proxmox:    proxmoxClient,
		BaseVMID:   1000,
		Namespace:  "autoscaler-system",
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
			// Extract VMID from path like /api2/json/nodes/pve/qemu/1000
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

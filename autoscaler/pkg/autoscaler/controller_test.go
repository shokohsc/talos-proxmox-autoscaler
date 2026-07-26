package autoscaler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAtoiDefault(t *testing.T) {
	assert.Equal(t, 5, atoiDefault("5", 0))
	assert.Equal(t, 0, atoiDefault("", 0))
	assert.Equal(t, 3, atoiDefault("abc", 3))
}

func TestCalculateNeeded(t *testing.T) {
	r := &Reconciler{}
	cfg := &Config{MinWorkers: 1, MaxCPU: 4, MaxMemoryGiB: 8}

	t.Run("no pending pods returns min workers", func(t *testing.T) {
		got := r.calculateNeeded(resource.Quantity{}, resource.Quantity{}, 0, cfg)
		assert.Equal(t, int32(1), got)
	})

	t.Run("cpu-driven scaling", func(t *testing.T) {
		// 20 millicpu pending, 4 CPU cap => ceil(20/4000) = 1
		pendingCPU := resource.MustParse("20m")
		got := r.calculateNeeded(pendingCPU, resource.Quantity{}, 1, cfg)
		assert.Equal(t, int32(1), got)
	})

	t.Run("mem-driven scaling", func(t *testing.T) {
		// 20Gi pending, 8Gi cap => ceil(20/8) = 3
		pendingMem := resource.MustParse("20Gi")
		got := r.calculateNeeded(resource.Quantity{}, pendingMem, 1, cfg)
		assert.Equal(t, int32(3), got)
	})

	t.Run("takes max of cpu and mem", func(t *testing.T) {
		pendingCPU := resource.MustParse("32")  // 32 CPU => 8 workers
		pendingMem := resource.MustParse("64Gi") // 64Gi => 8 workers
		got := r.calculateNeeded(pendingCPU, pendingMem, 10, cfg)
		assert.Equal(t, int32(8), got)
	})
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

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm)}
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

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm)}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "legacy", cfg.ClusterName)
	assert.Equal(t, 4, cfg.MaxCPU)
	assert.Equal(t, 8, cfg.MaxMemoryGiB)
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

	r := &Reconciler{KubeClient: fake.NewSimpleClientset(cm)}
	cfg, err := r.readConfig(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 16, cfg.MaxCPU)
	assert.Equal(t, 32, cfg.MaxMemoryGiB)
}

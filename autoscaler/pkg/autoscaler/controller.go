package autoscaler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/talos-proxmox-autoscaler/pkg/proxmox"
)

const (
	provisioningTimeout = 10 * time.Minute
	reconcileInterval   = 30 * time.Second
	deschedulerLabel    = "descheduler.kubernetes.io/node-probable-eviction"
)

type VMSize struct {
	CPU       int
	MemoryGiB int
}

type WorkerNodeConfig struct {
	Name string `json:"name"`
}

type GPUNodeConfig struct {
	Type       string              `json:"type"`
	Nodes      []string            `json:"nodes"`
	PCIDevices []proxmox.PCIDevice `json:"pci_devices"`
}

type Config struct {
	ClusterName   string
	MinWorkers    int32
	MaxWorkers    int32
	MinCPU        int
	MaxCPU        int
	MinMemoryGiB  int
	MaxMemoryGiB  int
	DiskGiB       int32
	StoragePool   string
	NetworkBridge string
	MACAddress    string
	Serial        string
	CPUType       string
	Tags          string
	VLANID        int

	WorkerNodes []WorkerNodeConfig
	GPUNodes    []GPUNodeConfig
}

type Reconciler struct {
	Proxmox      *proxmox.Client
	KubeClient   kubernetes.Interface
	Namespace    string
	BaseVMID     int
	BaseGPUVMID  int
	WorkerPrefix string // e.g. "worker-vm"
	GPUPrefix    string // e.g. "worker-vm-gpu"

	// ponytail: in-flight VMs tracked by name, prevents duplicate creation
	// when reconcile fires before previous VMs join the cluster
	InFlight   map[string]bool
	inFlightMu sync.Mutex
}

func (r *Reconciler) Start(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	klog.Info("Autoscaler started", "interval", reconcileInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				klog.Error(err, "Reconcile failed")
			}
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	cfg, err := r.readConfig(ctx)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if err := r.Proxmox.ResolveNode(ctx); err != nil {
		return fmt.Errorf("resolve node: %w", err)
	}
	klog.V(2).Info("Config loaded", "cluster", cfg.ClusterName, "min_workers", cfg.MinWorkers, "max_workers", cfg.MaxWorkers)

	evicted, err := r.findEvictableNodes(ctx, cfg.ClusterName)
	if err != nil {
		return err
	}
	if len(evicted) > 0 {
		klog.V(1).Info("Found evictable nodes", "count", len(evicted))
	}
	for _, node := range evicted {
		klog.Info("Removing descheduler-evicted node", "node", node.Name)
		// Detect type from name prefix to get correct base VMID
		if strings.HasPrefix(node.Name, cfg.ClusterName+"-"+r.GPUPrefix+"-") {
			r.drainAndDelete(ctx, node.Name, cfg.ClusterName, r.GPUPrefix, r.BaseGPUVMID)
		} else {
			r.drainAndDelete(ctx, node.Name, cfg.ClusterName, r.WorkerPrefix, r.BaseVMID)
		}
	}

	pendingCPU, pendingMem, pendingGPU, unschedulableCount, err := r.aggregatePending(ctx)
	if err != nil {
		return err
	}
	klog.V(2).Info("Pending pods", "cpu", pendingCPU.String(), "memory", pendingMem.String(), "gpu", pendingGPU, "count", unschedulableCount)

	currentWorkers := r.countWorkers(ctx, cfg.ClusterName, r.WorkerPrefix)
	currentGPUWorkers := r.countWorkers(ctx, cfg.ClusterName, r.GPUPrefix)

	// Count in-flight VMs as current to prevent duplicate creation
	// when reconcile fires before previous VMs join the cluster
	inFlightRegular := r.countInFlight(cfg.ClusterName, r.WorkerPrefix)
	inFlightGPU := r.countInFlight(cfg.ClusterName, r.GPUPrefix)
	currentWorkers += inFlightRegular
	currentGPUWorkers += inFlightGPU

	workersNeeded, vmSize := r.calculateNeeded(pendingCPU, pendingMem, unschedulableCount, cfg)
	workersNeeded = clamp(workersNeeded, cfg.MinWorkers, cfg.MaxWorkers)
	
	gpuWorkersNeeded := int32(0)
	if pendingGPU > 0 {
		gpuWorkersNeeded = int32(pendingGPU)
		// GPU workers use max resources by default
		gpuVMSize := VMSize{CPU: cfg.MaxCPU, MemoryGiB: cfg.MaxMemoryGiB}
		klog.V(2).Info("GPU Scale decision", "current", currentGPUWorkers, "needed", gpuWorkersNeeded, "vm_size", gpuVMSize)
		
		if gpuWorkersNeeded > currentGPUWorkers {
			klog.Info("Scaling up GPU workers", "current", currentGPUWorkers, "desired", gpuWorkersNeeded, "size", gpuVMSize)
			r.scaleUp(ctx, currentGPUWorkers, gpuWorkersNeeded, gpuVMSize, cfg, "gpu")
		} else if gpuWorkersNeeded < currentGPUWorkers && unschedulableCount == 0 {
			klog.Info("Scaling down GPU workers", "current", currentGPUWorkers, "desired", gpuWorkersNeeded)
			r.scaleDown(ctx, currentGPUWorkers, gpuWorkersNeeded, cfg.ClusterName, r.GPUPrefix, r.BaseGPUVMID)
		}
	}

	klog.V(2).Info("Scale decision", "current", currentWorkers, "needed", workersNeeded, "vm_size", vmSize)

	if workersNeeded > currentWorkers {
		klog.Info("Scaling up", "current", currentWorkers, "desired", workersNeeded, "size", vmSize)
		r.scaleUp(ctx, currentWorkers, workersNeeded, vmSize, cfg, "vm")
	} else if workersNeeded < currentWorkers && unschedulableCount == 0 {
		klog.Info("Scaling down", "current", currentWorkers, "desired", workersNeeded)
		r.scaleDown(ctx, currentWorkers, workersNeeded, cfg.ClusterName, r.WorkerPrefix, r.BaseVMID)
	}

	return nil
}

func (r *Reconciler) readConfig(ctx context.Context) (*Config, error) {
	cm, err := r.KubeClient.CoreV1().ConfigMaps(r.Namespace).Get(ctx, "autoscaler-config", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	d := cm.Data

	config := &Config{
		ClusterName:   d["cluster_name"],
		MinWorkers:    int32(clampInt(atoiDefault(d["min_workers"], 1), 0, 100)),
		MaxWorkers:    int32(clampInt(atoiDefault(d["max_workers"], 10), 1, 100)),
		MinCPU:        clampInt(atoiDefault(d["min_cpu"], 2), 1, 128),
		MaxCPU:        clampInt(atoiDefault(d["max_cpu"], 8), 1, 128),
		MinMemoryGiB:  clampInt(atoiDefault(d["min_memory_gib"], 4), 1, 1024),
		MaxMemoryGiB:  clampInt(atoiDefault(d["max_memory_gib"], 16), 1, 1024),
		DiskGiB:       int32(clampInt(atoiDefault(d["disk_gib"], 50), 10, 4096)),
		StoragePool:   d["storage_pool"],
		NetworkBridge: d["network_bridge"],
		MACAddress:    d["mac_address"],
		Serial:        d["serial"],
		CPUType:       d["cpu_type"],
		Tags:          d["tags"],
		VLANID:        atoiDefault(d["vlan_id"], 0),
	}

	if d["worker_nodes"] != "" {
		if err := json.Unmarshal([]byte(d["worker_nodes"]), &config.WorkerNodes); err != nil {
			return nil, fmt.Errorf("parse worker_nodes: %w", err)
		}
	}
	if d["worker_gpu_nodes"] != "" {
		if err := json.Unmarshal([]byte(d["worker_gpu_nodes"]), &config.GPUNodes); err != nil {
			return nil, fmt.Errorf("parse worker_gpu_nodes: %w", err)
		}
	}

	if config.ClusterName == "" {
		return nil, fmt.Errorf("cluster_name is required")
	}

	return config, nil
}

func (r *Reconciler) aggregatePending(ctx context.Context) (resource.Quantity, resource.Quantity, int, int, error) {
	podList, err := r.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return resource.Quantity{}, resource.Quantity{}, 0, 0, err
	}

	var totalCPU, totalMem resource.Quantity
	count := 0
	gpuCount := 0
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Reason == "Unschedulable" {
				count++
				for _, c := range pod.Spec.Containers {
					if cpu := c.Resources.Requests.Cpu(); cpu != nil {
						totalCPU.Add(*cpu)
					}
					if mem := c.Resources.Requests.Memory(); mem != nil {
						totalMem.Add(*mem)
					}
					if gpu := c.Resources.Requests.Name("nvidia.com/gpu", resource.DecimalSI); gpu != nil {
						gpuVal, _ := gpu.AsInt64()
						gpuCount += int(gpuVal)
					}
				}
				break
			}
		}
	}
	return totalCPU, totalMem, gpuCount, count, nil
}

func (r *Reconciler) calculateNeeded(pendingCPU, pendingMem resource.Quantity, pendingPods int, cfg *Config) (int32, VMSize) {
	if pendingPods == 0 {
		return cfg.MinWorkers, VMSize{CPU: cfg.MinCPU, MemoryGiB: cfg.MinMemoryGiB}
	}
	cpuCap := resource.MustParse(fmt.Sprintf("%d", cfg.MaxCPU))
	memCap := resource.MustParse(fmt.Sprintf("%dGi", cfg.MaxMemoryGiB))

	var byCPU, byMem int32
	if pendingCPU.Cmp(resource.Quantity{}) > 0 {
		byCPU = int32((pendingCPU.MilliValue() + cpuCap.MilliValue() - 1) / cpuCap.MilliValue())
	}
	if pendingMem.Cmp(resource.Quantity{}) > 0 {
		byMem = int32((pendingMem.Value() + memCap.Value() - 1) / memCap.Value())
	}
	needed := byCPU
	if byMem > needed {
		needed = byMem
	}
	if needed < 1 {
		needed = 1
	}

	totalCPU := int(pendingCPU.MilliValue()) / 1000
	totalMem := int(pendingMem.Value()) / (1024 * 1024 * 1024)
	size := calculateVMSize(totalCPU, totalMem, int(needed), cfg)

	return needed, size
}

func (r *Reconciler) countWorkers(ctx context.Context, clusterName, prefix string) int32 {
	nodeList, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	count := int32(0)
	fullPrefix := clusterName + "-" + prefix + "-"
	for _, node := range nodeList.Items {
		if strings.HasPrefix(node.Name, fullPrefix) {
			// Ensure exact prefix match: next char must be digit (or end of string)
			idx := len(fullPrefix)
			if idx == len(node.Name) || (idx < len(node.Name) && node.Name[idx] >= '0' && node.Name[idx] <= '9') {
				count++
			}
		}
	}
	return count
}

func (r *Reconciler) countInFlight(clusterName, prefix string) int32 {
	r.inFlightMu.Lock()
	defer r.inFlightMu.Unlock()
	count := int32(0)
	fullPrefix := clusterName + "-" + prefix + "-"
	for name := range r.InFlight {
		if strings.HasPrefix(name, fullPrefix) {
			idx := len(fullPrefix)
			if idx == len(name) || (idx < len(name) && name[idx] >= '0' && name[idx] <= '9') {
				count++
			}
		}
	}
	return count
}

func (r *Reconciler) findEvictableNodes(ctx context.Context, clusterName string) ([]corev1.Node, error) {
	nodeList, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var evictable []corev1.Node
	// Check both standard and GPU worker prefixes
	prefixes := []string{r.WorkerPrefix, r.GPUPrefix}
	for _, node := range nodeList.Items {
		for _, prefix := range prefixes {
			fullPrefix := clusterName + "-" + prefix + "-"
			if strings.HasPrefix(node.Name, fullPrefix) && labels.Set(node.Labels).Has(deschedulerLabel) {
				evictable = append(evictable, node)
				break
			}
		}
	}
	return evictable, nil
}

func (r *Reconciler) scaleUp(ctx context.Context, current, desired int32, size VMSize, cfg *Config, workerType string) {
	prefix := r.WorkerPrefix
	baseVMID := r.BaseVMID
	if workerType == "gpu" {
		prefix = r.GPUPrefix
		baseVMID = r.BaseGPUVMID
	}

	for i := current; i < desired; i++ {
		vmid := baseVMID + int(i)
		vmName := fmt.Sprintf("%s-%s-%d", cfg.ClusterName, prefix, i)

		// Check if already in-flight (shouldn't happen with in-flight counting, but guard anyway)
		r.inFlightMu.Lock()
		if _, exists := r.InFlight[vmName]; exists {
			r.inFlightMu.Unlock()
			klog.V(2).Info("VM already in-flight, skipping", "name", vmName)
			continue
		}
		r.inFlightMu.Unlock()

		// Pre-flight: skip if VM already exists in Proxmox (prev reconcile created it but node hasn't joined K8s yet)
		if existingID, err := r.Proxmox.FindVMByName(ctx, vmName); err == nil {
			klog.V(2).Info("VM already exists in Proxmox, skipping creation", "name", vmName, "vmid", existingID)
			continue
		}

		r.inFlightMu.Lock()
		r.InFlight[vmName] = true
		r.inFlightMu.Unlock()

		klog.Info("Creating worker VM", "name", vmName, "vmid", vmid, "type", workerType)

		var pciDevices []proxmox.PCIDevice
		if workerType == "gpu" {
			for _, gpuNode := range cfg.GPUNodes {
				if len(gpuNode.PCIDevices) > 0 {
					pciDevices = gpuNode.PCIDevices
					break
				}
			}
		}

		// Non-blocking: create VM and wait for node in a goroutine
		go func(vmName string, vmid int, pciDevices []proxmox.PCIDevice) {
			defer func() {
				r.inFlightMu.Lock()
				delete(r.InFlight, vmName)
				r.inFlightMu.Unlock()
			}()

			// Pre-flight: skip if VM already exists in Proxmox (prev reconcile created it but node hasn't joined K8s yet)
			if existingID, err := r.Proxmox.FindVMByName(ctx, vmName); err == nil {
				klog.V(2).Info("VM already exists in Proxmox, skipping creation", "name", vmName, "vmid", existingID)
				return
			}

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
				Tags:          cfg.Tags,
				VLANID:        cfg.VLANID,
				PCIDevices:    pciDevices,
			})
			if err != nil {
				klog.Error(err, "Failed to create VM", "vmid", vmid)
				return
			}
			if err := r.waitForNodeReady(ctx, ip); err != nil {
				klog.Error(err, "Node not ready after provisioning", "ip", ip)
			}
		}(vmName, vmid, pciDevices)
	}
}

func (r *Reconciler) scaleDown(ctx context.Context, current, desired int32, clusterName, prefix string, baseVMID int) {
	for i := current; i > desired; i-- {
		nodeName := fmt.Sprintf("%s-%s-%d", clusterName, prefix, i-1)
		klog.Info("Removing worker node", "node", nodeName)
		r.drainAndDelete(ctx, nodeName, clusterName, prefix, baseVMID)
	}
}

func (r *Reconciler) drainAndDelete(ctx context.Context, nodeName, clusterName, prefix string, baseVMID int) {
	r.drainNode(ctx, nodeName)

	// ponytail: use strings.Cut to parse "-{prefix}-N" suffix, avoids Sscanf greedy %s bug with multi-hyphen cluster names
	suffix, ok := strings.CutPrefix(nodeName, clusterName+"-"+prefix+"-")
	if !ok {
		klog.Error(fmt.Errorf("unexpected node name format: %s", nodeName), "Failed to parse VM index")
		return
	}
	var vmIndex int
	if _, err := fmt.Sscanf(suffix, "%d", &vmIndex); err != nil {
		klog.Error(err, "Failed to parse VM index", "node", nodeName)
		return
	}
	vmid := baseVMID + vmIndex
	if err := r.Proxmox.DeleteVM(ctx, vmid); err != nil {
		klog.Error(err, "Failed to delete VM", "vmid", vmid)
	}
}

func (r *Reconciler) drainNode(ctx context.Context, nodeName string) {
	node, err := r.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return
	}
	node.Spec.Unschedulable = true
	_, _ = r.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})

	podList, err := r.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return
	}
	for _, pod := range podList.Items {
		if pod.Namespace == "kube-system" {
			continue
		}
		_ = r.KubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: int64Ptr(30),
		})
	}
}

func (r *Reconciler) waitForNodeReady(ctx context.Context, ip string) error {
	deadline := time.Now().Add(provisioningTimeout)
	for time.Now().Before(deadline) {
		nodeList, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, node := range nodeList.Items {
				for _, addr := range node.Status.Addresses {
					if addr.Address == ip && addr.Type == corev1.NodeInternalIP {
						for _, c := range node.Status.Conditions {
							if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
								klog.Info("Node ready", "node", node.Name, "ip", ip)
								return nil
							}
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("timeout waiting for node at %s", ip)
}

func clamp(v, min, max int32) int32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func calculateVMSize(totalCPU, totalMemory, vmCount int, config *Config) VMSize {
	if vmCount <= 0 {
		vmCount = 1
	}
	cpu := (totalCPU + vmCount - 1) / vmCount
	mem := (totalMemory + vmCount - 1) / vmCount
	return VMSize{
		CPU:       clampInt(cpu, config.MinCPU, config.MaxCPU),
		MemoryGiB: clampInt(mem, config.MinMemoryGiB, config.MaxMemoryGiB),
	}
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func atoiDefault(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

func int64Ptr(i int64) *int64 {
	return &i
}

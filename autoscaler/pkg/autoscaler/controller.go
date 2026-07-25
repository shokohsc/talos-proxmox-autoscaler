package autoscaler

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/talos-proxmox-autoscaler/pkg/proxmox"
)

const (
	clusterFinalizer    = "autoscaler.talos.dev/cluster"
	provisioningTimeout = 10 * time.Minute
	drainTimeout        = 30 * time.Second
	reconcileInterval   = 30 * time.Second
	deschedulerLabel    = "descheduler.kubernetes.io/node-probable-eviction"
)

type MachineDeploymentReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Proxmox      *proxmox.Client
	KubeClient   kubernetes.Interface
	BaseVMID     int
}

func (r *MachineDeploymentReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := klog.FromContext(ctx)

	var deployment MachineDeployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !deployment.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &deployment)
	}

	if !controllerutil.ContainsFinalizer(&deployment, clusterFinalizer) {
		controllerutil.AddFinalizer(&deployment, clusterFinalizer)
		if err := r.Update(ctx, &deployment); err != nil {
			return reconcile.Result{}, err
		}
	}

	var machineClass MachineClass
	if err := r.Get(ctx, client.ObjectKey{Name: deployment.Spec.MachineClassName}, &machineClass); err != nil {
		log.Error(err, "Failed to get machine class", "name", deployment.Spec.MachineClassName)
		r.setPhase(&deployment, "Failed", fmt.Sprintf("MachineClass %s not found", deployment.Spec.MachineClassName))
		return reconcile.Result{RequeueAfter: reconcileInterval}, r.Status().Update(ctx, &deployment)
	}

	// Check for descheduler-evictable nodes first
	evictableNodes, err := r.findDeschedulerEvictableNodes(ctx, deployment.Spec.ClusterName)
	if err != nil {
		return reconcile.Result{RequeueAfter: reconcileInterval}, err
	}
	for _, node := range evictableNodes {
		log.Info("Removing descheduler-evicted node", "node", node.Name)
		if err := r.removeNode(ctx, &deployment, node.Name); err != nil {
			log.Error(err, "Failed to remove evicted node", "node", node.Name)
			continue
		}
		deployment.Status.ReadyReplicas--
		if err := r.Status().Update(ctx, &deployment); err != nil {
			return reconcile.Result{RequeueAfter: reconcileInterval}, err
		}
	}

	// Aggregate pending pod resource requests
	pendingCPU, pendingMemory, unschedulableCount, err := r.aggregatePendingRequests(ctx)
	if err != nil {
		return reconcile.Result{RequeueAfter: reconcileInterval}, err
	}

	// Calculate how many workers we need based on pending pods
	// Each worker gets: machineClass.Spec.VCPU cores, machineClass.Spec.MemoryGiB GB
	// We create enough workers to fit the pending requests, up to deployment.Spec.Replicas
	workersNeeded := r.calculateWorkersNeeded(pendingCPU, pendingMemory, unschedulableCount, &machineClass, deployment.Spec.Replicas)

	// Scale up
	if workersNeeded > deployment.Status.ReadyReplicas {
		log.Info("Scaling up", "current", deployment.Status.ReadyReplicas, "desired", workersNeeded,
			"pendingCPU", pendingCPU.MilliValue(), "pendingMemoryMi", pendingMemory.Value()/(1024*1024))
		if err := r.scaleUp(ctx, &deployment, &machineClass, workersNeeded); err != nil {
			return reconcile.Result{RequeueAfter: reconcileInterval}, err
		}
		return reconcile.Result{RequeueAfter: reconcileInterval}, nil
	}

	// Scale down (only if no pending pods and replicas < ready)
	if deployment.Spec.Replicas < deployment.Status.ReadyReplicas && unschedulableCount == 0 {
		log.Info("Scaling down", "current", deployment.Status.ReadyReplicas, "desired", deployment.Spec.Replicas)
		if err := r.scaleDown(ctx, &deployment, deployment.Spec.Replicas); err != nil {
			return reconcile.Result{RequeueAfter: reconcileInterval}, err
		}
		return reconcile.Result{RequeueAfter: reconcileInterval}, nil
	}

	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *MachineDeploymentReconciler) handleDeletion(ctx context.Context, deployment *MachineDeployment) (reconcile.Result, error) {
	if controllerutil.ContainsFinalizer(deployment, clusterFinalizer) {
		if err := r.drainAllNodes(ctx, deployment); err != nil {
			return reconcile.Result{}, err
		}

		if err := r.deleteAllVMs(ctx, deployment); err != nil {
			return reconcile.Result{}, err
		}

		controllerutil.RemoveFinalizer(deployment, clusterFinalizer)
		if err := r.Update(ctx, deployment); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

// aggregatePendingRequests sums CPU and memory requests of all unschedulable pods.
func (r *MachineDeploymentReconciler) aggregatePendingRequests(ctx context.Context) (resource.Quantity, resource.Quantity, int, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace("")); err != nil {
		return resource.Quantity{}, resource.Quantity{}, 0, err
	}

	totalCPU := resource.Quantity{}
	totalMemory := resource.Quantity{}
	count := 0

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Reason == "Unschedulable" {
				count++
				for _, container := range pod.Spec.Containers {
					if cpu := container.Resources.Requests.Cpu(); cpu != nil {
						totalCPU.Add(*cpu)
					}
					if mem := container.Resources.Requests.Memory(); mem != nil {
						totalMemory.Add(*mem)
					}
				}
				break
			}
		}
	}
	return totalCPU, totalMemory, count, nil
}

// calculateWorkersNeeded determines how many workers to create to fit pending requests.
func (r *MachineDeploymentReconciler) calculateWorkersNeeded(pendingCPU, pendingMemory resource.Quantity, pendingPods int, mc *MachineClass, maxReplicas int32) int32 {
	if pendingPods == 0 {
		return 0
	}

	workerCPUCapacity := resource.MustParse(fmt.Sprintf("%d", mc.Spec.VCPU))
	workerMemCapacity := resource.MustParse(fmt.Sprintf("%dGi", mc.Spec.MemoryGiB))

	// Calculate workers needed based on CPU
	workersByCPU := int32(0)
	if pendingCPU.Cmp(resource.Quantity{}) > 0 {
		workersByCPU = int32((pendingCPU.MilliValue() + workerCPUCapacity.MilliValue() - 1) / workerCPUCapacity.MilliValue())
	}

	// Calculate workers needed based on memory
	workersByMem := int32(0)
	if pendingMemory.Cmp(resource.Quantity{}) > 0 {
		workersByMem = int32((pendingMemory.Value() + workerMemCapacity.Value() - 1) / workerMemCapacity.Value())
	}

	// Take the larger of the two, ensure at least 1 if there are pending pods
	needed := workersByCPU
	if workersByMem > needed {
		needed = workersByMem
	}
	if needed < 1 {
		needed = 1
	}

	// Cap at maxReplicas
	if needed > maxReplicas {
		needed = maxReplicas
	}

	return needed
}

// findDeschedulerEvictableNodes returns nodes marked by the descheduler for eviction.
func (r *MachineDeploymentReconciler) findDeschedulerEvictableNodes(ctx context.Context, clusterName string) ([]corev1.Node, error) {
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return nil, err
	}

	var evictable []corev1.Node
	prefix := fmt.Sprintf("%s-worker-", clusterName)

	for _, node := range nodeList.Items {
		if !strings.HasPrefix(node.Name, prefix) {
			continue
		}
		if labels.Set(node.Labels).Has(deschedulerLabel) {
			evictable = append(evictable, node)
		}
	}
	return evictable, nil
}

// removeNode cordons, drains, and deletes a worker node and its VM.
func (r *MachineDeploymentReconciler) removeNode(ctx context.Context, deployment *MachineDeployment, nodeName string) error {
	log := klog.FromContext(ctx)

	if err := r.drainNode(ctx, nodeName); err != nil {
		log.Error(err, "Failed to drain node, proceeding with deletion", "node", nodeName)
	}

	var vmIndex int
	if _, err := fmt.Sscanf(nodeName, "%s-worker-%d", &deployment.Spec.ClusterName, &vmIndex); err != nil {
		return fmt.Errorf("failed to parse VM index from node name %s: %w", nodeName, err)
	}

	vmid := r.BaseVMID + vmIndex
	if err := r.Proxmox.DeleteVM(ctx, vmid); err != nil {
		return fmt.Errorf("failed to delete VM: %w", err)
	}

	return nil
}

func (r *MachineDeploymentReconciler) scaleUp(ctx context.Context, deployment *MachineDeployment, machineClass *MachineClass, desired int32) error {
	log := klog.FromContext(ctx)

	for i := deployment.Status.ReadyReplicas; i < desired; i++ {
		vmid := r.BaseVMID + int(i)
		vmName := fmt.Sprintf("%s-worker-%d", deployment.Spec.ClusterName, i)
		log.Info("Creating new worker VM", "name", vmName, "vmid", vmid)

		vmIP, err := r.Proxmox.CreateVM(ctx, proxmox.VMConfig{
			Name:          vmName,
			VMID:          vmid,
			VCPU:          machineClass.Spec.VCPU,
			MemoryMiB:     machineClass.Spec.MemoryGiB * 1024,
			DiskGiB:       machineClass.Spec.DiskGiB,
			StoragePool:   machineClass.Spec.StoragePool,
			NetworkBridge: machineClass.Spec.NetworkBridge,
			MACAddress:    machineClass.Spec.MACAddress,
			Serial:        machineClass.Spec.Serial,
		})
		if err != nil {
			return fmt.Errorf("failed to create VM: %w", err)
		}

		if err := r.waitForNodeReady(ctx, vmIP); err != nil {
			return fmt.Errorf("node %s not ready: %w", vmIP, err)
		}

		deployment.Status.ReadyReplicas++
		if err := r.Status().Update(ctx, deployment); err != nil {
			return err
		}
	}

	r.setPhase(deployment, "Ready", fmt.Sprintf("All %d nodes ready", desired))
	return r.Status().Update(ctx, deployment)
}

func (r *MachineDeploymentReconciler) scaleDown(ctx context.Context, deployment *MachineDeployment, desired int32) error {
	log := klog.FromContext(ctx)

	for i := deployment.Status.ReadyReplicas; i > desired; i-- {
		nodeName := fmt.Sprintf("%s-worker-%d", deployment.Spec.ClusterName, i-1)
		log.Info("Removing worker node", "node", nodeName)

		if err := r.drainNode(ctx, nodeName); err != nil {
			log.Error(err, "Failed to drain node, proceeding with deletion", "node", nodeName)
		}

		vmid := r.BaseVMID + int(i-1)
		if err := r.Proxmox.DeleteVM(ctx, vmid); err != nil {
			return fmt.Errorf("failed to delete VM: %w", err)
		}

		deployment.Status.ReadyReplicas--
		if err := r.Status().Update(ctx, deployment); err != nil {
			return err
		}
	}

	r.setPhase(deployment, "Ready", fmt.Sprintf("Scaled down to %d nodes", desired))
	return r.Status().Update(ctx, deployment)
}

func (r *MachineDeploymentReconciler) drainNode(ctx context.Context, nodeName string) error {
	log := klog.FromContext(ctx)

	node, err := r.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	node.Spec.Unschedulable = true
	if _, err := r.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return err
	}

	podList, err := r.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return err
	}

	for _, pod := range podList.Items {
		if pod.Namespace == "kube-system" {
			continue
		}
		log.Info("Evicting pod", "pod", pod.Name, "namespace", pod.Namespace)
		if err := r.KubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: int64Ptr(30),
		}); err != nil {
			log.Error(err, "Failed to evict pod", "pod", pod.Name)
		}
	}

	return nil
}

func (r *MachineDeploymentReconciler) drainAllNodes(ctx context.Context, deployment *MachineDeployment) error {
	for i := int32(0); i < deployment.Status.ReadyReplicas; i++ {
		nodeName := fmt.Sprintf("%s-worker-%d", deployment.Spec.ClusterName, i)
		if err := r.drainNode(ctx, nodeName); err != nil {
			klog.FromContext(ctx).Error(err, "Failed to drain node during deletion", "node", nodeName)
		}
	}
	return nil
}

func (r *MachineDeploymentReconciler) deleteAllVMs(ctx context.Context, deployment *MachineDeployment) error {
	for i := int32(0); i < deployment.Status.ReadyReplicas; i++ {
		vmid := r.BaseVMID + int(i)
		if err := r.Proxmox.DeleteVM(ctx, vmid); err != nil {
			klog.FromContext(ctx).Error(err, "Failed to delete VM during deletion", "vmid", vmid)
		}
	}
	return nil
}

func (r *MachineDeploymentReconciler) waitForNodeReady(ctx context.Context, ip string) error {
	log := klog.FromContext(ctx)
	deadline := time.Now().Add(provisioningTimeout)

	for time.Now().Before(deadline) {
		nodeList, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}

		for _, node := range nodeList.Items {
			for _, addr := range node.Status.Addresses {
				if addr.Address == ip && addr.Type == corev1.NodeInternalIP {
					for _, condition := range node.Status.Conditions {
						if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
							log.Info("Node ready", "node", node.Name, "ip", ip)
							return nil
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

func (r *MachineDeploymentReconciler) setPhase(deployment *MachineDeployment, phase, message string) {
	deployment.Status.Phase = phase
	deployment.Status.Message = message
}

func int64Ptr(i int64) *int64 {
	return &i
}

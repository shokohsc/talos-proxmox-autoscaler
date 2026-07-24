package tofu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"k8s.io/klog/v2"
)

type Client struct {
	binary     string
	workingDir string
	statePath  string
	mu         sync.Mutex
}

type VMConfig struct {
	ClusterName   string
	VMIndex       int
	VCPU          int32
	MemoryGiB     int32
	DiskGiB       int32
	NetworkBridge string
	StoragePool   string
	MACAddress    string
	Serial        string
}

type VMInfo struct {
	ID     string
	IP     string
	Status string
}

func NewClient(binary, workingDir string) *Client {
	statePath := filepath.Join(workingDir, "terraform.tfstate")
	return &Client{
		binary:     binary,
		workingDir: workingDir,
		statePath:  statePath,
	}
}

func (c *Client) CreateVM(ctx context.Context, config VMConfig) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	log := klog.FromContext(ctx)

	vars := map[string]string{
		"cluster_name":    config.ClusterName,
		"vm_index":        fmt.Sprintf("%d", config.VMIndex),
		"vcpu":            fmt.Sprintf("%d", config.VCPU),
		"memory_gib":      fmt.Sprintf("%d", config.MemoryGiB),
		"disk_gib":        fmt.Sprintf("%d", config.DiskGiB),
		"network_bridge":  config.NetworkBridge,
		"storage_pool":    config.StoragePool,
	}
	if config.MACAddress != "" {
		vars["mac_address"] = config.MACAddress
	}
	if config.Serial != "" {
		vars["serial"] = config.Serial
	}

	if err := c.run(ctx, "apply", "-auto-approve", "-var", vars); err != nil {
		return "", fmt.Errorf("tofu apply failed: %w", err)
	}

	output, err := c.output(ctx, "vm_ip")
	if err != nil {
		return "", fmt.Errorf("failed to get output: %w", err)
	}

	log.Info("Worker VM created", "ip", output, "config", config)
	return output, nil
}

func (c *Client) DeleteVM(ctx context.Context, clusterName string, vmIndex int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	log := klog.FromContext(ctx)

	vars := map[string]string{
		"cluster_name": clusterName,
		"vm_index":     fmt.Sprintf("%d", vmIndex),
	}

	if err := c.run(ctx, "destroy", "-auto-approve", "-var", vars); err != nil {
		return fmt.Errorf("tofu destroy failed: %w", err)
	}

	log.Info("Worker VM deleted", "cluster", clusterName, "index", vmIndex)
	return nil
}

func (c *Client) Destroy(ctx context.Context, clusterName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	log := klog.FromContext(ctx)

	vars := map[string]string{
		"cluster_name": clusterName,
	}

	if err := c.run(ctx, "destroy", "-auto-approve", "-var", vars); err != nil {
		return fmt.Errorf("tofu destroy all failed: %w", err)
	}

	log.Info("All worker VMs destroyed", "cluster", clusterName)
	return nil
}

func (c *Client) run(ctx context.Context, args ...string) error {
	cmdArgs := append([]string{}, args...)
	cmd := exec.CommandContext(ctx, c.binary, cmdArgs...)
	cmd.Dir = c.workingDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	klog.V(2).Infof("tofu %s completed", args[0])
	return nil
}

func (c *Client) output(ctx context.Context, name string) (string, error) {
	cmd := exec.CommandContext(ctx, c.binary, "output", "-json", name)
	cmd.Dir = c.workingDir

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", err
	}

	var result string
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", err
	}

	return result, nil
}

func init() {
	dir := filepath.Join(os.TempDir(), "tofu-autoscaler")
	if err := os.MkdirAll(dir, 0755); err != nil {
		klog.Fatalf("Failed to create tofu working directory: %v", err)
	}
}

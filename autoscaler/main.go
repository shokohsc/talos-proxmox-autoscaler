package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/talos-proxmox-autoscaler/pkg/autoscaler"
	"github.com/talos-proxmox-autoscaler/pkg/proxmox"
)

func main() {
	logLevel := getEnv("LOG_LEVEL", "info")

	level := zap.NewAtomicLevelAt(zapLogLevel(logLevel))
	cfg := zap.NewProductionConfig()
	cfg.Level = level
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	zap.ReplaceGlobals(logger)
	defer func() { _ = logger.Sync() }()

	zap.S().Infow("Starting talos-proxmox-autoscaler", "log_level", logLevel)

	proxmoxURL := getEnv("PROXMOX_API_URL", "https://pve.example.com:8006")
	proxmoxNode := getEnv("PROXMOX_NODE", "pve")
	username := readFile(getEnv("PROXMOX_USERNAME_FILE", ""))
	password := readFile(getEnv("PROXMOX_PASSWORD_FILE", ""))
	tokenID := readFile(getEnv("PROXMOX_API_TOKEN_ID_FILE", "/etc/secrets/proxmox_api_token_id"))
	tokenSecret := readFile(getEnv("PROXMOX_API_TOKEN_SECRET_FILE", "/etc/secrets/proxmox_api_token_secret"))
	insecure := getEnv("PROXMOX_INSECURE", "false") == "true"
	baseVMID, _ := strconv.Atoi(getEnv("BASE_VMID", "2000"))
	if baseVMID == 0 {
		baseVMID = 2000
	}
	baseGPUVMID, _ := strconv.Atoi(getEnv("BASE_GPU_VMID", "3000"))
	if baseGPUVMID == 0 {
		baseGPUVMID = 3000
	}
	workerPrefix := getEnv("WORKER_PREFIX", "worker-vm")
	gpuPrefix := getEnv("GPU_PREFIX", "worker-vm-gpu")

	zap.S().Infow("Proxmox configuration", "url", proxmoxURL, "node", proxmoxNode, "insecure", insecure)

	proxmoxClient, err := proxmox.NewClient(proxmoxURL, username, password, tokenID, tokenSecret, proxmoxNode, insecure)
	if err != nil {
		zap.S().Fatalf("Unable to create proxmox client: %v", err)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		zap.S().Fatalf("Unable to get in-cluster config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		zap.S().Fatalf("Unable to create kubernetes client: %v", err)
	}

	namespace := getEnv("NAMESPACE", "autoscaler-system")
	zap.S().Infow("Controller configuration", "namespace", namespace, "base_vmid", baseVMID)

	r := &autoscaler.Reconciler{
		Proxmox:      proxmoxClient,
		KubeClient:   kubeClient,
		Namespace:    namespace,
		BaseVMID:     baseVMID,
		BaseGPUVMID:  baseGPUVMID,
		WorkerPrefix: workerPrefix,
		GPUPrefix:    gpuPrefix,
		InFlight:     make(map[string]bool),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	r.Start(ctx)
}

func zapLogLevel(level string) zapcore.Level {
	switch level {
	case "trace", "debug":
		return zap.DebugLevel
	case "info":
		return zap.InfoLevel
	case "warn":
		return zap.WarnLevel
	case "error":
		return zap.ErrorLevel
	default:
		return zap.InfoLevel
	}
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

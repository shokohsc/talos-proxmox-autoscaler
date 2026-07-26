package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/talos-proxmox-autoscaler/pkg/autoscaler"
	"github.com/talos-proxmox-autoscaler/pkg/proxmox"
)

func main() {
	flag.Parse()

	logLevel := getEnv("LOG_LEVEL", "info")
	verbosity := logLevelToVerbosity(logLevel)
	klog.InitFlags(nil)
	_ = flag.Set("v", strconv.Itoa(verbosity))
	flag.Parse()

	klog.Info("Starting talos-proxmox-autoscaler", "log_level", logLevel)

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

	klog.V(1).Info("Proxmox configuration", "url", proxmoxURL, "node", proxmoxNode, "insecure", insecure)

	proxmoxClient, err := proxmox.NewClient(proxmoxURL, username, password, tokenID, tokenSecret, proxmoxNode, insecure)
	if err != nil {
		klog.Fatalf("Unable to create proxmox client: %v", err)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Unable to get in-cluster config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Unable to create kubernetes client: %v", err)
	}

	namespace := getEnv("NAMESPACE", "autoscaler-system")
	klog.V(1).Info("Controller configuration", "namespace", namespace, "base_vmid", baseVMID)

	r := &autoscaler.Reconciler{
		Proxmox:    proxmoxClient,
		KubeClient: kubeClient,
		Namespace:  namespace,
		BaseVMID:   baseVMID,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	r.Start(ctx)
}

// logLevelToVerbosity maps log level names to klog verbosity.
// klog verbosity: 0=info+, 1=info, 2=debug, 3=trace
func logLevelToVerbosity(level string) int {
	switch level {
	case "trace":
		return 3
	case "debug":
		return 2
	case "info":
		return 1
	case "warn", "error":
		return 0
	default:
		return 1
	}
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

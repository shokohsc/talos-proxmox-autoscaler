package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/talos-proxmox-autoscaler/pkg/autoscaler"
	"github.com/talos-proxmox-autoscaler/pkg/proxmox"
)

// leaseName is the coordination.k8s.io Lease both replicas contend for. One
// autoscaler per namespace, so it needs no configuration.
const leaseName = "talos-proxmox-autoscaler"

// Renew timings are deliberately longer than the Kubernetes defaults: a
// reconcile can take seconds (draining nodes, Proxmox stop/delete), and losing
// the lease mid-reconcile would abort a scale operation. Failover stays
// invisible at the 30s reconcile interval, and a graceful exit releases the
// lease immediately. Package-level vars so tests can shrink them.
var (
	leaseDuration = 30 * time.Second
	renewDeadline = 20 * time.Second
	retryPeriod   = 5 * time.Second
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
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	identity := getEnv("POD_NAME", "")
	if identity == "" {
		identity, _ = os.Hostname()
	}

	runAsLeader(ctx, kubeClient, namespace, identity, r.Start)
}

// runAsLeader reconciles only while this pod holds the autoscaler Lease. Both
// replicas read the same ConfigMap and the same Proxmox VM list, so without the
// lease they compute the same next index and race on the same VMID, and on
// scale-down both drain and delete the same node. Proxmox rejects the losing
// create and both deletes are idempotent, but the loser keeps deciding from a
// view that is already stale. The standby pod just contends for the lease and
// takes over when the leader exits.
func runAsLeader(ctx context.Context, client kubernetes.Interface, namespace, identity string, onLeading func(context.Context)) {
	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		namespace,
		leaseName,
		client.CoreV1(),
		client.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: identity},
	)
	if err != nil {
		zap.S().Fatalf("Unable to create lease lock: %v", err)
	}

	zap.S().Infow("Contending for autoscaler lease", "lease", namespace+"/"+leaseName, "identity", identity)

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			// Runs in its own goroutine while Run renews the lease below, so a
			// reconcile cannot starve renewal.
			OnStartedLeading: func(ctx context.Context) {
				zap.S().Infow("Acquired autoscaler lease, reconciling", "identity", identity)
				onLeading(ctx)
			},
			// Required by RunOrDie, but it also fires on an ordinary shutdown
			// and on a standby that never led, so the interesting case
			// (leadership lost) is handled after Run returns.
			OnStoppedLeading: func() {},
		},
	})

	// Run returns either on shutdown (ctx cancelled) or when leadership is lost.
	// Leader election does not re-contend in-process, so a pod that stayed up
	// after losing the lease would sit idle forever and silently stop scaling —
	// only the leader's reconcile loop ever ran. Exit instead; the pod restarts
	// and re-contends.
	if ctx.Err() == nil {
		zap.S().Fatalw("Leadership lost, exiting so the pod restarts and re-contends", "identity", identity)
	}
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

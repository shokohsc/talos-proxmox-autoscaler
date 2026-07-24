package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/talos-proxmox-autoscaler/pkg/autoscaler"
)

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election for controller manager.")
	klog.InitFlags(nil)
	flag.Parse()

	klog.Info("Starting talos-proxmox-autoscaler")

	ctrlManager, err := manager.New(manager.GetConfigOrDie(), manager.Options{
		Scheme:                 autoscaler.NewScheme(),
		MetricsBindAddress:     metricsAddr,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "talos-proxmox-autoscaler.lock",
	})
	if err != nil {
		klog.Fatalf("Unable to create manager: %v", err)
	}

	if err := autoscaler.SetupReconciler(ctrlManager); err != nil {
		klog.Fatalf("Unable to setup reconciler: %v", err)
	}

	if err := ctrlManager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		klog.Fatalf("Unable to set up health check: %v", err)
	}
	if err := ctrlManager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		klog.Fatalf("Unable to set up ready check: %v", err)
	}

	ctx, cancel := signal.NotifyContext(ctrlManager.GetContext(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := ctrlManager.Start(ctx); err != nil {
		klog.Fatalf("Unable to start manager: %v", err)
	}
}

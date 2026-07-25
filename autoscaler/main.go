package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/talos-proxmox-autoscaler/pkg/autoscaler"
	"github.com/talos-proxmox-autoscaler/pkg/tofu"
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

	ctrlManager, err := manager.New(config.GetConfigOrDie(), manager.Options{
		Scheme:                 autoscaler.NewScheme(),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "talos-proxmox-autoscaler.lock",
	})
	if err != nil {
		klog.Fatalf("Unable to create manager: %v", err)
	}

	tofuClient := tofu.NewClient("tofu", "/var/lib/tofu")

	restConfig := ctrlManager.GetConfig()
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Unable to create kubernetes client: %v", err)
	}

	reconciler := &autoscaler.MachineDeploymentReconciler{
		Client:     ctrlManager.GetClient(),
		Scheme:     ctrlManager.GetScheme(),
		TofuClient: tofuClient,
		KubeClient: kubeClient,
	}

	if err := builder.ControllerManagedBy(ctrlManager).
		For(&autoscaler.MachineDeployment{}).
		Complete(reconciler); err != nil {
		klog.Fatalf("Unable to setup controller: %v", err)
	}

	if err := ctrlManager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		klog.Fatalf("Unable to set up health check: %v", err)
	}
	if err := ctrlManager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		klog.Fatalf("Unable to set up ready check: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := ctrlManager.Start(ctx); err != nil {
		klog.Fatalf("Unable to start manager: %v", err)
	}
}

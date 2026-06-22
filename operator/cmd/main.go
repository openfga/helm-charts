package main

import (
	"flag"
	"fmt"
	"math"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/openfga/openfga-operator/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
}

func main() {
	var (
		leaderElect     bool
		watchNamespace  string
		metricsAddr     string
		healthProbeAddr string
		backoffLimit    int
		activeDeadline  int
		ttlAfterFinished int
	)

	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "Namespace to watch. Defaults to the operator pod namespace.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.IntVar(&backoffLimit, "backoff-limit", int(controller.DefaultBackoffLimit), "BackoffLimit for migration Jobs.")
	flag.IntVar(&activeDeadline, "active-deadline-seconds", int(controller.DefaultActiveDeadlineSeconds), "ActiveDeadlineSeconds for migration Jobs.")
	flag.IntVar(&ttlAfterFinished, "ttl-seconds-after-finished", int(controller.DefaultTTLSecondsAfterFinished), "TTLSecondsAfterFinished for migration Jobs.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Validate flag values.
	for _, v := range []struct {
		name  string
		value int
		max   int
	}{
		{"backoff-limit", backoffLimit, math.MaxInt32},
		{"active-deadline-seconds", activeDeadline, math.MaxInt32},
		{"ttl-seconds-after-finished", ttlAfterFinished, math.MaxInt32},
	} {
		if v.value < 0 || v.value > v.max {
			fmt.Fprintf(os.Stderr, "invalid value for --%s: must be between 0 and %d\n", v.name, v.max)
			os.Exit(1)
		}
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("setup")

	// Fall back to the pod's namespace when no explicit scope is set.
	if watchNamespace == "" {
		if podNS, ok := os.LookupEnv("POD_NAMESPACE"); ok && podNS != "" {
			watchNamespace = podNS
			logger.Info("defaulting watch scope to pod namespace", "namespace", podNS)
		}
	}

	// Configure cache namespace restrictions.
	var cacheOpts cache.Options
	if watchNamespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{
			watchNamespace: {},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "openfga-operator-leader",
		Cache:                  cacheOpts,
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	reconciler := &controller.MigrationReconciler{
		Client:                  mgr.GetClient(),
		BackoffLimit:            int32(backoffLimit),
		ActiveDeadlineSeconds:   int64(activeDeadline),
		TTLSecondsAfterFinished: int32(ttlAfterFinished),
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "MigrationReconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up readiness check")
		os.Exit(1)
	}

	logger.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "problem running manager")
		os.Exit(1)
	}
}

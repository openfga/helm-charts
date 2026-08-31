package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/openfga/helm-charts/operator/internal/controller"
)

const serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
}

func main() {
	var (
		leaderElect      bool
		watchNamespace   string
		metricsAddr      string
		healthProbeAddr  string
		backoffLimit     int
		activeDeadline   int
		ttlAfterFinished int
	)

	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "Namespace to watch. Defaults to the operator pod namespace.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.IntVar(&backoffLimit, "backoff-limit", int(controller.DefaultBackoffLimit), "BackoffLimit for migration Jobs.")
	flag.IntVar(&activeDeadline, "active-deadline-seconds", int(controller.DefaultActiveDeadlineSeconds), "ActiveDeadlineSeconds for migration Jobs.")
	flag.IntVar(&ttlAfterFinished, "ttl-seconds-after-finished", int(controller.DefaultTTLSecondsAfterFinished), "Diagnostic retention in seconds for completed and failed migration Jobs.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if err := validateJobOptions(backoffLimit, activeDeadline, ttlAfterFinished); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("setup")

	watchNamespace, err := resolveWatchNamespace(
		watchNamespace,
		serviceAccountNamespacePath,
		os.LookupEnv,
		os.ReadFile,
	)
	if err != nil {
		logger.Error(err, "unable to determine watch namespace")
		os.Exit(1)
	}
	logger.Info("using namespace-scoped cache", "namespace", watchNamespace)

	// Configure cache namespace restrictions.
	cacheOpts := cache.Options{
		DefaultNamespaces: map[string]cache.Config{
			watchNamespace: {},
		},
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

func validateJobOptions(backoffLimit, activeDeadline, ttlAfterFinished int) error {
	for _, option := range []struct {
		name  string
		value int64
		min   int64
		max   int64
	}{
		{name: "backoff-limit", value: int64(backoffLimit), min: 0, max: math.MaxInt32},
		{name: "active-deadline-seconds", value: int64(activeDeadline), min: 1, max: math.MaxInt64},
		{name: "ttl-seconds-after-finished", value: int64(ttlAfterFinished), min: 0, max: math.MaxInt32},
	} {
		if option.value < option.min || option.value > option.max {
			return fmt.Errorf(
				"invalid value for --%s: must be between %d and %d",
				option.name,
				option.min,
				option.max,
			)
		}
	}
	return nil
}

func resolveWatchNamespace(
	explicitNamespace string,
	namespaceFile string,
	lookupEnv func(string) (string, bool),
	readFile func(string) ([]byte, error),
) (string, error) {
	if namespace := strings.TrimSpace(explicitNamespace); namespace != "" {
		return namespace, nil
	}
	if value, ok := lookupEnv("POD_NAMESPACE"); ok {
		if namespace := strings.TrimSpace(value); namespace != "" {
			return namespace, nil
		}
	}

	value, err := readFile(namespaceFile)
	if err != nil {
		return "", fmt.Errorf(
			"POD_NAMESPACE is empty and reading service-account namespace file %q: %w",
			namespaceFile,
			err,
		)
	}
	if namespace := strings.TrimSpace(string(value)); namespace != "" {
		return namespace, nil
	}
	return "", fmt.Errorf(
		"POD_NAMESPACE and service-account namespace file %q are empty",
		namespaceFile,
	)
}

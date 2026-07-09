package operator

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// RunManager starts the controller manager and blocks until the context
// given by controller-runtime's signal handler is done.
func RunManager(version, evidenceDir, metricsAddr string) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}
	if err := (&Reconciler{
		Client:      mgr.GetClient(),
		Version:     version,
		EvidenceDir: evidenceDir,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

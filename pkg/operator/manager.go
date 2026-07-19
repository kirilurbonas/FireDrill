package operator

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kirilurbonas/FireDrill/pkg/gc"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// RunManager starts the controller manager and blocks until the context
// given by controller-runtime's signal handler is done.
func RunManager(version, evidenceDir, metricsAddr string, maxConcurrent int) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: ":8081",
		// Rolling updates overlap old and new pods; without leader election
		// both would run the same drills simultaneously.
		LeaderElection:   true,
		LeaderElectionID: "firedrill-operator.firedrill.dev",
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}
	if err := (&Reconciler{
		Client:      mgr.GetClient(),
		Version:     version,
		EvidenceDir: evidenceDir,
		Recorder:    mgr.GetEventRecorder("firedrill"),
	}).SetupWithManager(mgr, maxConcurrent); err != nil {
		return fmt.Errorf("setting up controller: %w", err)
	}
	// Startup GC: reap sandboxes orphaned by a previous operator crash.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		res, err := gc.SweepKubernetes(ctx, gc.Options{OlderThan: time.Hour})
		if err != nil {
			ctrl.Log.Error(err, "startup sandbox gc failed")
			return nil // GC failure must not stop the operator
		}
		if len(res.Reaped) > 0 {
			ctrl.Log.Info("startup gc reaped orphaned sandboxes", "reaped", res.Reaped)
		}
		return nil
	})); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

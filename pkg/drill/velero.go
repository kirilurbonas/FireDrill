package drill

import (
	"context"
	"fmt"
	"time"

	velero "github.com/kirilurbonas/FireDrill/pkg/drivers/velero"
	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

// runVelero executes a namespace-level drill: restore a Velero backup into
// an ephemeral namespace, verify the workloads, and tear the namespace down.
func runVelero(ctx context.Context, d *spec.Drill, opts Options, e *report.Evidence) (*report.Evidence, string, error) {
	p := opts.Printer
	from := d.Spec.Source.From

	// 1+2. Validate the backup and provision the ephemeral namespace.
	t0 := time.Now()
	vd, err := velero.Prepare(ctx, d.Metadata.Name, from.Backup, from.Namespace, d.Spec.Sandbox.TTL.Duration)
	if err != nil {
		return nil, "", fmt.Errorf("preparing velero drill: %w", err)
	}
	defer func() {
		if derr := vd.Destroy(context.Background()); derr != nil && p != nil {
			p.Info("warning: namespace teardown: %v", derr)
		}
		e.Sandbox.Destroyed = vd.WasDestroyed()
	}()

	e.Backup.URI = fmt.Sprintf("velero://%s (namespace %s)", from.Backup, from.Namespace)
	e.Backup.ModTime = time.Now().Add(-vd.BackupAge).UTC()
	e.Backup.AgeSecs = vd.BackupAge.Seconds()
	e.Sandbox.Image = "namespace:" + vd.TargetNS
	if p != nil {
		p.Step(fmt.Sprintf("provision namespace  %s", vd.TargetNS),
			fmt.Sprintf("ok   %.1fs", time.Since(t0).Seconds()), true)
	}

	// Cap the whole drill at the sandbox TTL, like engine drills.
	ctx, cancel := context.WithTimeout(ctx, d.Spec.Sandbox.TTL.Duration)
	defer cancel()

	// 3. Restore via Velero, timed.
	res, restoreErr := vd.Restore(ctx)
	var restoreDur time.Duration
	if res != nil {
		restoreDur = res.Duration
		e.Measured.RestoreSeconds = res.Duration.Seconds()
	}
	if p != nil {
		if restoreErr == nil {
			p.Step(fmt.Sprintf("restore  velero backup %s → %s", from.Backup, vd.TargetNS),
				fmt.Sprintf("ok   %s", restoreDur.Round(time.Second)), true)
		} else {
			p.Step(fmt.Sprintf("restore  velero backup %s", from.Backup), "FAIL", false)
		}
	}

	// 4. Verify against the restored namespace.
	e.Checks = verify.Run(ctx, nil, d.Spec.Verify, verify.Context{
		RestoreErr: restoreErr,
		BackupAge:  vd.BackupAge,
		RTO:        d.Spec.Objectives.RTO.Duration,
		K8s:        vd.Clients(),
		Namespace:  vd.TargetNS,
	})
	if p != nil {
		for _, r := range e.Checks {
			status := "PASS"
			switch {
			case r.Skipped:
				status = "SKIP"
			case !r.Passed:
				status = "FAIL"
			}
			p.Step(fmt.Sprintf("verify   %s  (%s)", r.Name, r.Detail), status, r.Passed || r.Skipped)
		}
	}

	return finalize(ctx, d, opts, e, restoreErr, restoreDur, vd.BackupAge)
}

// Package drill orchestrates one recovery drill end to end:
// fetch backup → provision sandbox → restore → verify → report → destroy.
package drill

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver

	"github.com/kirilurbonas/FireDrill/pkg/drivers/postgres"
	"github.com/kirilurbonas/FireDrill/pkg/report"
	sbdocker "github.com/kirilurbonas/FireDrill/pkg/sandbox/docker"
	"github.com/kirilurbonas/FireDrill/pkg/source"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

// Options tune a single run.
type Options struct {
	Printer     *report.Printer
	EvidenceDir string // overrides spec report.dir when non-empty
	KeyDir      string // signing key location; empty = default
	Version     string
}

// Run executes the drill and returns the evidence. A non-nil error means the
// drill could not execute; a failed-but-executed drill returns evidence with
// Verified=false and nil error.
func Run(ctx context.Context, d *spec.Drill, opts Options) (*report.Evidence, string, error) {
	p := opts.Printer
	e := &report.Evidence{
		Drill:     d.Metadata.Name,
		Tool:      "firedrill " + opts.Version,
		StartedAt: time.Now().UTC(),
	}
	e.Objectives.RTO = d.Spec.Objectives.RTO.String()
	e.Objectives.RPO = d.Spec.Objectives.RPO.String()
	e.Sandbox.Provider = d.Spec.Sandbox.Provider
	e.Sandbox.Image = d.Spec.Sandbox.Image
	e.Controls = d.Spec.Report.Controls

	// 1. Fetch the backup (read-only).
	backup, err := source.Fetch(ctx, d.Spec.Source.From)
	if err != nil {
		return nil, "", fmt.Errorf("fetching backup: %w", err)
	}
	defer func() { _ = backup.Cleanup() }()
	backupAge := time.Since(backup.ModTime)
	e.Backup.URI = d.Spec.Source.From.URI
	e.Backup.ModTime = backup.ModTime.UTC()
	e.Backup.AgeSecs = backupAge.Seconds()
	e.Backup.Bytes = backup.Size

	// 2. Provision the isolated sandbox. Destroy is guaranteed via defer and
	// backstopped by the in-provider TTL watchdog.
	t0 := time.Now()
	sb, err := sbdocker.Provision(ctx, sbdocker.Config{
		Image: d.Spec.Sandbox.Image,
		TTL:   d.Spec.Sandbox.TTL.Duration,
		Name:  d.Metadata.Name,
	})
	if err != nil {
		return nil, "", fmt.Errorf("provisioning sandbox: %w", err)
	}
	defer func() {
		if derr := sb.Destroy(context.Background()); derr != nil && p != nil {
			p.Info("warning: sandbox teardown: %v", derr)
		}
		e.Sandbox.Destroyed = sb.Destroyed
	}()
	if p != nil {
		p.Step(fmt.Sprintf("provision sandbox  %s %s", d.Spec.Sandbox.Provider, d.Spec.Sandbox.Image),
			fmt.Sprintf("ok   %.1fs", time.Since(t0).Seconds()), true)
	}

	// Cap the whole drill at the sandbox TTL: past it the sandbox is gone anyway.
	ctx, cancel := context.WithTimeout(ctx, d.Spec.Sandbox.TTL.Duration)
	defer cancel()

	// 3. Restore, timed. Restore failure is a drill result, not an execution error.
	res, restoreErr := postgres.Restore(ctx, sb, backup.Path)
	if res != nil {
		e.Measured.RestoreSeconds = res.Duration.Seconds()
	}
	if p != nil {
		if restoreErr == nil {
			p.Step(fmt.Sprintf("restore  %s", d.Spec.Source.From.URI),
				fmt.Sprintf("ok   %s", res.Duration.Round(time.Second)), true)
		} else {
			p.Step(fmt.Sprintf("restore  %s", d.Spec.Source.From.URI), "FAIL", false)
		}
	}

	// 4. Verify.
	var db *sql.DB
	if restoreErr == nil {
		db, err = sql.Open("pgx", sb.DSN())
		if err != nil {
			return nil, "", fmt.Errorf("connecting to sandbox: %w", err)
		}
		defer func() { _ = db.Close() }()
	}
	e.Checks = verify.Run(ctx, db, d.Spec.Verify, verify.Context{
		RestoreErr: restoreErr,
		BackupAge:  backupAge,
		RTO:        d.Spec.Objectives.RTO.Duration,
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

	// 5. Objectives.
	e.Measured.RTOMet = restoreErr == nil && res.Duration <= d.Spec.Objectives.RTO.Duration
	e.Measured.RPOMet = backupAge <= d.Spec.Objectives.RPO.Duration

	e.Verified = restoreErr == nil && e.Measured.RTOMet && e.Measured.RPOMet
	for _, r := range e.Checks {
		if !r.Passed && !r.Skipped {
			e.Verified = false
		}
		if r.Skipped {
			e.Verified = false // skipped checks are unproven, not passing
		}
	}
	if restoreErr != nil {
		e.Error = restoreErr.Error()
	}

	// 6. Evidence + signature.
	e.FinishedAt = time.Now().UTC()
	dir := d.Spec.Report.Dir
	if opts.EvidenceDir != "" {
		dir = opts.EvidenceDir
	}
	path, err := e.Write(dir)
	if err != nil {
		return e, "", fmt.Errorf("writing evidence: %w", err)
	}
	if d.Spec.Report.Sign {
		keyDir := opts.KeyDir
		if keyDir == "" {
			keyDir, err = report.DefaultKeyDir()
			if err != nil {
				return e, path, err
			}
		}
		priv, err := report.LoadPrivateKey(keyDir)
		if err != nil {
			return e, path, err
		}
		if _, err := report.Sign(path, priv); err != nil {
			return e, path, fmt.Errorf("signing evidence: %w", err)
		}
	}
	return e, path, nil
}

// Package operator reconciles RecoveryDrill custom resources: it schedules
// and runs recovery drills in-cluster and records the outcome in status.
//
// The controller works on unstructured objects and converts the CR spec to
// the same spec.Drill the CLI uses (the CRD spec IS the firedrill.yaml
// spec), so CLI and operator can never drift.
package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kirilurbonas/FireDrill/pkg/drill"
	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// GVK of the RecoveryDrill custom resource.
var GVK = schema.GroupVersionKind{Group: "firedrill.dev", Version: "v1", Kind: "RecoveryDrill"}

// Reconciler runs due drills and updates CR status.
type Reconciler struct {
	client.Client
	Version string
	// EvidenceDir is where evidence JSON is written inside the operator pod.
	EvidenceDir string
	// Recorder emits Kubernetes Events on the RecoveryDrill CR (optional).
	Recorder events.EventRecorder
	// Now is stubbed in tests.
	Now func() time.Time
}

// event emits a Kubernetes Event when a recorder is configured.
func (r *Reconciler) event(obj *unstructured.Unstructured, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, eventType, reason, "Reconcile", "%s", message)
	}
}

// Reconcile implements the drill scheduling loop for one RecoveryDrill.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GVK)
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	d, err := drillFromCR(obj)
	if err != nil {
		// Invalid spec: surface in status, don't requeue (edits retrigger).
		_ = r.patchStatus(ctx, obj, map[string]any{"phase": "Invalid", "message": err.Error()})
		r.event(obj, corev1.EventTypeWarning, "InvalidSpec", err.Error())
		logger.Error(err, "invalid RecoveryDrill spec")
		return ctrl.Result{}, nil
	}

	now := r.now()
	lastRun, _ := time.Parse(time.RFC3339, str(obj.Object, "status", "lastRunTime"))

	// Decide whether a run is due.
	var nextRun time.Time
	if d.Spec.Schedule == "" {
		// No schedule: run once, then wait for spec changes.
		if !lastRun.IsZero() && str(obj.Object, "status", "observedGeneration") == fmt.Sprint(obj.GetGeneration()) {
			return ctrl.Result{}, nil
		}
	} else {
		sched, err := cron.ParseStandard(d.Spec.Schedule)
		if err != nil {
			_ = r.patchStatus(ctx, obj, map[string]any{"phase": "Invalid", "message": "bad schedule: " + err.Error()})
			return ctrl.Result{}, nil
		}
		base := lastRun
		if base.IsZero() {
			base = obj.GetCreationTimestamp().Time
		}
		nextRun = sched.Next(base)
		if now.Before(nextRun) {
			return ctrl.Result{RequeueAfter: nextRun.Sub(now)}, nil
		}
	}

	// Run the drill now. Drills take minutes; status shows progress.
	_ = r.patchStatus(ctx, obj, map[string]any{"phase": "Running", "message": "drill in progress"})
	logger.Info("running recovery drill", "drill", d.Metadata.Name)

	e, _, err := drill.Run(ctx, d, drill.Options{EvidenceDir: r.EvidenceDir, Version: r.Version})
	status := map[string]any{
		"lastRunTime":        r.now().UTC().Format(time.RFC3339),
		"observedGeneration": fmt.Sprint(obj.GetGeneration()),
	}
	switch {
	case err != nil:
		status["phase"] = "Error"
		status["message"] = err.Error()
		status["verified"] = false
		r.event(obj, corev1.EventTypeWarning, "DrillError", err.Error())
	case e.Verified:
		status["phase"] = "Verified"
		status["message"] = "recovery verified"
		status["verified"] = true
		r.event(obj, corev1.EventTypeNormal, "DrillVerified",
			fmt.Sprintf("recovery verified: restore %.0fs, backup age %.0fm",
				e.Measured.RestoreSeconds, e.Backup.AgeSecs/60))
	default:
		status["phase"] = "Failed"
		status["message"] = failureSummary(e)
		status["verified"] = false
		r.event(obj, corev1.EventTypeWarning, "DrillFailed", failureSummary(e))
	}
	if e != nil {
		status["measuredRestoreSeconds"] = e.Measured.RestoreSeconds
		status["backupAgeSeconds"] = e.Backup.AgeSecs
		status["rtoMet"] = e.Measured.RTOMet
		status["rpoMet"] = e.Measured.RPOMet
	}
	if perr := r.patchStatus(ctx, obj, status); perr != nil {
		return ctrl.Result{}, perr
	}

	if d.Spec.Schedule != "" {
		sched, _ := cron.ParseStandard(d.Spec.Schedule)
		return ctrl.Result{RequeueAfter: time.Until(sched.Next(r.now()))}, nil
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) patchStatus(ctx context.Context, obj *unstructured.Unstructured, fields map[string]any) error {
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(GVK)
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
		return err
	}
	existing, _, _ := unstructured.NestedMap(fresh.Object, "status")
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range fields {
		existing[k] = v
	}
	if err := unstructured.SetNestedMap(fresh.Object, existing, "status"); err != nil {
		return err
	}
	return r.Status().Update(ctx, fresh)
}

// drillFromCR converts the CR to the CLI's spec.Drill. The CR spec is JSON;
// YAML is a superset, so the spec package's decoder (with its Duration
// parsing and validation) applies directly.
func drillFromCR(obj *unstructured.Unstructured) (*spec.Drill, error) {
	raw, ok, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var s spec.Spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing spec: %w", err)
	}
	d := &spec.Drill{
		APIVersion: spec.APIVersion,
		Kind:       spec.Kind,
		Metadata:   spec.Metadata{Name: obj.GetName()},
		Spec:       s,
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

func failureSummary(e *report.Evidence) string {
	var names []string
	for _, c := range e.Checks {
		if !c.Passed && !c.Skipped {
			names = append(names, c.Name)
		}
	}
	if !e.Measured.RTOMet {
		names = append(names, "rto-objective")
	}
	if !e.Measured.RPOMet {
		names = append(names, "rpo-objective")
	}
	if len(names) == 0 {
		return "recovery not verified"
	}
	return "failed: " + fmt.Sprint(names)
}

// str reads a nested string field, tolerating absence.
func str(obj map[string]any, fields ...string) string {
	v, _, _ := unstructured.NestedString(obj, fields...)
	return v
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GVK)
	return ctrl.NewControllerManagedBy(mgr).For(obj).Complete(r)
}

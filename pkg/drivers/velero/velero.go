// Package velero runs namespace-level recovery drills: it restores a Velero
// backup into an ephemeral namespace via a Restore CR with namespaceMapping,
// so the original namespace is never touched. Talks to Velero's CRDs through
// the dynamic client — no velero CLI required.
package velero

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	corev1 "k8s.io/api/core/v1"

	sbk8s "github.com/kirilurbonas/FireDrill/pkg/sandbox/kubernetes"
)

var (
	backupGVR  = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}
	restoreGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "restores"}
)

// VeleroNamespace is where Velero's server-side objects live.
const VeleroNamespace = "velero"

// Drill is a namespace-restore drill session.
type Drill struct {
	dyn       dynamic.Interface
	cli       kubernetes.Interface
	SourceNS  string
	TargetNS  string // ephemeral namespace the backup restores into
	Backup    string
	BackupAge time.Duration

	restoreName string
	destroyOnce sync.Once
	destroyErr  error
	ttlCancel   context.CancelFunc
	destroyed   bool
}

// Clients exposes the typed client for verify checks.
func (d *Drill) Clients() kubernetes.Interface { return d.cli }
func (d *Drill) WasDestroyed() bool            { return d.destroyed }

// Prepare validates the backup exists and completed, records its age, and
// creates the ephemeral target namespace with a deny-egress NetworkPolicy.
func Prepare(ctx context.Context, drillName, backup, sourceNS string, ttl time.Duration) (*Drill, error) {
	restCfg, _, err := sbk8s.LoadConfig()
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	cli, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	b, err := dyn.Resource(backupGVR).Namespace(VeleroNamespace).Get(ctx, backup, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading Velero backup %q: %w (is Velero installed and the backup name correct?)", backup, err)
	}
	phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
	if phase != "Completed" {
		return nil, fmt.Errorf("velero backup %q is in phase %q, want Completed", backup, phase)
	}
	// completionTimestamp drives freshness/RPO; fall back to creation time.
	age := time.Since(b.GetCreationTimestamp().Time)
	if ts, ok, _ := unstructured.NestedString(b.Object, "status", "completionTimestamp"); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			age = time.Since(t)
		}
	}

	d := &Drill{
		dyn:       dyn,
		cli:       cli,
		SourceNS:  sourceNS,
		TargetNS:  fmt.Sprintf("firedrill-%s-%s", sanitize(drillName), randomHex(4)),
		Backup:    backup,
		BackupAge: age,
	}

	if _, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   d.TargetNS,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "firedrill", "firedrill/sandbox": "true"},
		},
	}, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating ephemeral namespace: %w", err)
	}
	if err := sbk8s.EnsureDenyEgressPolicy(ctx, cli, d.TargetNS); err != nil {
		_ = d.Destroy(context.Background())
		return nil, err
	}

	ttlCtx, cancel := context.WithCancel(context.Background())
	d.ttlCancel = cancel
	go func() { // #nosec G118 -- watchdog must outlive the request context to guarantee teardown
		select {
		case <-ttlCtx.Done():
		case <-time.After(ttl):
			_ = d.Destroy(context.Background())
		}
	}()
	return d, nil
}

// RestoreResult mirrors the engine drivers' result shape.
type RestoreResult struct {
	Duration time.Duration
	Phase    string
	Warnings int64
	Errors   int64
}

// Restore creates the Velero Restore CR mapped into the ephemeral namespace
// and waits for it to reach a terminal phase. The wall clock is the drill's
// measured RTO. A Failed/PartiallyFailed phase is a drill result, not a crash.
func (d *Drill) Restore(ctx context.Context) (*RestoreResult, error) {
	d.restoreName = fmt.Sprintf("%s-%s", d.TargetNS, randomHex(3))
	restore := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata": map[string]any{
			"name":      d.restoreName,
			"namespace": VeleroNamespace,
			"labels":    map[string]any{"app.kubernetes.io/managed-by": "firedrill"},
		},
		"spec": map[string]any{
			"backupName":         d.Backup,
			"includedNamespaces": []any{d.SourceNS},
			"namespaceMapping":   map[string]any{d.SourceNS: d.TargetNS},
			// The drill must never write back into the source namespace.
			"existingResourcePolicy": "none",
		},
	}}

	start := time.Now()
	if _, err := d.dyn.Resource(restoreGVR).Namespace(VeleroNamespace).Create(ctx, restore, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating Velero restore: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		r, err := d.dyn.Resource(restoreGVR).Namespace(VeleroNamespace).Get(ctx, d.restoreName, metav1.GetOptions{})
		if err != nil {
			continue // transient API hiccups shouldn't kill the drill
		}
		phase, _, _ := unstructured.NestedString(r.Object, "status", "phase")
		// Terminal phases per Velero: Completed, PartiallyFailed, Failed,
		// FailedValidation. Anything else (New/InProgress/WaitingForPluginOperations)
		// keeps polling. Treat every Failed* variant as terminal so an
		// unexpected phase can never hang the drill until the TTL.
		terminal := phase == "Completed" || phase == "PartiallyFailed" || strings.HasPrefix(phase, "Failed")
		if terminal {
			warnings, _, _ := unstructured.NestedInt64(r.Object, "status", "warnings")
			errs, _, _ := unstructured.NestedInt64(r.Object, "status", "errors")
			res := &RestoreResult{Duration: time.Since(start), Phase: phase, Warnings: warnings, Errors: errs}
			if phase != "Completed" {
				verrs, _, _ := unstructured.NestedStringSlice(r.Object, "status", "validationErrors")
				detail := ""
				if len(verrs) > 0 {
					detail = ": " + strings.Join(verrs, "; ")
				}
				return res, fmt.Errorf("velero restore phase %s (%d errors, %d warnings)%s", phase, errs, warnings, detail)
			}
			return res, nil
		}
	}
}

// Destroy deletes the ephemeral namespace and the Restore CR. Idempotent.
func (d *Drill) Destroy(ctx context.Context) error {
	d.destroyOnce.Do(func() {
		if d.ttlCancel != nil {
			d.ttlCancel()
		}
		if err := d.cli.CoreV1().Namespaces().Delete(ctx, d.TargetNS, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			d.destroyErr = fmt.Errorf("deleting ephemeral namespace: %w", err)
		}
		if d.restoreName != "" {
			if err := d.dyn.Resource(restoreGVR).Namespace(VeleroNamespace).Delete(ctx, d.restoreName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) && d.destroyErr == nil {
				d.destroyErr = fmt.Errorf("deleting restore CR: %w", err)
			}
		}
		d.destroyed = d.destroyErr == nil
	})
	return d.destroyErr
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return hex.EncodeToString(b)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, s)
}

//go:build e2e

package operator_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kirilurbonas/FireDrill/pkg/operator"
)

// TestE2EOperator drives the whole operator path against a real cluster:
// applies the CRD, starts the manager (out-of-cluster), creates a
// RecoveryDrill CR, and waits for the drill to run and status to say
// Verified. Skips when no cluster is reachable.
func TestE2EOperator(t *testing.T) {
	if out, err := exec.Command("kubectl", "cluster-info").CombinedOutput(); err != nil {
		t.Skipf("no reachable kubernetes cluster: %v (%s)", err, string(out))
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- repoRoot derives from the test file location
	if out, err := exec.Command("kubectl", "apply", "-f", filepath.Join(repoRoot, "deploy/crd.yaml")).CombinedOutput(); err != nil {
		t.Fatalf("applying crd: %v (%s)", err, string(out))
	}
	// The CRD must be Established before CRs of its kind can be created;
	// creating immediately after apply races discovery.
	if out, err := exec.Command("kubectl", "wait", "--for=condition=Established",
		"crd/recoverydrills.firedrill.dev", "--timeout=60s").CombinedOutput(); err != nil {
		t.Fatalf("waiting for crd: %v (%s)", err, string(out))
	}

	dir := t.TempDir()
	dump := filepath.Join(dir, "demo.sql")
	sqlText := "create table ledger (id bigserial primary key, amount bigint not null);\n" +
		"insert into ledger (amount) select g from generate_series(1, 3000) g;\n"
	if err := os.WriteFile(dump, []byte(sqlText), 0o600); err != nil {
		t.Fatal(err)
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	rec := &operator.Reconciler{
		Client:      mgr.GetClient(),
		Version:     "e2e",
		EvidenceDir: filepath.Join(dir, "evidence"),
		Recorder:    mgr.GetEventRecorder("firedrill"),
	}
	if err := rec.SetupWithManager(mgr, 2); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager stopped: %v", err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	cr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "firedrill.dev/v1",
		"kind":       "RecoveryDrill",
		"metadata":   map[string]any{"name": "e2e-operator", "namespace": "default"},
		"spec": map[string]any{
			// no schedule: run once
			"objectives": map[string]any{"rto": "10m", "rpo": "24h"},
			"source": map[string]any{
				"driver": "postgres",
				"from":   map[string]any{"type": "file", "uri": dump},
			},
			"sandbox": map[string]any{"provider": "kubernetes", "image": "postgres:16.10-alpine", "ttl": "10m"},
			"verify": []any{
				map[string]any{"restoreSucceeded": map[string]any{}},
				map[string]any{"rowCount": map[string]any{"query": "select count(*) from ledger", "min": 3000}},
			},
			"report": map[string]any{"sign": false},
		},
	}}
	cr.SetGroupVersionKind(operator.GVK)
	_ = mgr.GetClient().Delete(ctx, cr) // clean any previous run
	if err := mgr.GetClient().Create(ctx, cr); err != nil {
		t.Fatalf("creating RecoveryDrill: %v", err)
	}
	defer func() {
		_ = mgr.GetClient().Delete(context.Background(), cr, client.GracePeriodSeconds(0))
	}()

	key := types.NamespacedName{Name: "e2e-operator", Namespace: "default"}
	deadline := time.Now().Add(6 * time.Minute)
	var phase, msg string
	for time.Now().Before(deadline) {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(operator.GVK)
		if err := mgr.GetClient().Get(ctx, key, got); err == nil {
			phase, _, _ = unstructured.NestedString(got.Object, "status", "phase")
			msg, _, _ = unstructured.NestedString(got.Object, "status", "message")
			if phase == "Verified" {
				verified, _, _ := unstructured.NestedBool(got.Object, "status", "verified")
				if !verified {
					t.Fatal("phase Verified but verified=false")
				}
				// A DrillVerified event must land on the CR (recorder is async).
				evDeadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(evDeadline) {
					out, _ := exec.Command("kubectl", "get", "events", "-n", "default",
						"--field-selector", "involvedObject.name=e2e-operator", "-o", "name").CombinedOutput()
					if len(strings.TrimSpace(string(out))) > 0 {
						return
					}
					time.Sleep(2 * time.Second)
				}
				t.Fatal("no Kubernetes event recorded for the drill")
			}
			if phase == "Error" || phase == "Failed" || phase == "Invalid" {
				t.Fatalf("drill ended in phase %s: %s", phase, msg)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out; last phase %q (%s)", phase, msg)
}

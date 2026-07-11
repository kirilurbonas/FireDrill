//go:build e2e

package drill_test

import (
	"context"

	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/drill"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// TestE2EVeleroDrill runs a namespace-level drill against a real Velero
// installation (see examples/velero/setup-velero-kind.sh). Skips when no
// cluster is reachable or Velero + the demo backup are absent.
func TestE2EVeleroDrill(t *testing.T) {
	if out, err := exec.Command("kubectl", "cluster-info").CombinedOutput(); err != nil {
		t.Skipf("no reachable kubernetes cluster: %v (%s)", err, string(out))
	}
	if err := exec.Command("kubectl", "get", "crd", "backups.velero.io").Run(); err != nil {
		t.Skip("velero not installed in cluster")
	}
	if out, err := exec.Command("kubectl", "-n", "velero", "get", "backup", "shop-backup").CombinedOutput(); err != nil {
		t.Skipf("demo backup missing (run examples/velero/setup-velero-kind.sh): %s", string(out))
	}

	doc := `
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-velero }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: velero
    from: { type: velero, backup: shop-backup, namespace: shop }
  sandbox: { provider: kubernetes, ttl: 15m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - podsReady: { timeout: 5m }
    - resourceCount: { kind: deployments, min: 1 }
    - resourceCount: { kind: configmaps, min: 1 }
    - resourceCount: { kind: secrets, min: 1 }
  report: { sign: false }
`
	d, err := spec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dir := t.TempDir()
	e, _, err := drill.Run(ctx, d, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("drill.Run: %v", err)
	}
	if !e.Verified {
		t.Fatalf("velero drill not verified: %+v", e)
	}
	if !e.Sandbox.Destroyed {
		t.Error("ephemeral namespace was not destroyed")
	}
	if e.Measured.RestoreSeconds <= 0 {
		t.Error("restore duration not measured")
	}

	// The ephemeral namespace must be gone (or at least Terminating).
	out, _ := exec.Command("kubectl", "get", "ns", "-o", "name").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "namespace/firedrill-e2e-velero-") {
			phase, _ := exec.Command("kubectl", "get", strings.TrimSpace(line), "-o", "jsonpath={.status.phase}").CombinedOutput() // #nosec G204 -- line comes from kubectl output in this test
			if string(phase) != "Terminating" {
				t.Errorf("leftover namespace %s in phase %s", line, string(phase))
			}
		}
	}

	// Nonexistent backup must be an execution error (exit-2 class), not a crash.
	bad := strings.Replace(doc, "backup: shop-backup", "backup: does-not-exist", 1)
	db, err := spec.Parse(strings.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := drill.Run(ctx, db, drill.Options{EvidenceDir: filepath.Join(dir, "evidence2"), Version: "e2e-test"}); err == nil {
		t.Error("expected error for nonexistent backup")
	} else if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the backup: %v", err)
	}
}

//go:build e2e

package drill_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/drill"
	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// TestE2EDrill runs the complete drill loop against real Docker: writes a
// plain-SQL backup, restores it into a fresh sandbox, verifies, signs.
func TestE2EDrill(t *testing.T) {
	dir := t.TempDir()

	dump := filepath.Join(dir, "demo.sql")
	var sb strings.Builder
	sb.WriteString("create table ledger (id bigserial primary key, amount bigint not null);\n")
	sb.WriteString("insert into ledger (amount) select g from generate_series(1, 5000) g;\n")
	if err := os.WriteFile(dump, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := report.GenerateKeypair(dir); err != nil {
		t.Fatal(err)
	}

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    from: { type: file, uri: %s }
  sandbox: { provider: docker, image: "postgres:16.10-alpine", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - rowCount: { query: "select count(*) from ledger", min: 5000 }
    - checksum: { table: ledger, column: id }
    - smoke: { sql: "select 1 from ledger where amount = 1", expectRows: "==1" }
  report:
    sign: true
    html: true
    sinks:
      - { type: prometheus, textfileDir: %s }
`, dump, filepath.Join(dir, "metrics"))

	d, err := spec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e, path, err := drill.Run(ctx, d, drill.Options{
		EvidenceDir: filepath.Join(dir, "evidence"),
		KeyDir:      dir,
		Version:     "e2e-test",
	})
	if err != nil {
		t.Fatalf("drill.Run: %v", err)
	}
	if !e.Verified {
		t.Fatalf("drill not verified: %+v", e)
	}
	if !e.Sandbox.Destroyed {
		t.Error("sandbox was not destroyed")
	}
	if e.Measured.RestoreSeconds <= 0 {
		t.Error("restore duration not measured")
	}
	if err := report.Verify(path, nil); err != nil {
		t.Errorf("evidence signature invalid: %v", err)
	}

	htmlPath := strings.TrimSuffix(path, ".json") + ".html"
	html, err := os.ReadFile(htmlPath) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatalf("html report not written: %v", err)
	}
	if !strings.Contains(string(html), "RECOVERY VERIFIED") {
		t.Error("html report missing verdict")
	}

	prom, err := os.ReadFile(filepath.Join(dir, "metrics", "firedrill-e2e.prom")) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatalf("metrics textfile not written: %v", err)
	}
	if !strings.Contains(string(prom), `firedrill_drill_verified{drill="e2e"} 1`) {
		t.Errorf("metrics missing verified gauge:\n%s", prom)
	}
}

// TestE2EMySQLDrill runs the drill loop against a MySQL sandbox, proving the
// driver abstraction: same spec shape, different engine.
func TestE2EMySQLDrill(t *testing.T) {
	dir := t.TempDir()

	dump := filepath.Join(dir, "demo.sql")
	sqlText := "create table ledger (id bigint primary key auto_increment, amount bigint not null);\n" +
		"insert into ledger (amount) values " + rowValues(2000) + ";\n"
	if err := os.WriteFile(dump, []byte(sqlText), 0o600); err != nil {
		t.Fatal(err)
	}

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-mysql }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: mysql
    from: { type: file, uri: %s }
  sandbox: { provider: docker, image: "mysql:8.4", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "select count(*) from ledger", min: 2000 }
    - checksum: { table: ledger, column: id }
    - smoke: { sql: "select 1 from ledger where amount = 1", expectRows: "==1" }
  report: { sign: false }
`, dump)

	d, err := spec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	e, _, err := drill.Run(ctx, d, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("drill.Run: %v", err)
	}
	if !e.Verified {
		t.Fatalf("mysql drill not verified: %+v", e)
	}
	if !e.Sandbox.Destroyed {
		t.Error("sandbox was not destroyed")
	}
}

// TestE2EKubernetesDrill runs the drill loop with the kubernetes sandbox
// provider (pod + exec + port-forward). Skips when no cluster is reachable.
func TestE2EKubernetesDrill(t *testing.T) {
	if out, err := exec.Command("kubectl", "cluster-info").CombinedOutput(); err != nil {
		t.Skipf("no reachable kubernetes cluster: %v (%s)", err, string(out))
	}
	dir := t.TempDir()

	dump := filepath.Join(dir, "demo.sql")
	sqlText := "create table ledger (id bigserial primary key, amount bigint not null);\n" +
		"insert into ledger (amount) select g from generate_series(1, 5000) g;\n"
	if err := os.WriteFile(dump, []byte(sqlText), 0o600); err != nil {
		t.Fatal(err)
	}

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-k8s }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    from: { type: file, uri: %s }
  sandbox: { provider: kubernetes, image: "postgres:16.10-alpine", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "select count(*) from ledger", min: 5000 }
    - checksum: { table: ledger, column: id }
  report: { sign: false }
`, dump)

	d, err := spec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	e, _, err := drill.Run(ctx, d, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("drill.Run: %v", err)
	}
	if !e.Verified {
		t.Fatalf("k8s drill not verified: %+v", e)
	}
	if !e.Sandbox.Destroyed {
		t.Error("sandbox pod was not destroyed")
	}
}

func rowValues(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d)", i)
	}
	return b.String()
}

// TestE2ECorruptBackup proves a garbage backup yields a failed (not verified)
// drill — the whole point of the product.
func TestE2ECorruptBackup(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "bad.sql")
	if err := os.WriteFile(dump, []byte("this is not sql; select broken from;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-corrupt }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    from: { type: file, uri: %s }
  sandbox: { provider: docker, image: "postgres:16.10-alpine", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "select count(*) from ledger", min: 1 }
  report: { sign: false }
`, dump)

	d, err := spec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e, _, err := drill.Run(ctx, d, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("drill.Run should execute even for bad backups: %v", err)
	}
	if e.Verified {
		t.Fatal("corrupt backup must not verify")
	}
	if !e.Sandbox.Destroyed {
		t.Error("sandbox was not destroyed")
	}
}

//go:build e2e

package drill_test

import (
	"context"
	"fmt"
	"os"
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
	if err := os.WriteFile(dump, []byte(sb.String()), 0o644); err != nil {
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
  sandbox: { provider: docker, image: "postgres:16.6", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - rowCount: { query: "select count(*) from ledger", min: 5000 }
    - checksum: { table: ledger, column: id }
    - smoke: { sql: "select 1 from ledger where amount = 1", expectRows: "==1" }
  report: { sign: true }
`, dump)

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
}

// TestE2ECorruptBackup proves a garbage backup yields a failed (not verified)
// drill — the whole point of the product.
func TestE2ECorruptBackup(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "bad.sql")
	if err := os.WriteFile(dump, []byte("this is not sql; select broken from;\n"), 0o644); err != nil {
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
  sandbox: { provider: docker, image: "postgres:16.6", ttl: 10m }
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

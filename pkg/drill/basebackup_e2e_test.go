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
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// TestE2EBasebackupDrill takes a REAL pg_basebackup (tar format, WAL
// included) from a seeded Postgres and physically restores it via the
// cold-start sandbox path: untar into PGDATA, crash-recover, verify.
func TestE2EBasebackupDrill(t *testing.T) {
	dir := t.TempDir()
	name := "firedrill-bb-src"
	image := "postgres:16.10-alpine"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	// #nosec G204 -- fixed args
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=src", "-e", "POSTGRES_DB=payments",
		image).CombinedOutput(); err != nil {
		t.Skipf("cannot start source postgres: %v (%s)", err, string(out))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	// Wait ready, then seed data + a canary.
	ready := false
	for i := 0; i < 60; i++ {
		if exec.Command("docker", "exec", name, "psql", "-U", "postgres", "-d", "payments", "-c", "select 1").Run() == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		t.Fatal("source postgres never became ready")
	}
	seed := "create table ledger (id bigserial primary key, amount bigint not null);" +
		"insert into ledger (amount) select g from generate_series(1, 2000) g;" +
		"create table firedrill_canary (token text); insert into firedrill_canary values ('fd-bb-token');"
	if out, err := exec.Command("docker", "exec", name, "psql", "-U", "postgres", "-d", "payments", "-c", seed).CombinedOutput(); err != nil {
		t.Fatalf("seeding: %v (%s)", err, string(out))
	}

	// Take the physical backup: tar to stdout, WAL included.
	bb := filepath.Join(dir, "base.tar")
	f, err := os.Create(bb) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- fixed args
	cmd := exec.Command("docker", "exec", name, "pg_basebackup", "-U", "postgres", "-D", "-", "-Ft", "-X", "fetch")
	cmd.Stdout = f
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("pg_basebackup: %v (%s)", err, errBuf.String())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(bb); fi == nil || fi.Size() < 1<<20 {
		t.Fatalf("basebackup suspiciously small")
	}

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-basebackup }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    format: basebackup
    database: payments
    from: { type: file, uri: %s }
  sandbox: { provider: docker, image: "%s", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 1h }
    - rowCount: { query: "select count(*) from ledger", min: 2000 }
    - checksum: { table: ledger, column: id }
    - canary: { sql: "select token from firedrill_canary", expect: "fd-bb-token" }
  report: { sign: false }
`, bb, image)

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
		t.Fatalf("basebackup drill not verified: %+v", e)
	}
	if !e.Sandbox.Destroyed {
		t.Error("sandbox was not destroyed")
	}
	if e.Measured.RestoreSeconds <= 0 {
		t.Error("restore duration not measured")
	}
}

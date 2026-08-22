//go:build e2e

package drill_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/drill"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// TestE2EDiscoveryDrill drills what a real pipeline actually writes: a
// directory of timestamped, gzipped dumps. The newest matching one must be
// selected, decompressed and restored — and evidence must name it.
func TestE2EDiscoveryDrill(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatal(err)
	}

	write := func(name, sql string, age time.Duration) {
		t.Helper()
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte(sql)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(backups, name)
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := time.Now().Add(-age)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	table := "create table ledger (id bigserial primary key, amount bigint not null);\n"
	// Yesterday's backup, today's backup, and another drill's newer backup
	// that the glob must exclude.
	write("payments-2026-08-21.dump.gz", table+"insert into ledger (amount) select g from generate_series(1, 10) g;\n", 26*time.Hour)
	write("payments-2026-08-22.dump.gz", table+"insert into ledger (amount) select g from generate_series(1, 4000) g;\n", 2*time.Hour)
	write("orders-2026-08-22.dump.gz", table+"insert into ledger (amount) select g from generate_series(1, 7) g;\n", time.Minute)

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-latest }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    from:
      type: file
      uri: %s
      select: latest
      match: "payments-*.dump.gz"
  sandbox: { provider: docker, image: "postgres:16.10-alpine", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - rowCount: { query: "select count(*) from ledger", min: 4000 }
  report: { sign: false }
`, backups)

	d, err := spec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e, _, err := drill.Run(ctx, d, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("drill.Run: %v", err)
	}
	if !e.Verified {
		t.Fatalf("discovery drill not verified: %+v", e)
	}
	// Exactly 4000 rows proves the *right* backup was chosen: the older
	// payments dump has 10 and the orders dump 7.
	for _, c := range e.Checks {
		if c.Name == "rowCount" && !strings.HasPrefix(c.Detail, "4000 rows") {
			t.Errorf("wrong backup restored: %s", c.Detail)
		}
	}
	if !strings.HasSuffix(e.Backup.ResolvedURI, "payments-2026-08-22.dump.gz") {
		t.Errorf("evidence does not name the selected artifact: %q", e.Backup.ResolvedURI)
	}
	if e.Backup.Compression != "gzip" || e.Backup.UncompressedBytes <= e.Backup.Bytes {
		t.Errorf("expected gzip expansion recorded, got %+v", e.Backup)
	}
	// RPO is measured from the backup's own timestamp, not from when
	// FireDrill expanded it.
	if e.Backup.AgeSecs < 3600 {
		t.Errorf("backup age should reflect the artifact's mod time, got %.0fs", e.Backup.AgeSecs)
	}
}

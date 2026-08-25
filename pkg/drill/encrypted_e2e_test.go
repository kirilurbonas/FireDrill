//go:build e2e

package drill_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/kirilurbonas/FireDrill/pkg/drill"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// TestE2EEncryptedDrill drills what a security-conscious pipeline actually
// writes: pg_dump | gzip | age, discovered by prefix. The whole chain has to
// work — select the newest artifact, decrypt it, decompress it, restore it,
// and prove the data came back.
func TestE2EEncryptedDrill(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatal(err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "age.key")
	if err := os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	write := func(name, sql string, age0 time.Duration) {
		t.Helper()
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		if _, err := zw.Write([]byte(sql)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		w, err := age.Encrypt(&out, id.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, &gz); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(backups, name)
		if err := os.WriteFile(p, out.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := time.Now().Add(-age0)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	table := "create table ledger (id bigserial primary key, amount bigint not null);\n"
	canary := "create table firedrill_canary (token text); insert into firedrill_canary values ('fd-enc-token');\n"
	write("payments-2026-08-24.dump.gz.age", table+"insert into ledger (amount) select g from generate_series(1, 5) g;\n"+canary, 26*time.Hour)
	write("payments-2026-08-25.dump.gz.age", table+"insert into ledger (amount) select g from generate_series(1, 3000) g;\n"+canary, time.Hour)

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-encrypted }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    from:
      type: file
      uri: %s
      select: latest
      match: "payments-*.dump.gz.age"
      decrypt: { type: age, identityFile: %s }
  sandbox: { provider: docker, image: "postgres:16.10-alpine", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - rowCount: { query: "select count(*) from ledger", min: 3000 }
    - canary: { sql: "select token from firedrill_canary", expect: "fd-enc-token" }
  report: { sign: false }
`, backups, keyFile)

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
	for _, c := range e.Checks {
		if !c.Passed {
			t.Errorf("check %s failed: %s", c.Name, c.Detail)
		}
	}
	if !e.Verified {
		t.Fatalf("encrypted drill not verified: %+v", e)
	}
	if e.Backup.Encryption != "age" || e.Backup.Compression != "gzip" {
		t.Errorf("evidence should record both layers, got %q/%q", e.Backup.Encryption, e.Backup.Compression)
	}
	if !strings.HasSuffix(e.Backup.ResolvedURI, "payments-2026-08-25.dump.gz.age") {
		t.Errorf("selected the wrong artifact: %q", e.Backup.ResolvedURI)
	}

	// Without the key the drill must fail to execute, not silently pass.
	doc2 := strings.Replace(strings.Replace(doc, "decrypt: { type: age, identityFile: "+keyFile+" }", "{}", 1),
		"name: e2e-encrypted }", "name: e2e-encrypted-nokey }", 1)
	if d2, err := spec.Parse(strings.NewReader(doc2)); err == nil {
		if _, _, err := drill.Run(ctx, d2, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"}); err == nil {
			t.Error("a drill with no decrypt block must not succeed against encrypted backups")
		} else if !strings.Contains(err.Error(), "source.from.decrypt") {
			t.Errorf("error should tell the operator what to add, got %v", err)
		}
	}
}

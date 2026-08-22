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

const mongoImage = "mongo:8"

// TestE2EMongoDrill takes a REAL mongodump archive from a seeded MongoDB and
// drills it: restore into a throwaway sandbox, then run every check type
// through the mongosh verification engine.
func TestE2EMongoDrill(t *testing.T) {
	dir := t.TempDir()
	name := "firedrill-mongo-src"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	// #nosec G204 -- fixed args
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "MONGO_INITDB_ROOT_USERNAME=src", "-e", "MONGO_INITDB_ROOT_PASSWORD=srcpw",
		mongoImage).CombinedOutput(); err != nil {
		t.Skipf("cannot start source mongodb: %v (%s)", err, string(out))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	auth := []string{"-u", "src", "-p", "srcpw", "--authenticationDatabase", "admin"}
	ready := false
	for i := 0; i < 90; i++ {
		args := append([]string{"exec", name, "mongosh", "--quiet"}, auth...)
		if exec.Command("docker", append(args, "--eval", "db.adminCommand({ping:1})")...).Run() == nil { // #nosec G204 -- fixed args
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		t.Fatal("source mongodb never became ready")
	}

	seed := `const d = db.getSiblingDB("shop");
d.ledger.insertMany(Array.from({length: 2000}, (_, i) => ({id: i + 1, amount: i * 10, status: i % 2 ? "active" : "closed"})));
d.firedrill_canary.insertOne({token: "fd-mongo-token"});`
	args := append([]string{"exec", name, "mongosh", "--quiet"}, auth...)
	if out, err := exec.Command("docker", append(args, "--eval", seed)...).CombinedOutput(); err != nil { // #nosec G204 -- fixed args
		t.Fatalf("seeding: %v (%s)", err, string(out))
	}

	// Take the backup the way a real pipeline would, gzipped: FireDrill must
	// decompress it transparently.
	archive := filepath.Join(dir, "shop.archive.gz")
	f, err := os.Create(archive) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	dumpArgs := append([]string{"exec", name, "mongodump"}, auth...)
	dump := exec.Command("docker", append(dumpArgs, "--db", "shop", "--archive", "--gzip")...) // #nosec G204 -- fixed args
	dump.Stdout = f
	if err := dump.Run(); err != nil {
		_ = f.Close()
		t.Fatalf("mongodump: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-mongo }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: mongodb
    format: archive
    database: shop
    from: { type: file, uri: %s }
  sandbox: { provider: docker, image: %q, ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - rowCount: { query: "db.ledger.countDocuments({})", min: 2000 }
    - checksum: { table: ledger, column: id }
    - smoke: { query: "db.ledger.find({status: 'active'}).limit(3)", expectRows: "==3" }
    - canary: { query: "db.firedrill_canary.findOne().token", expect: "fd-mongo-token" }
  report: { sign: false }
`, archive, mongoImage)

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
	for _, c := range e.Checks {
		if !c.Passed {
			t.Errorf("check %s failed: %s", c.Name, c.Detail)
		}
	}
	if !e.Verified {
		t.Fatalf("mongo drill not verified: %+v", e)
	}
	if !e.Sandbox.Destroyed {
		t.Error("sandbox was not destroyed")
	}
	// The archive was gzipped, so evidence must say so.
	if e.Backup.Compression != "gzip" || e.Backup.UncompressedBytes <= 0 {
		t.Errorf("expected gzip compression recorded, got %+v", e.Backup)
	}

	// A drill pointed at the wrong database must not quietly verify: the
	// collections do not exist there.
	doc2 := strings.Replace(strings.Replace(doc, "database: shop", "database: nosuchdb", 1),
		"name: e2e-mongo }", "name: e2e-mongo-wrongdb }", 1)
	d2, err := spec.Parse(strings.NewReader(doc2))
	if err != nil {
		t.Fatal(err)
	}
	e2, _, err := drill.Run(ctx, d2, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("wrong-db drill should still execute: %v", err)
	}
	if e2.Verified {
		t.Error("a drill against an empty database must not verify")
	}
}

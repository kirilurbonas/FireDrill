//go:build e2e

package drill_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/kirilurbonas/FireDrill/pkg/drill"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// TestE2ES3MinIODrill exercises the s3 source end-to-end against a local
// MinIO container using a custom endpoint: upload a dump, drill from
// s3://…, verify. Covers endpoint + path-style addressing + LastModified
// driving the freshness check.
func TestE2ES3MinIODrill(t *testing.T) {
	const port = "19100"
	name := "firedrill-minio-e2e"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	// #nosec G204 -- fixed args
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-p", "127.0.0.1:"+port+":9000",
		"-e", "MINIO_ROOT_USER=minio", "-e", "MINIO_ROOT_PASSWORD=minio123",
		"minio/minio:latest", "server", "/data").CombinedOutput(); err != nil {
		t.Skipf("cannot start MinIO container: %v (%s)", err, string(out))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	endpoint := "http://127.0.0.1:" + port
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// Point the default AWS chain at MinIO credentials for both the test
	// uploader and the drill's fetcher.
	t.Setenv("AWS_ACCESS_KEY_ID", "minio")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "minio123")
	t.Setenv("AWS_REGION", "us-east-1")

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("minio", "minio123", "")))
	if err != nil {
		t.Fatal(err)
	}
	cli := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &endpoint
		o.UsePathStyle = true
	})

	// Wait for MinIO to accept requests, then create bucket + upload dump.
	var lastErr error
	for i := 0; i < 60; i++ {
		_, lastErr = cli.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("backups")})
		if lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		t.Fatalf("minio never became ready: %v", lastErr)
	}
	dump := "create table ledger (id bigserial primary key);\n" +
		"insert into ledger select from generate_series(1, 1000);\n"
	if _, err := cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("backups"), Key: aws.String("pg/demo.sql"),
		Body: strings.NewReader(dump),
	}); err != nil {
		t.Fatalf("uploading dump: %v", err)
	}

	doc := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-s3 }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    from: { type: s3, uri: "s3://backups/pg/demo.sql", endpoint: "%s" }
  sandbox: { provider: docker, image: "postgres:16.10-alpine", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 1h }
    - rowCount: { query: "select count(*) from ledger", min: 1000 }
  report: { sign: false }
`, endpoint)

	d, err := spec.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	dir := t.TempDir()
	e, _, err := drill.Run(ctx, d, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("drill.Run: %v", err)
	}
	if !e.Verified {
		t.Fatalf("s3 drill not verified: %+v", e)
	}
	if e.Backup.AgeSecs > 3600 {
		t.Errorf("backup age from S3 LastModified looks wrong: %v", e.Backup.AgeSecs)
	}

	// Discovery over a prefix: what a nightly pipeline actually leaves behind
	// is timestamped, gzipped keys — the newest matching one must be drilled.
	bigger := "create table ledger (id bigserial primary key);\n" +
		"insert into ledger select from generate_series(1, 3000);\n"
	// Ordered oldest-first: selection is by LastModified, so the upload
	// sequence is the fixture and must not depend on map iteration order.
	for _, up := range []struct{ key, body string }{
		{"pg/nightly/payments-2026-08-21.sql.gz", dump},
		{"pg/nightly/payments-2026-08-22.sql.gz", bigger},
	} {
		key, body := up.key, up.body
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := cli.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("backups"), Key: aws.String(key), Body: bytes.NewReader(buf.Bytes()),
		}); err != nil {
			t.Fatalf("uploading %s: %v", key, err)
		}
		// LastModified drives selection; space the uploads so the ordering
		// is unambiguous.
		time.Sleep(1100 * time.Millisecond)
	}

	doc2 := fmt.Sprintf(`
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: e2e-s3-latest }
spec:
  objectives: { rto: 10m, rpo: 24h }
  source:
    driver: postgres
    from:
      type: s3
      uri: "s3://backups/pg/nightly/"
      endpoint: "%s"
      select: latest
      match: "payments-*.sql.gz"
  sandbox: { provider: docker, image: "postgres:16.10-alpine", ttl: 10m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "select count(*) from ledger", min: 3000 }
  report: { sign: false }
`, endpoint)

	d2, err := spec.Parse(strings.NewReader(doc2))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	e2, _, err := drill.Run(ctx, d2, drill.Options{EvidenceDir: filepath.Join(dir, "evidence"), Version: "e2e-test"})
	if err != nil {
		t.Fatalf("drill.Run (prefix): %v", err)
	}
	if !e2.Verified {
		t.Fatalf("prefix drill not verified: %+v", e2)
	}
	if e2.Backup.ResolvedURI != "s3://backups/pg/nightly/payments-2026-08-22.sql.gz" {
		t.Errorf("selected the wrong object: %q", e2.Backup.ResolvedURI)
	}
	if e2.Backup.Compression != "gzip" {
		t.Errorf("gzip artifact not recorded as compressed: %+v", e2.Backup)
	}
	_ = os.Unsetenv("AWS_REGION")
}

package spec

import (
	"strings"
	"testing"
	"time"
)

const valid = `
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata:
  name: payments-db
spec:
  objectives:
    rto: 15m
    rpo: 1h
  source:
    driver: postgres
    from: { type: file, uri: ./demo.dump }
  sandbox:
    provider: docker
    image: postgres:16
    ttl: 30m
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 1h }
    - rowCount:  { query: "select count(*) from ledger", min: 1000000 }
    - checksum:  { table: ledger, column: id }
    - smoke:     { sql: "select 1", expectRows: ">=1" }
  report:
    sign: true
    controls: [ISO27001-A.8.13]
`

func TestParseValid(t *testing.T) {
	d, err := Parse(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Metadata.Name != "payments-db" {
		t.Errorf("name = %q", d.Metadata.Name)
	}
	if d.Spec.Objectives.RTO.Duration != 15*time.Minute {
		t.Errorf("rto = %v", d.Spec.Objectives.RTO)
	}
	if len(d.Spec.Verify) != 5 {
		t.Errorf("verify checks = %d, want 5", len(d.Spec.Verify))
	}
}

const validVelero = `
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata:
  name: shop-ns
spec:
  objectives:
    rto: 15m
    rpo: 24h
  source:
    driver: velero
    from: { type: velero, backup: shop-nightly, namespace: shop }
  sandbox:
    provider: kubernetes
    ttl: 30m
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - podsReady: { timeout: 5m }
    - resourceCount: { kind: deployments, min: 1 }
  report:
    sign: false
`

func TestParseVelero(t *testing.T) {
	d, err := Parse(strings.NewReader(validVelero))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Spec.Source.From.Backup != "shop-nightly" {
		t.Errorf("backup = %q", d.Spec.Source.From.Backup)
	}

	bad := map[string]string{
		"sql check on velero":  strings.Replace(validVelero, "- podsReady: { timeout: 5m }", `- rowCount: { query: "select 1", min: 1 }`, 1),
		"docker provider":      strings.Replace(validVelero, "provider: kubernetes", "provider: docker", 1),
		"missing backup":       strings.Replace(validVelero, "backup: shop-nightly, ", "", 1),
		"missing namespace":    strings.Replace(validVelero, ", namespace: shop", "", 1),
		"bad resourceCount":    strings.Replace(validVelero, "kind: deployments", "kind: crontabs", 1),
		"file type for velero": strings.Replace(validVelero, "type: velero, backup: shop-nightly, namespace: shop", "type: file, uri: /x", 1),
	}
	for name, doc := range bad {
		if _, err := Parse(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":  strings.Replace(valid, "image:", "imagee:", 1),
		"bad kind":       strings.Replace(valid, "RecoveryDrill", "Backup", 1),
		"bad driver":     strings.Replace(valid, "driver: postgres", "driver: oracle", 1),
		"no ttl":         strings.Replace(valid, "ttl: 30m", "ttl: 0s", 1),
		"bad duration":   strings.Replace(valid, "rto: 15m", "rto: soon", 1),
		"two check keys": strings.Replace(valid, "- freshness:", "  freshness2: 1\n    - freshness:", 1),
		"empty smoke":    strings.Replace(valid, `sql: "select 1"`, `sql: ""`, 1),
		"unsafe name":    strings.Replace(valid, "name: payments-db", "name: ../../etc/passwd", 1),
		"uppercase name": strings.Replace(valid, "name: payments-db", "name: PaymentsDB", 1),
		"k8s check on engine drill": strings.Replace(valid, "- freshness: { maxAge: 1h }",
			"- podsReady: { timeout: 5m }", 1),
	}
	for name, doc := range cases {
		if _, err := Parse(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

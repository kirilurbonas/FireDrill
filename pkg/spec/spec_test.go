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

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":  strings.Replace(valid, "image:", "imagee:", 1),
		"bad kind":       strings.Replace(valid, "RecoveryDrill", "Backup", 1),
		"bad driver":     strings.Replace(valid, "driver: postgres", "driver: mysql", 1),
		"no ttl":         strings.Replace(valid, "ttl: 30m", "ttl: 0s", 1),
		"bad duration":   strings.Replace(valid, "rto: 15m", "rto: soon", 1),
		"two check keys": strings.Replace(valid, "- freshness:", "  freshness2: 1\n    - freshness:", 1),
		"empty smoke":    strings.Replace(valid, `sql: "select 1"`, `sql: ""`, 1),
	}
	for name, doc := range cases {
		if _, err := Parse(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

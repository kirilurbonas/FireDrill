package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

func TestWriteHTML(t *testing.T) {
	dir := t.TempDir()
	e := &Evidence{
		Drill:      "payments-db",
		Tool:       "firedrill test",
		StartedAt:  time.Unix(1770000000, 0),
		FinishedAt: time.Unix(1770000300, 0),
		Verified:   true,
		Controls:   []string{"ISO27001-A.8.13"},
		Checks: []verify.Result{
			{Name: "restoreSucceeded", Passed: true, Detail: "restore completed"},
			{Name: "rowCount", Passed: false, Detail: "0 rows (min 1)"},
			{Name: "smoke", Skipped: true, Detail: "skipped: restore failed"},
		},
	}
	e.Objectives.RTO = "15m0s"
	e.Objectives.RPO = "1h0m0s"
	e.Measured.RestoreSeconds = 230
	e.Measured.RTOMet = true
	e.Backup.URI = "s3://acme/payments.dump"
	e.Sandbox.Provider = "docker"
	e.Sandbox.Image = "postgres:16.6"
	e.Sandbox.Destroyed = true

	jsonPath := filepath.Join(dir, "payments-db-x.json")
	htmlPath, err := WriteHTML(e, jsonPath)
	if err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	if htmlPath != filepath.Join(dir, "payments-db-x.html") {
		t.Errorf("path = %s", htmlPath)
	}
	data, err := os.ReadFile(htmlPath) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{
		"RECOVERY VERIFIED", "payments-db", "3m50s", "restoreSucceeded",
		"PASS", "FAIL", "SKIP", "ISO27001-A.8.13", "s3://acme/payments.dump",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html missing %q", want)
		}
	}
}

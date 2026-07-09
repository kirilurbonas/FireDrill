package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

func sampleEvidence() *report.Evidence {
	e := &report.Evidence{
		Drill:      "payments-db",
		FinishedAt: time.Unix(1770000000, 0),
		Verified:   true,
		Checks: []verify.Result{
			{Name: "restoreSucceeded", Passed: true},
			{Name: "rowCount", Passed: false},
		},
	}
	e.Measured.RestoreSeconds = 230
	e.Measured.RTOMet = true
	e.Measured.RPOMet = true
	e.Backup.AgeSecs = 2460
	return e
}

func TestTextfileSink(t *testing.T) {
	dir := t.TempDir()
	errs := Export(sampleEvidence(), []spec.Sink{{Type: "prometheus", TextfileDir: dir}})
	if len(errs) != 0 {
		t.Fatalf("export errors: %v", errs)
	}
	data, err := os.ReadFile(filepath.Join(dir, "firedrill-payments-db.prom")) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{
		`firedrill_drill_verified{drill="payments-db"} 1`,
		`firedrill_restore_duration_seconds{drill="payments-db"} 230`,
		`firedrill_backup_age_seconds{drill="payments-db"} 2460`,
		`firedrill_rto_met{drill="payments-db"} 1`,
		`firedrill_check_passed{check="restoreSucceeded",drill="payments-db"} 1`,
		`firedrill_check_passed{check="rowCount",drill="payments-db"} 0`,
		`firedrill_drill_timestamp_seconds{drill="payments-db"} 1.77e+09`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("textfile missing %q\n---\n%s", want, out)
		}
	}
}

func TestPushgatewaySink(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	errs := Export(sampleEvidence(), []spec.Sink{{Type: "pushgateway", URL: srv.URL}})
	if len(errs) != 0 {
		t.Fatalf("export errors: %v", errs)
	}
	if gotPath != "/metrics/job/firedrill/drill/payments-db" {
		t.Errorf("push path = %q", gotPath)
	}
	if !strings.Contains(string(gotBody), "firedrill_drill_verified") {
		t.Error("push body missing metrics")
	}
}

func TestSinkFailureIsReturned(t *testing.T) {
	errs := Export(sampleEvidence(), []spec.Sink{{Type: "pushgateway", URL: "http://127.0.0.1:1"}})
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
}

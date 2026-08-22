package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

func TestEvalRows(t *testing.T) {
	cases := []struct {
		n    int
		expr string
		want bool
	}{
		{1, ">=1", true}, {0, ">=1", false}, {5, "==5", true},
		{5, "<10", true}, {5, "<=4", false}, {2, "> 1", true},
	}
	for _, c := range cases {
		got, err := evalRows(c.n, c.expr)
		if err != nil || got != c.want {
			t.Errorf("evalRows(%d, %q) = %v, %v; want %v", c.n, c.expr, got, err, c.want)
		}
	}
	if _, err := evalRows(1, "about 5"); err == nil {
		t.Error("expected error for invalid expression")
	}
}

func TestFreshnessAndRestore(t *testing.T) {
	checks := []spec.Check{
		{RestoreSucceeded: &struct{}{}},
		{Freshness: &spec.FreshnessCheck{MaxAge: spec.Duration{Duration: time.Hour}}},
	}
	res := Run(context.Background(), nil, checks, Context{BackupAge: 41 * time.Minute})
	for _, r := range res {
		if !r.Passed {
			t.Errorf("%s failed: %s", r.Name, r.Detail)
		}
	}

	res = Run(context.Background(), nil, checks, Context{
		RestoreErr: errors.New("boom"),
		BackupAge:  2 * time.Hour,
	})
	if res[0].Passed {
		t.Error("restoreSucceeded should fail")
	}
	if res[1].Passed {
		t.Error("freshness should fail at 2h > 1h")
	}
}

func TestDataChecksSkippedOnRestoreFailure(t *testing.T) {
	checks := []spec.Check{
		{RowCount: &spec.RowCountCheck{Query: "select 1", Min: 1}},
		{Smoke: &spec.SmokeCheck{SQL: "select 1"}},
		{Canary: &spec.CanaryCheck{SQL: "select token from c", Expect: "x"}},
	}
	res := Run(context.Background(), nil, checks, Context{RestoreErr: errors.New("boom")})
	for _, r := range res {
		if !r.Skipped {
			t.Errorf("%s: expected skipped, got %+v", r.Name, r)
		}
	}
}

func TestChecksumIdentifierValidation(t *testing.T) {
	// A malicious identifier must be rejected before it reaches the engine.
	r := checksum(context.Background(), &fakeEngine{}, &spec.ChecksumCheck{Table: "ledger; drop table x", Column: "id"})
	if r.Passed || r.Detail != "invalid table/column identifier" {
		t.Errorf("expected identifier rejection, got %+v", r)
	}
	// A SQL engine with no dialect wired reports it rather than passing.
	r = checksum(context.Background(), &sqlEngine{}, &spec.ChecksumCheck{Table: "ledger", Column: "id"})
	if r.Passed || r.Detail != "no checksum dialect configured" {
		t.Errorf("expected nil-dialect rejection, got %+v", r)
	}
}

// fakeEngine is a scripted Engine for check-logic tests (no real database).
type fakeEngine struct {
	count  int64
	rows   int
	scalar string
	sum    string
	err    error
}

func (f *fakeEngine) Count(context.Context, string) (int64, error)   { return f.count, f.err }
func (f *fakeEngine) Rows(context.Context, string) (int, error)      { return f.rows, f.err }
func (f *fakeEngine) Scalar(context.Context, string) (string, error) { return f.scalar, f.err }
func (f *fakeEngine) Checksum(context.Context, string, string) (string, error) {
	return f.sum, f.err
}

func TestDataChecksAgainstEngine(t *testing.T) {
	eng := &fakeEngine{count: 120, rows: 1, scalar: "fd-canary", sum: "abc123"}
	checks := []spec.Check{
		{RowCount: &spec.RowCountCheck{Query: "select count(*) from ledger", Min: 100}},
		{Smoke: &spec.SmokeCheck{SQL: "select 1"}},
		{Canary: &spec.CanaryCheck{Query: "db.c.findOne().token", Expect: "fd-canary"}},
		{Checksum: &spec.ChecksumCheck{Table: "ledger", Column: "id", Expect: "abc123"}},
	}
	for _, r := range Run(context.Background(), eng, checks, Context{}) {
		if !r.Passed {
			t.Errorf("%s: expected pass, got %+v", r.Name, r)
		}
	}

	// Below the minimum, wrong sentinel and a checksum drift must all fail.
	eng = &fakeEngine{count: 3, rows: 0, scalar: "tampered", sum: "deadbeef"}
	for _, r := range Run(context.Background(), eng, checks, Context{}) {
		if r.Passed {
			t.Errorf("%s: expected failure, got %+v", r.Name, r)
		}
	}
	if got := Run(context.Background(), eng, checks[2:3], Context{})[0].Detail; strings.Contains(got, "fd-canary") {
		t.Errorf("canary detail leaked the sentinel value: %q", got)
	}
}

func TestDataChecksWithoutEngine(t *testing.T) {
	checks := []spec.Check{{RowCount: &spec.RowCountCheck{Query: "select 1", Min: 1}}}
	r := Run(context.Background(), nil, checks, Context{})[0]
	if r.Passed || r.Skipped {
		t.Errorf("a missing engine must fail the check, got %+v", r)
	}
}

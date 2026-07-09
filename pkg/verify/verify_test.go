package verify

import (
	"context"
	"errors"
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
	}
	res := Run(context.Background(), nil, checks, Context{RestoreErr: errors.New("boom")})
	for _, r := range res {
		if !r.Skipped {
			t.Errorf("%s: expected skipped, got %+v", r.Name, r)
		}
	}
}

func TestChecksumIdentifierValidation(t *testing.T) {
	r := checksum(context.Background(), nil, &spec.ChecksumCheck{Table: "ledger; drop table x", Column: "id"})
	if r.Passed || r.Detail != "invalid table/column identifier" {
		t.Errorf("expected identifier rejection, got %+v", r)
	}
}

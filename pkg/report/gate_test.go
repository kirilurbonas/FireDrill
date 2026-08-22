package report

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gateFixture writes a small evidence directory: payments-db verified
// yesterday, orders-db verified a month ago and failing since.
func gateFixture(t *testing.T, now time.Time) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "evidence")
	write := func(drill string, at time.Time, verified bool, controls ...string) string {
		t.Helper()
		e := &Evidence{Drill: drill, FinishedAt: at, Verified: verified, Controls: controls}
		p, err := e.Write(dir)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("payments-db", now.Add(-20*time.Hour), true, "ISO27001-A.8.13")
	write("orders-db", now.Add(-30*24*time.Hour), true, "ISO27001-A.8.13")
	write("orders-db", now.Add(-2*24*time.Hour), false, "ISO27001-A.8.13")
	return dir
}

func statusOf(t *testing.T, r *GateReport, subject string) SubjectStatus {
	t.Helper()
	for _, s := range r.Subjects {
		if s.Subject == subject {
			return s
		}
	}
	t.Fatalf("subject %q missing from report", subject)
	return SubjectStatus{}
}

func TestGateStaleAndFailing(t *testing.T) {
	now := time.Unix(1770000000, 0).UTC()
	dir := gateFixture(t, now)

	r, err := Gate(GateOptions{Dir: dir, MaxAge: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r.Violations != 1 {
		t.Fatalf("violations = %d, want 1 (orders-db)\n%+v", r.Violations, r.Subjects)
	}
	if p := statusOf(t, r, "payments-db"); !p.OK() {
		t.Errorf("payments-db should pass: %v", p.Violations)
	}
	o := statusOf(t, r, "orders-db")
	if o.OK() || len(o.Violations) != 2 {
		t.Fatalf("orders-db should fail on both counts, got %v", o.Violations)
	}
	if !strings.Contains(o.Violations[0], "did not verify") ||
		!strings.Contains(o.Violations[1], "was 30d ago (max 24h)") {
		t.Errorf("unexpected violations: %v", o.Violations)
	}

	// --allow-unverified keeps the staleness check but tolerates the failure.
	r, err = Gate(GateOptions{Dir: dir, MaxAge: 60 * 24 * time.Hour, Now: now, AllowUnverified: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Violations != 0 {
		t.Errorf("expected no violations with a 60d window, got %+v", r.Subjects)
	}
}

func TestGateMissingSubject(t *testing.T) {
	now := time.Unix(1770000000, 0).UTC()
	dir := gateFixture(t, now)

	// The failure mode `controls` cannot see: a drill that stopped running
	// leaves no evidence at all, so it must be named to be checked.
	r, err := Gate(GateOptions{
		Dir: dir, MaxAge: 24 * time.Hour, Now: now,
		Subjects: []string{"payments-db", "ledger-db"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Violations != 1 {
		t.Fatalf("violations = %d, want 1", r.Violations)
	}
	got := statusOf(t, r, "ledger-db")
	if got.Runs != 0 || !strings.Contains(got.Violations[0], "no evidence") {
		t.Errorf("ledger-db: got %+v", got)
	}
	// orders-db was not asked for, so it is not reported on.
	if len(r.Subjects) != 2 {
		t.Errorf("subjects = %d, want only the two requested", len(r.Subjects))
	}
}

func TestGateByControlAndSigning(t *testing.T) {
	now := time.Unix(1770000000, 0).UTC()
	dir := gateFixture(t, now)

	r, err := Gate(GateOptions{Dir: dir, By: "control", MaxAge: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	// A control aggregates every drill that evidences it, so payments-db's
	// recent pass satisfies it even though orders-db is failing — gating by
	// drill (the default) is the stricter check.
	c := statusOf(t, r, "ISO27001-A.8.13")
	if c.Runs != 3 || !c.OK() {
		t.Fatalf("control status = %+v", c)
	}
	if len(r.Subjects) != 1 {
		t.Errorf("expected one control subject, got %+v", r.Subjects)
	}

	// Nothing here is signed, so --require-signed must fail every subject.
	r, err = Gate(GateOptions{Dir: dir, Now: now, AllowUnverified: true, RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Violations != len(r.Subjects) {
		t.Errorf("expected every subject to fail the signature requirement, got %+v", r.Subjects)
	}

	if _, err := Gate(GateOptions{Dir: dir, By: "team"}); err == nil {
		t.Error("expected an error for an unsupported --by")
	}
}

func TestGateOutput(t *testing.T) {
	now := time.Unix(1770000000, 0).UTC()
	dir := gateFixture(t, now)
	r, err := Gate(GateOptions{Dir: dir, MaxAge: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	r.WriteText(&b)
	for _, want := range []string{"DRILL", "payments-db", "ok", "orders-db", "FAIL:", "2 drill(s): 1 ok, 1 failing"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("gate output missing %q\n%s", want, b.String())
		}
	}

	var j strings.Builder
	if err := r.WriteJSON(&j); err != nil {
		t.Fatal(err)
	}
	var round GateReport
	if err := json.Unmarshal([]byte(j.String()), &round); err != nil {
		t.Fatalf("gate json does not round-trip: %v", err)
	}
	if round.Violations != 1 || len(round.Subjects) != 2 {
		t.Errorf("round-tripped report = %+v", round)
	}

	var empty strings.Builder
	er, err := Gate(GateOptions{Dir: filepath.Join(t.TempDir(), "nothing"), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	er.WriteText(&empty)
	if !strings.Contains(empty.String(), "no drills to check") {
		t.Errorf("empty gate message = %q", empty.String())
	}
}

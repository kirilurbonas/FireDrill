package report

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evidence")
	write := func(drill string, at time.Time, verified bool, rto float64) {
		e := &Evidence{Drill: drill, FinishedAt: at, Verified: verified}
		e.Measured.RestoreSeconds = rto
		e.Measured.RTOMet = verified
		e.Measured.RPOMet = true
		if _, err := e.Write(dir); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Unix(1770000000, 0).UTC()
	write("payments-db", base, true, 30)
	write("payments-db", base.Add(24*time.Hour), true, 45)
	write("payments-db", base.Add(48*time.Hour), false, 90)
	write("orders-db", base.Add(2*time.Hour), true, 10)

	all, err := LoadHistory(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("entries = %d, want 4", len(all))
	}
	if !all[0].FinishedAt.Before(all[3].FinishedAt) {
		t.Error("history not sorted oldest first")
	}

	filtered, err := LoadHistory(dir, "payments-db")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 3 {
		t.Fatalf("filtered entries = %d, want 3", len(filtered))
	}

	var b strings.Builder
	WriteHistory(&b, filtered)
	out := b.String()
	for _, want := range []string{"payments-db", "FAILED", "ok", "1m30s", "3 run(s), 2 verified (67%)", "▇"} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q\n%s", want, out)
		}
	}

	var empty strings.Builder
	WriteHistory(&empty, nil)
	if !strings.Contains(empty.String(), "no drill evidence") {
		t.Error("empty history message missing")
	}
}

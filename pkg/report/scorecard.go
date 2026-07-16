package report

import (
	"fmt"
	"io"
	"time"
)

// Outcome is one drill's result within a fleet run.
type Outcome struct {
	Drill        string
	Evidence     *Evidence // nil if the drill could not execute
	EvidencePath string
	Err          error // execution error (infra), not a failed verdict
}

// WriteScorecard renders the fleet summary after `firedrill run --all`.
func WriteScorecard(w io.Writer, outcomes []Outcome) (verified, failed, errored int) {
	fmt.Fprintf(w, "\n%-20s  %-10s  %-10s  %-4s %-4s  %s\n", "DRILL", "RESULT", "RESTORE", "RTO", "RPO", "EVIDENCE")
	for _, o := range outcomes {
		switch {
		case o.Err != nil:
			errored++
			fmt.Fprintf(w, "%-20s  %-10s  %-10s  %-4s %-4s  %s\n", o.Drill, "ERROR", "-", "-", "-", truncErr(o.Err, 60))
		case o.Evidence.Verified:
			verified++
			fmt.Fprintf(w, "%-20s  %-10s  %-10s  %-4s %-4s  %s\n", o.Drill, "verified",
				rtoStr(o.Evidence), mark(o.Evidence.Measured.RTOMet), mark(o.Evidence.Measured.RPOMet), o.EvidencePath)
		default:
			failed++
			fmt.Fprintf(w, "%-20s  %-10s  %-10s  %-4s %-4s  %s\n", o.Drill, "FAILED",
				rtoStr(o.Evidence), mark(o.Evidence.Measured.RTOMet), mark(o.Evidence.Measured.RPOMet), o.EvidencePath)
		}
	}
	fmt.Fprintf(w, "\n%d drill(s): %d verified, %d failed, %d errored\n",
		len(outcomes), verified, failed, errored)
	return verified, failed, errored
}

func rtoStr(e *Evidence) string {
	return (time.Duration(e.Measured.RestoreSeconds * float64(time.Second))).Round(time.Second).String()
}

func truncErr(err error, n int) string {
	s := err.Error()
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

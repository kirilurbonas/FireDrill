package postgres

import (
	"strings"
	"testing"
)

// TestFatalStartupLine pins the difference between "still recovering" and
// "broken". Postgres logs FATAL while it replays WAL — provoked by the
// readiness poll itself — and treating that as a failure reports a good
// backup as unrecoverable.
func TestFatalStartupLine(t *testing.T) {
	recovering := `2026-08-22 21:01:41.001 UTC [1] LOG:  starting PostgreSQL 16.10
2026-08-22 21:01:41.900 UTC [1] LOG:  database system was interrupted; last known up at 2026-08-22 20:59:12 UTC
2026-08-22 21:01:42.960 UTC [71] FATAL:  the database system is not yet accepting connections
2026-08-22 21:01:42.960 UTC [71] DETAIL:  Consistent recovery state has not been yet reached.
2026-08-22 21:01:43.100 UTC [72] FATAL:  the database system is starting up`
	if got := fatalStartupLine(recovering); got != "" {
		t.Errorf("crash recovery must not be reported as failure, got %q", got)
	}

	broken := recovering + `
2026-08-22 21:01:44.010 UTC [1] PANIC:  could not locate a valid checkpoint record
2026-08-22 21:01:44.020 UTC [1] LOG:  startup process was terminated`
	got := fatalStartupLine(broken)
	if got == "" {
		t.Fatal("a corrupt data directory must be caught")
	}
	if want := "PANIC:  could not locate a valid checkpoint record"; !strings.Contains(got, want) {
		t.Errorf("fatalStartupLine = %q, want the line containing %q", got, want)
	}

	// A real FATAL that is not a startup phase must surface even when
	// transient lines came first.
	mixed := "FATAL:  the database system is starting up\nFATAL:  data directory has wrong ownership"
	if got := fatalStartupLine(mixed); !strings.Contains(got, "wrong ownership") {
		t.Errorf("fatalStartupLine = %q, want the ownership error", got)
	}

	if got := fatalStartupLine(""); got != "" {
		t.Errorf("empty log = %q, want empty", got)
	}
	if got := fatalStartupLine("LOG:  database system is ready to accept connections"); got != "" {
		t.Errorf("healthy log = %q, want empty", got)
	}
}

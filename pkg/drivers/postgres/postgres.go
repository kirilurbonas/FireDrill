// Package postgres restores Postgres backups into a sandbox. It runs the
// restore tooling inside the sandbox container itself (docker exec), so the
// host needs no Postgres client installed and versions always match the
// sandbox image.
package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	sbdocker "github.com/kirilurbonas/FireDrill/pkg/sandbox/docker"
)

// Result describes a completed restore attempt.
type Result struct {
	Duration time.Duration // measured restore time — the drill's RTO
	Output   string        // tail of tool output for diagnostics
	Format   string        // "custom" or "plain"
}

// Restore loads the dump at path into the sandbox database and times it.
// Custom-format dumps (pg_dump -Fc) go through pg_restore; plain SQL through psql.
func Restore(ctx context.Context, sb *sbdocker.Sandbox, path string) (*Result, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from the drill spec / fetched backup
	if err != nil {
		return nil, fmt.Errorf("opening backup: %w", err)
	}
	defer func() { _ = f.Close() }()

	format, err := detectFormat(f)
	if err != nil {
		return nil, err
	}

	var cmd []string
	switch format {
	case "custom":
		cmd = []string{"pg_restore", "-U", sb.User(), "-d", sb.DB(), "--no-owner", "--no-privileges", "--exit-on-error"}
	case "plain":
		cmd = []string{"psql", "-U", sb.User(), "-d", sb.DB(), "-v", "ON_ERROR_STOP=1", "-f", "-"}
	}

	start := time.Now()
	code, out, err := sb.Exec(ctx, cmd, sbdocker.WithStdin(f))
	elapsed := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("restore exec: %w", err)
	}
	res := &Result{Duration: elapsed, Output: tail(out, 4000), Format: format}
	if code != 0 {
		return res, fmt.Errorf("restore failed (exit %d): %s", code, res.Output)
	}
	return res, nil
}

// detectFormat sniffs the pg_dump custom-format magic ("PGDMP") and rewinds.
func detectFormat(f io.ReadSeeker) (string, error) {
	magic := make([]byte, 5)
	n, err := f.Read(magic)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading backup header: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if n >= 5 && bytes.Equal(magic, []byte("PGDMP")) {
		return "custom", nil
	}
	return "plain", nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

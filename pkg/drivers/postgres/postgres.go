// Package postgres implements the Postgres driver. Restore tooling runs
// inside the sandbox container itself (docker exec), so the host needs no
// Postgres client installed and versions always match the sandbox image.
package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/drivers"
)

func init() { drivers.Register(Driver{}) }

// Driver adapts Postgres to the drill loop.
type Driver struct{}

func (Driver) Name() string { return "postgres" }
func (Driver) Port() string { return "5432/tcp" }

func (Driver) ContainerEnv(user, password, db string) []string {
	return []string{
		"POSTGRES_DB=" + db,
		"POSTGRES_USER=" + user,
		"POSTGRES_PASSWORD=" + password,
	}
}

// ReadyCmds: pg_isready can pass during the entrypoint's init-phase restart,
// so a real query confirms the server that will stay up is accepting work.
func (Driver) ReadyCmds(user, _, db string) [][]string {
	return [][]string{
		{"pg_isready", "-U", user, "-d", db},
		{"psql", "-U", user, "-d", db, "-c", "select 1"},
	}
}

func (Driver) SQLDriver() string { return "pgx" }

func (Driver) DSN(sb drivers.Sandbox) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
		sb.User(), sb.Password(), sb.HostPort(), sb.DB())
}

// ChecksumQuery is an order-independent md5 over one column.
func (Driver) ChecksumQuery(table, column string) string {
	return fmt.Sprintf(
		`select coalesce(md5(string_agg(%s::text, ',' order by %s)), 'empty') from %s`,
		column, column, table)
}

// Restore loads the dump at path into the sandbox database and times it.
// Custom-format dumps (pg_dump -Fc) go through pg_restore; plain SQL through psql.
func (Driver) Restore(ctx context.Context, sb drivers.Sandbox, path string) (*drivers.RestoreResult, error) {
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
	code, out, err := sb.Exec(ctx, cmd, f)
	elapsed := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("restore exec: %w", err)
	}
	res := &drivers.RestoreResult{Duration: elapsed, Output: tail(out, 4000), Format: format}
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

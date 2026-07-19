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
	"github.com/kirilurbonas/FireDrill/pkg/spec"
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
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		sb.User(), sb.Password(), sb.Host(), sb.HostPort(), sb.DB())
}

// ChecksumQuery is an order-independent md5 over one column.
func (Driver) ChecksumQuery(table, column string) string {
	return fmt.Sprintf(
		`select coalesce(md5(string_agg(%s::text, ',' order by %s)), 'empty') from %s`,
		column, column, table)
}

// Restore loads the backup at path into the sandbox and times it.
// Logical dumps (pg_dump) go through pg_restore/psql; basebackup tars are
// physically restored into PGDATA before first start.
func (d Driver) Restore(ctx context.Context, sb drivers.Sandbox, path string, src spec.Source) (*drivers.RestoreResult, error) {
	if src.Format == "basebackup" {
		return d.restoreBasebackup(ctx, sb, path, src)
	}
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

const pgdata = "/var/lib/postgresql/data"

// restoreBasebackup physically restores a pg_basebackup tar (-Ft -X fetch)
// into an empty PGDATA and starts Postgres over it — crash recovery replays
// the WAL shipped inside the backup. The sandbox was provisioned cold.
//
// A physical restore brings back the SOURCE cluster's users and pg_hba, whose
// passwords we don't know; the sandbox's pg_hba is therefore replaced with
// trust auth. That is acceptable only because the sandbox is throwaway and
// network-isolated (loopback-only / deny-egress) — never do this to a real
// server.
func (Driver) restoreBasebackup(ctx context.Context, sb drivers.Sandbox, path string, src spec.Source) (*drivers.RestoreResult, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from the drill spec / fetched backup
	if err != nil {
		return nil, fmt.Errorf("opening backup: %w", err)
	}
	defer func() { _ = f.Close() }()

	start := time.Now()
	fail := func(step string, code int, out string, err error) (*drivers.RestoreResult, error) {
		res := &drivers.RestoreResult{Duration: time.Since(start), Format: "basebackup", Output: tail(out, 4000)}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", step, err)
		}
		return res, fmt.Errorf("%s failed (exit %d): %s", step, code, res.Output)
	}

	// 1. Untar the basebackup into PGDATA (streamed, never touches host disk paths).
	code, out, err := sb.Exec(ctx, []string{"sh", "-c",
		"mkdir -p " + pgdata + " && tar -xf - -C " + pgdata}, f)
	if err != nil || code != 0 {
		return fail("untar basebackup", code, out, err)
	}

	// 2. Ownership/permissions + trust auth confined to the isolated sandbox.
	code, out, err = sb.Exec(ctx, []string{"sh", "-c",
		"chown -R postgres:postgres " + pgdata +
			" && chmod 700 " + pgdata +
			` && printf 'local all all trust\nhost all all all trust\n' > ` + pgdata + "/pg_hba.conf"}, nil)
	if err != nil || code != 0 {
		return fail("preparing data directory", code, out, err)
	}

	// 3. Start Postgres in the background over the restored data directory.
	code, out, err = sb.Exec(ctx, []string{"sh", "-c",
		"nohup docker-entrypoint.sh postgres >/tmp/firedrill-pg.log 2>&1 & echo started"}, nil)
	if err != nil || code != 0 {
		return fail("starting postgres", code, out, err)
	}

	// 4. Wait until the restored cluster accepts queries; recovery time is
	// part of the measured RTO.
	db := src.Database
	if db == "" {
		db = "postgres"
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if time.Now().After(deadline) {
			_, logOut, _ := sb.Exec(ctx, []string{"sh", "-c", "tail -c 2000 /tmp/firedrill-pg.log"}, nil)
			return fail("waiting for restored cluster", -1, logOut,
				fmt.Errorf("not ready after 3m"))
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		code, _, err = sb.Exec(ctx, []string{"psql", "-U", "postgres", "-d", db, "-c", "select 1"}, nil)
		if err == nil && code == 0 {
			break
		}
		time.Sleep(time.Second)
	}

	return &drivers.RestoreResult{
		Duration: time.Since(start),
		Format:   "basebackup",
		DSN: fmt.Sprintf("postgres://postgres@%s:%s/%s?sslmode=disable",
			sb.Host(), sb.HostPort(), db),
	}, nil
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

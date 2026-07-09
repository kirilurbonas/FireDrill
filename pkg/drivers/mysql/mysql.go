// Package mysql implements the MySQL driver. Restore tooling runs inside
// the sandbox container (docker exec), so the host needs no MySQL client.
package mysql

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/drivers"
)

func init() { drivers.Register(Driver{}) }

// Driver adapts MySQL to the drill loop. v0.3 supports plain SQL dumps
// (mysqldump output); physical backups (XtraBackup/clone) are future work.
type Driver struct{}

func (Driver) Name() string { return "mysql" }
func (Driver) Port() string { return "3306/tcp" }

func (Driver) ContainerEnv(user, password, db string) []string {
	return []string{
		"MYSQL_DATABASE=" + db,
		"MYSQL_USER=" + user,
		"MYSQL_PASSWORD=" + password,
		"MYSQL_RANDOM_ROOT_PASSWORD=yes",
	}
}

// ReadyCmds: mysqladmin ping can succeed while the entrypoint's temporary
// init server is up, so a real query against the drill database confirms
// the final server is serving.
func (Driver) ReadyCmds(user, password, db string) [][]string {
	auth := []string{"-u" + user, "-p" + password}
	return [][]string{
		append([]string{"mysqladmin", "ping", "--silent"}, auth...),
		append([]string{"mysql", db, "-e", "select 1"}, auth...),
	}
}

func (Driver) SQLDriver() string { return "mysql" }

func (Driver) DSN(sb drivers.Sandbox) string {
	return fmt.Sprintf("%s:%s@tcp(127.0.0.1:%s)/%s",
		sb.User(), sb.Password(), sb.HostPort(), sb.DB())
}

// ChecksumQuery is an order-independent checksum: XOR of per-row CRC32s.
// (GROUP_CONCAT + md5 would silently truncate at group_concat_max_len.)
func (Driver) ChecksumQuery(table, column string) string {
	return fmt.Sprintf(
		`select coalesce(conv(bit_xor(crc32(%s)), 10, 16), 'empty') from %s`,
		column, table)
}

// Restore streams a mysqldump SQL file into the sandbox database, timed.
func (Driver) Restore(ctx context.Context, sb drivers.Sandbox, path string) (*drivers.RestoreResult, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from the drill spec / fetched backup
	if err != nil {
		return nil, fmt.Errorf("opening backup: %w", err)
	}
	defer func() { _ = f.Close() }()

	cmd := []string{"mysql", "-u" + sb.User(), "-p" + sb.Password(), sb.DB()}
	start := time.Now()
	code, out, err := sb.Exec(ctx, cmd, f)
	elapsed := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("restore exec: %w", err)
	}
	res := &drivers.RestoreResult{Duration: elapsed, Output: tail(out, 4000), Format: "plain"}
	if code != 0 {
		return res, fmt.Errorf("restore failed (exit %d): %s", code, res.Output)
	}
	return res, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

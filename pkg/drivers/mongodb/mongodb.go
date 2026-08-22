// Package mongodb implements the MongoDB driver. Like the SQL drivers, all
// tooling runs inside the sandbox container (mongorestore, mongosh), so the
// host needs no MongoDB client and tool versions always match the engine.
//
// MongoDB has no database/sql driver, so this driver brings its own
// verification engine (drivers.Verifier) rather than a DSN: checks are
// mongosh expressions evaluated inside the sandbox.
package mongodb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/drivers"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

func init() { drivers.Register(Driver{}) }

// Driver adapts MongoDB to the drill loop. It restores `mongodump --archive`
// artifacts; directory-style dumps are future work.
type Driver struct{}

var (
	_ drivers.Driver   = Driver{}
	_ drivers.Verifier = Driver{}
)

func (Driver) Name() string { return "mongodb" }
func (Driver) Port() string { return "27017/tcp" }

func (Driver) ContainerEnv(user, password, db string) []string {
	return []string{
		"MONGO_INITDB_ROOT_USERNAME=" + user,
		"MONGO_INITDB_ROOT_PASSWORD=" + password,
		"MONGO_INITDB_DATABASE=" + db,
	}
}

// ReadyCmds waits for a server that authenticates, not just one that
// answers: the entrypoint creates the root user after the first start.
//
// The password never appears in argv. mongosh reads it out of the container's
// own environment, which is where the entrypoint already put it.
func (Driver) ReadyCmds(_, _, _ string) [][]string {
	return [][]string{
		{"mongosh", "--quiet", "--eval", authJS + `db.adminCommand({ping: 1});`},
	}
}

// authJS authenticates the mongosh session from the container environment.
// Credentials in argv would be visible in the host's process list.
const authJS = `db.getSiblingDB("admin").auth(process.env.MONGO_INITDB_ROOT_USERNAME, process.env.MONGO_INITDB_ROOT_PASSWORD);`

// toolsConfigPath points at a mongorestore/mongodump config file holding the
// password, so
// it stays out of argv here too.
const toolsConfigPath = "/tmp/.firedrill-mongo.conf"

// Restore streams a mongodump archive into the sandbox, timed.
//
// admin and config are excluded: an archive of a whole cluster carries the
// source's user catalog, and restoring it over the sandbox's own root user
// would lock the drill out of the database it just restored.
func (Driver) Restore(ctx context.Context, sb drivers.Sandbox, path string, src spec.Source) (*drivers.RestoreResult, error) {
	if err := preflight(ctx, sb); err != nil {
		return nil, err
	}
	f, err := os.Open(path) // #nosec G304 -- path comes from the drill spec / fetched backup
	if err != nil {
		return nil, fmt.Errorf("opening backup: %w", err)
	}
	defer func() { _ = f.Close() }()

	if code, out, err := sb.Exec(ctx, writePasswordConfig(), nil); err != nil || code != 0 {
		return nil, fmt.Errorf("preparing restore credentials (exit %d): %s %w", code, tail(out, 500), err)
	}

	cmd := []string{
		"mongorestore",
		"--username", sb.User(),
		"--config", toolsConfigPath,
		"--authenticationDatabase", "admin",
		"--nsExclude", "admin.*",
		"--nsExclude", "config.*",
		"--archive",
	}
	start := time.Now()
	code, out, err := sb.Exec(ctx, cmd, f)
	elapsed := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("restore exec: %w", err)
	}
	res := &drivers.RestoreResult{Duration: elapsed, Output: tail(out, 4000), Format: "archive"}
	if code != 0 {
		return res, fmt.Errorf("restore failed (exit %d): %s", code, res.Output)
	}
	return res, nil
}

// preflight fails with an actionable message when the sandbox image ships no
// database tools. The official mongo images do; slimmed-down ones may not.
func preflight(ctx context.Context, sb drivers.Sandbox) error {
	code, _, err := sb.Exec(ctx, []string{"sh", "-c", "command -v mongorestore >/dev/null"}, nil)
	if err != nil {
		return fmt.Errorf("checking sandbox tooling: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("sandbox image has no mongorestore — use an image that ships the " +
			"MongoDB Database Tools (the official mongo:8 image does)")
	}
	return nil
}

// writePasswordConfig materializes the tools' config file from the
// container's environment; printf is a shell builtin, so the password is
// never an argument to a new process.
func writePasswordConfig() []string {
	return []string{"sh", "-c",
		`umask 077 && printf 'password: %s\n' "$MONGO_INITDB_ROOT_PASSWORD" > ` + toolsConfigPath}
}

// Engine returns the verification engine: mongosh, run inside the sandbox,
// against the restored database.
func (Driver) Engine(sb drivers.Sandbox, src spec.Source) verify.Engine {
	db := src.Database
	if db == "" {
		db = sb.DB()
	}
	return &engine{sb: sb, db: db}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// quoteJS renders a Go string as a JavaScript string literal. Only used for
// identifiers that spec validation has already constrained, but quoting is
// cheap insurance against a stray quote breaking the eval.
func quoteJS(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}

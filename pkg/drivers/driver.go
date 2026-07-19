// Package drivers defines the database-engine abstraction: everything the
// orchestrator and sandbox need to know about a specific engine (container
// environment, readiness, restore tooling, SQL dialect) lives behind Driver.
package drivers

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Sandbox is the subset of a sandbox the driver needs: run commands inside
// the container and know the connection facts. Implemented by sandbox/docker.
type Sandbox interface {
	Exec(ctx context.Context, cmd []string, stdin io.Reader) (int, string, error)
	// Host and HostPort are the address the drill process can reach the
	// sandbox database on (loopback for docker, pod IP or a local
	// port-forward for kubernetes).
	Host() string
	HostPort() string
	User() string
	Password() string
	DB() string
}

// RestoreResult describes a completed restore attempt.
type RestoreResult struct {
	Duration time.Duration // measured restore time — the drill's RTO
	Output   string        // tail of tool output for diagnostics
	Format   string        // dump format, e.g. "custom", "plain", "basebackup"
	// DSN, when non-empty, overrides Driver.DSN for verification — physical
	// restores bring back the source cluster's own users/databases rather
	// than the sandbox's generated credentials.
	DSN string
}

// Driver adapts one database engine to the drill loop.
type Driver interface {
	// Name matches spec.source.driver.
	Name() string
	// ContainerEnv returns the env vars that make the image bootstrap
	// a database with the given credentials.
	ContainerEnv(user, password, db string) []string
	// Port is the container port the engine listens on, e.g. "5432/tcp".
	Port() string
	// ReadyCmds are exec'd in order inside the container; the sandbox polls
	// until every command exits 0 in one pass.
	ReadyCmds(user, password, db string) [][]string
	// Restore loads the backup at path into the sandbox and times it.
	// src carries format/database options for drivers that support
	// multiple artifact kinds.
	Restore(ctx context.Context, sb Sandbox, path string, src spec.Source) (*RestoreResult, error)
	// SQLDriver is the database/sql driver name for verification queries.
	SQLDriver() string
	// DSN builds a connection string for the sandbox, reachable from the host.
	DSN(sb Sandbox) string
	// ChecksumQuery returns an order-independent checksum query over one
	// column, in the engine's dialect. Identifiers are pre-validated.
	ChecksumQuery(table, column string) string
}

var registry = map[string]Driver{}

// Register adds a driver; called from driver package init()s.
func Register(d Driver) { registry[d.Name()] = d }

// Get resolves a driver by spec name.
func Get(name string) (Driver, error) {
	d, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown driver %q", name)
	}
	return d, nil
}

// Names lists registered driver names (for validation and errors).
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}

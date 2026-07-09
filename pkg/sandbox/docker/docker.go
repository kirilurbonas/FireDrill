// Package docker provisions ephemeral, isolated Postgres sandboxes as
// Docker containers. Sandboxes bind to loopback only, live on their own
// bridge network, use random one-off credentials, and are force-destroyed
// when the TTL expires — even if the drill hangs.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const (
	dbName = "firedrill"
	dbUser = "firedrill"
)

// Config describes the sandbox to provision.
type Config struct {
	Image string        // e.g. postgres:16
	TTL   time.Duration // hard teardown deadline
	Name  string        // drill name, used for labels/container name
}

// Sandbox is a running, isolated Postgres container.
type Sandbox struct {
	ContainerID string
	networkID   string
	cli         *client.Client
	password    string
	hostPort    string

	destroyOnce sync.Once
	destroyErr  error
	ttlCancel   context.CancelFunc
	Destroyed   bool
}

// Provision pulls the image, creates an isolated network + container, waits
// until Postgres accepts connections, and arms the TTL watchdog.
func Provision(ctx context.Context, cfg Config) (sb *Sandbox, err error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connecting to docker: %w", err)
	}

	suffix := randomHex(4)
	name := fmt.Sprintf("firedrill-%s-%s", sanitize(cfg.Name), suffix)
	password := randomHex(16)

	// Dedicated internal-capable bridge network: sandbox cannot be reached
	// from (or confused with) anything else on the default bridge.
	nw, err := cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"})
	if err != nil {
		return nil, fmt.Errorf("creating sandbox network: %w", err)
	}

	sb = &Sandbox{networkID: nw.ID, cli: cli, password: password}
	defer func() {
		if err != nil {
			_ = sb.Destroy(context.Background())
		}
	}()

	rc, err := cli.ImagePull(ctx, cfg.Image, image.PullOptions{})
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", cfg.Image, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	pgPort := nat.Port("5432/tcp")
	created, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: cfg.Image,
			Env: []string{
				"POSTGRES_DB=" + dbName,
				"POSTGRES_USER=" + dbUser,
				"POSTGRES_PASSWORD=" + password,
			},
			Labels:       map[string]string{"firedrill": "sandbox", "firedrill/drill": cfg.Name},
			ExposedPorts: nat.PortSet{pgPort: struct{}{}},
		},
		&container.HostConfig{
			// Loopback only: the sandbox is never exposed beyond this host.
			PortBindings: nat.PortMap{pgPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: ""}}},
			AutoRemove:   false,
			NetworkMode:  container.NetworkMode(nw.ID),
		},
		nil, nil, name)
	if err != nil {
		return nil, fmt.Errorf("creating sandbox container: %w", err)
	}
	sb.ContainerID = created.ID

	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("starting sandbox: %w", err)
	}

	if err := sb.waitReady(ctx); err != nil {
		return nil, err
	}

	insp, err := cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return nil, fmt.Errorf("inspecting sandbox: %w", err)
	}
	bindings := insp.NetworkSettings.Ports[pgPort]
	if len(bindings) == 0 {
		return nil, fmt.Errorf("sandbox has no published port")
	}
	sb.hostPort = bindings[0].HostPort

	// TTL watchdog: destroy no matter what once the deadline passes.
	ttlCtx, cancel := context.WithCancel(context.Background())
	sb.ttlCancel = cancel
	go func() { // #nosec G118 -- watchdog must outlive the request context to guarantee teardown
		select {
		case <-ttlCtx.Done():
		case <-time.After(cfg.TTL):
			_ = sb.Destroy(context.Background())
		}
	}()

	return sb, nil
}

// waitReady polls pg_isready inside the container until Postgres accepts
// connections as the drill user (the entrypoint restarts once during init).
func (s *Sandbox) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		code, _, err := s.Exec(ctx, []string{"pg_isready", "-U", dbUser, "-d", dbName})
		if err == nil && code == 0 {
			// pg_isready can pass during the init-phase restart; confirm with a real query.
			code, _, err = s.Exec(ctx, []string{"psql", "-U", dbUser, "-d", dbName, "-c", "select 1"})
			if err == nil && code == 0 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("sandbox postgres not ready after 2m")
}

// Exec runs a command inside the sandbox container and returns its exit
// code and combined output. stdin, when non-nil, is streamed to the command.
func (s *Sandbox) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, string, error) {
	eo := execOpts{}
	for _, o := range opts {
		o(&eo)
	}
	execCfg := container.ExecOptions{
		Cmd:          cmd,
		Env:          eo.env,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  eo.stdin != nil,
	}
	created, err := s.cli.ContainerExecCreate(ctx, s.ContainerID, execCfg)
	if err != nil {
		return -1, "", fmt.Errorf("exec create: %w", err)
	}
	att, err := s.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return -1, "", fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()

	if eo.stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, eo.stdin)
			_ = att.CloseWrite()
		}()
	}
	out, _ := io.ReadAll(att.Reader) // multiplexed stream; fine for diagnostics
	insp, err := s.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return -1, string(out), fmt.Errorf("exec inspect: %w", err)
	}
	return insp.ExitCode, string(out), nil
}

type execOpts struct {
	stdin io.Reader
	env   []string
}

type ExecOption func(*execOpts)

func WithStdin(r io.Reader) ExecOption { return func(o *execOpts) { o.stdin = r } }
func WithEnv(env ...string) ExecOption { return func(o *execOpts) { o.env = env } }

// DSN returns a connection string reachable from this host only.
func (s *Sandbox) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
		dbUser, s.password, s.hostPort, dbName)
}

// User and DB expose the sandbox credentials needed for in-container tools.
func (s *Sandbox) User() string { return dbUser }
func (s *Sandbox) DB() string   { return dbName }

// Destroy force-removes the container and network. Idempotent.
func (s *Sandbox) Destroy(ctx context.Context) error {
	s.destroyOnce.Do(func() {
		if s.ttlCancel != nil {
			s.ttlCancel()
		}
		if s.ContainerID != "" {
			if err := s.cli.ContainerRemove(ctx, s.ContainerID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				s.destroyErr = fmt.Errorf("removing container: %w", err)
			}
		}
		if s.networkID != "" {
			if err := s.cli.NetworkRemove(ctx, s.networkID); err != nil && s.destroyErr == nil {
				s.destroyErr = fmt.Errorf("removing network: %w", err)
			}
		}
		s.Destroyed = s.destroyErr == nil
	})
	return s.destroyErr
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return hex.EncodeToString(b)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, s)
}

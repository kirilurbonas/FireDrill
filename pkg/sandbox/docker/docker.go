// Package docker provisions ephemeral, isolated database sandboxes as
// Docker containers. Sandboxes bind to loopback only, live on their own
// bridge network, use random one-off credentials, and are force-destroyed
// when the TTL expires — even if the drill hangs. Engine specifics
// (environment, readiness, ports) come from the drill's driver.
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

	"net/netip"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kirilurbonas/FireDrill/pkg/drivers"
	"github.com/kirilurbonas/FireDrill/pkg/sandbox"
)

const (
	dbName = "firedrill"
	dbUser = "firedrill"
)

// Sandbox is a running, isolated database container.
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

var (
	_ drivers.Sandbox = (*Sandbox)(nil)
	_ sandbox.Sandbox = (*Sandbox)(nil)
)

// Provision pulls the image, creates an isolated network + container, waits
// until the engine accepts connections, and arms the TTL watchdog.
func Provision(ctx context.Context, cfg sandbox.Config) (sb *Sandbox, err error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("connecting to docker: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("cannot reach the Docker daemon — is Docker running? (%w)", err)
	}

	suffix := randomHex(4)
	name := fmt.Sprintf("firedrill-%s-%s", sanitize(cfg.Name), suffix)
	password := randomHex(16)

	// Dedicated bridge network: the sandbox cannot be reached from (or
	// confused with) anything else on the default bridge.
	nw, err := cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge"})
	if err != nil {
		return nil, fmt.Errorf("creating sandbox network: %w", err)
	}

	sb = &Sandbox{networkID: nw.ID, cli: cli, password: password}
	defer func() {
		if err != nil {
			_ = sb.Destroy(context.Background())
		}
	}()

	pull, err := cli.ImagePull(ctx, cfg.Image, client.ImagePullOptions{})
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", cfg.Image, err)
	}
	if err := pull.Wait(ctx); err != nil {
		_ = pull.Close()
		return nil, fmt.Errorf("pulling %s: %w", cfg.Image, err)
	}
	_ = pull.Close()

	enginePort, err := network.ParsePort(cfg.Driver.Port())
	if err != nil {
		return nil, fmt.Errorf("driver port: %w", err)
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        cfg.Image,
			Env:          cfg.Driver.ContainerEnv(dbUser, password, dbName),
			Labels:       map[string]string{"firedrill": "sandbox", "firedrill/drill": cfg.Name},
			ExposedPorts: network.PortSet{enginePort: struct{}{}},
		},
		HostConfig: &container.HostConfig{
			// Loopback only: the sandbox is never exposed beyond this host.
			PortBindings: network.PortMap{enginePort: []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: ""}}},
			AutoRemove:   false,
			NetworkMode:  container.NetworkMode(nw.ID),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating sandbox container: %w", err)
	}
	sb.ContainerID = created.ID

	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("starting sandbox: %w", err)
	}

	if err := sb.waitReady(ctx, cfg.Driver.ReadyCmds(dbUser, password, dbName)); err != nil {
		return nil, err
	}

	insp, err := cli.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspecting sandbox: %w", err)
	}
	if insp.Container.NetworkSettings == nil {
		return nil, fmt.Errorf("sandbox has no network settings")
	}
	bindings := insp.Container.NetworkSettings.Ports[enginePort]
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

// waitReady polls the driver's readiness commands until every one exits 0
// in a single pass (database entrypoints often restart once during init).
func (s *Sandbox) waitReady(ctx context.Context, cmds [][]string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ready := true
		for _, cmd := range cmds {
			code, _, err := s.Exec(ctx, cmd, nil)
			if err != nil || code != 0 {
				ready = false
				break
			}
		}
		if ready {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("sandbox database not ready after 3m")
}

// Exec runs a command inside the sandbox container and returns its exit
// code and combined output. stdin, when non-nil, is streamed to the command.
func (s *Sandbox) Exec(ctx context.Context, cmd []string, stdin io.Reader) (int, string, error) {
	created, err := s.cli.ExecCreate(ctx, s.ContainerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  stdin != nil,
	})
	if err != nil {
		return -1, "", fmt.Errorf("exec create: %w", err)
	}
	att, err := s.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, "", fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()

	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			_ = att.CloseWrite()
		}()
	}
	out, _ := io.ReadAll(att.Reader) // multiplexed stream; fine for diagnostics
	insp, err := s.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, string(out), fmt.Errorf("exec inspect: %w", err)
	}
	return insp.ExitCode, string(out), nil
}

// Connection facts for drivers (drivers.Sandbox).
func (s *Sandbox) Host() string       { return "127.0.0.1" }
func (s *Sandbox) HostPort() string   { return s.hostPort }
func (s *Sandbox) User() string       { return dbUser }
func (s *Sandbox) Password() string   { return s.password }
func (s *Sandbox) DB() string         { return dbName }
func (s *Sandbox) WasDestroyed() bool { return s.Destroyed }

// Destroy force-removes the container and network. Idempotent.
func (s *Sandbox) Destroy(ctx context.Context) error {
	s.destroyOnce.Do(func() {
		if s.ttlCancel != nil {
			s.ttlCancel()
		}
		if s.ContainerID != "" {
			if _, err := s.cli.ContainerRemove(ctx, s.ContainerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				s.destroyErr = fmt.Errorf("removing container: %w", err)
			}
		}
		if s.networkID != "" {
			if _, err := s.cli.NetworkRemove(ctx, s.networkID, client.NetworkRemoveOptions{}); err != nil && s.destroyErr == nil {
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

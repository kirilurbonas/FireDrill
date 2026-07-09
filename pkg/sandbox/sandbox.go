// Package sandbox defines the provider abstraction for ephemeral, isolated
// database sandboxes. Implementations: docker (local), kubernetes (in-cluster
// or via kubeconfig).
package sandbox

import (
	"context"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/drivers"
)

// Sandbox is a running sandbox a drill restores into. It extends the
// driver-facing view with lifecycle control.
type Sandbox interface {
	drivers.Sandbox
	// Destroy tears the sandbox down. Idempotent; guaranteed by callers via
	// defer and backstopped by the provider's TTL watchdog.
	Destroy(ctx context.Context) error
	// WasDestroyed reports whether teardown completed successfully.
	WasDestroyed() bool
}

// Config describes the sandbox to provision, independent of provider.
type Config struct {
	Image     string        // e.g. postgres:16
	TTL       time.Duration // hard teardown deadline
	Name      string        // drill name, used for labels/naming
	Driver    drivers.Driver
	Namespace string // kubernetes only; defaults to "firedrill"
}

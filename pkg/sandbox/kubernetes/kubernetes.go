// Package kubernetes provisions ephemeral database sandboxes as pods.
// The pod runs in a dedicated namespace with a deny-all egress
// NetworkPolicy (the sandbox can never reach production), random one-off
// credentials, and a TTL watchdog that force-deletes it.
//
// Connectivity: in-cluster (operator) the drill talks to the pod IP
// directly; out-of-cluster (CLI with a kubeconfig) it opens a local
// port-forward through the API server.
package kubernetes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"

	"github.com/kirilurbonas/FireDrill/pkg/drivers"
	"github.com/kirilurbonas/FireDrill/pkg/sandbox"
)

const (
	dbName = "firedrill"
	dbUser = "firedrill"
)

// Sandbox is a running, isolated database pod.
type Sandbox struct {
	cli       kubernetes.Interface
	cfg       *rest.Config
	namespace string
	podName   string
	password  string
	inCluster bool

	host     string
	hostPort string

	pfStop      chan struct{}
	destroyOnce sync.Once
	destroyErr  error
	ttlCancel   context.CancelFunc
	destroyed   bool
}

var (
	_ drivers.Sandbox = (*Sandbox)(nil)
	_ sandbox.Sandbox = (*Sandbox)(nil)
)

// Provision creates the namespace (if needed), a deny-egress NetworkPolicy,
// and the sandbox pod; waits for the engine to accept work; and arms the TTL.
func Provision(ctx context.Context, cfg sandbox.Config) (sb *Sandbox, err error) {
	restCfg, inCluster, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	cli, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = "firedrill"
	}
	password := randomHex(16)
	podName := fmt.Sprintf("firedrill-%s-%s", sanitize(cfg.Name), randomHex(4))
	// Capture the sandbox in a local for cleanup: `return nil, err` resets
	// the named return before deferred functions run.
	s := &Sandbox{cli: cli, cfg: restCfg, namespace: ns, podName: podName, password: password, inCluster: inCluster}
	sb = s
	defer func() {
		if err != nil {
			_ = s.Destroy(context.Background())
		}
	}()

	if err := ensureNamespace(ctx, cli, ns); err != nil {
		return nil, err
	}
	if err := EnsureDenyEgressPolicy(ctx, cli, ns); err != nil {
		return nil, err
	}

	port := intstr.Parse(strings.TrimSuffix(cfg.Driver.Port(), "/tcp"))
	var command []string
	if cfg.ColdStart {
		// The engine must not start yet — the driver places the data
		// directory first, then starts it via Exec.
		command = []string{"sleep", "infinity"}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "firedrill", "firedrill/sandbox": "true", "firedrill/drill": sanitize(cfg.Name)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "db",
				Image:   cfg.Image,
				Command: command,
				Env:     envVars(cfg.Driver.ContainerEnv(dbUser, password, dbName)),
				Ports: []corev1.ContainerPort{{ContainerPort: int32(port.IntValue() & 0xffff)}}, // #nosec G115 -- valid TCP port from driver constant
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
				},
			}},
		},
	}
	if _, err := cli.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating sandbox pod: %w", err)
	}

	var readyCmds [][]string
	if !cfg.ColdStart {
		readyCmds = cfg.Driver.ReadyCmds(dbUser, password, dbName)
	}
	if err := s.waitReady(ctx, readyCmds); err != nil {
		return nil, err
	}

	if inCluster {
		p, err := cli.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		s.host = p.Status.PodIP
		s.hostPort = port.String()
	} else {
		localPort, stop, err := s.portForward(port.IntValue())
		if err != nil {
			return nil, fmt.Errorf("port-forward to sandbox: %w", err)
		}
		s.host = "127.0.0.1"
		s.hostPort = fmt.Sprintf("%d", localPort)
		s.pfStop = stop
	}

	ttlCtx, cancel := context.WithCancel(context.Background())
	s.ttlCancel = cancel
	go func() { // #nosec G118 -- watchdog must outlive the request context to guarantee teardown
		select {
		case <-ttlCtx.Done():
		case <-time.After(cfg.TTL):
			_ = s.Destroy(context.Background())
		}
	}()
	return sb, nil
}

// LoadConfig prefers in-cluster config, falling back to the kubeconfig chain.
func LoadConfig() (*rest.Config, bool, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, true, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		return nil, false, fmt.Errorf("no kubernetes config (in-cluster or kubeconfig): %w", err)
	}
	return c, false, nil
}

func (s *Sandbox) waitReady(ctx context.Context, cmds [][]string) error {
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p, err := s.cli.CoreV1().Pods(s.namespace).Get(ctx, s.podName, metav1.GetOptions{})
		if err == nil {
			switch p.Status.Phase {
			case corev1.PodFailed, corev1.PodSucceeded:
				return fmt.Errorf("sandbox pod exited (%s)", p.Status.Phase)
			case corev1.PodRunning:
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
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("sandbox database not ready after 4m")
}

// Exec runs a command inside the sandbox pod (SPDY exec, like kubectl exec).
func (s *Sandbox) Exec(ctx context.Context, cmd []string, stdin io.Reader) (int, string, error) {
	req := s.cli.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(s.namespace).Name(s.podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "db",
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.cfg, http.MethodPost, req.URL())
	if err != nil {
		return -1, "", fmt.Errorf("exec setup: %w", err)
	}
	var out bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: &out, Stderr: &out})
	if err != nil {
		var exitErr interface{ ExitStatus() int }
		if ok := errorAs(err, &exitErr); ok {
			return exitErr.ExitStatus(), out.String(), nil
		}
		return -1, out.String(), err
	}
	return 0, out.String(), nil
}

// portForward opens a local forward to the pod and returns the local port.
func (s *Sandbox) portForward(podPort int) (int, chan struct{}, error) {
	req := s.cli.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(s.namespace).Name(s.podName).SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(s.cfg)
	if err != nil {
		return 0, nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	stop := make(chan struct{})
	ready := make(chan struct{})
	pf, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", podPort)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- pf.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-errCh:
		return 0, nil, err
	case <-time.After(30 * time.Second):
		close(stop)
		return 0, nil, fmt.Errorf("port-forward not ready after 30s")
	}
	ports, err := pf.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stop)
		return 0, nil, fmt.Errorf("port-forward ports: %w", err)
	}
	return int(ports[0].Local), stop, nil
}

// Connection facts (drivers.Sandbox).
func (s *Sandbox) Host() string       { return s.host }
func (s *Sandbox) HostPort() string   { return s.hostPort }
func (s *Sandbox) User() string       { return dbUser }
func (s *Sandbox) Password() string   { return s.password }
func (s *Sandbox) DB() string         { return dbName }
func (s *Sandbox) WasDestroyed() bool { return s.destroyed }

// Destroy force-deletes the pod. The namespace and policy stay (shared).
func (s *Sandbox) Destroy(ctx context.Context) error {
	s.destroyOnce.Do(func() {
		if s.ttlCancel != nil {
			s.ttlCancel()
		}
		if s.pfStop != nil {
			close(s.pfStop)
		}
		zero := int64(0)
		err := s.cli.CoreV1().Pods(s.namespace).Delete(ctx, s.podName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		if err != nil && !apierrors.IsNotFound(err) {
			s.destroyErr = fmt.Errorf("deleting sandbox pod: %w", err)
		}
		s.destroyed = s.destroyErr == nil
	})
	return s.destroyErr
}

func ensureNamespace(ctx context.Context, cli kubernetes.Interface, ns string) error {
	_, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "firedrill"}},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// EnsureDenyEgressPolicy blocks all egress from sandbox pods: a restored
// database must never be able to reach production. (Enforcement requires a
// NetworkPolicy-capable CNI.)
func EnsureDenyEgressPolicy(ctx context.Context, cli kubernetes.Interface, ns string) error {
	pol := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "firedrill-sandbox-deny-egress", Namespace: ns},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"firedrill/sandbox": "true"}},
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeEgress},
			Egress:      nil, // no rules = deny all egress
		},
	}
	_, err := cli.NetworkingV1().NetworkPolicies(ns).Create(ctx, pol, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func envVars(env []string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out = append(out, corev1.EnvVar{Name: k, Value: v})
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func errorAs[T any](err error, target *T) bool {
	for err != nil {
		if t, ok := err.(T); ok { //nolint:errorlint // manual unwrap loop
			*target = t
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
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

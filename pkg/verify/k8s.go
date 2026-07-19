package verify

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// stablePolls is how many consecutive all-ready samples podsReady requires:
// a crashlooping pod can flash Ready once, and a single sample would turn
// that into a false "recovery verified".
const stablePolls = 3

// podsReady waits until every pod in the restored namespace reports Ready
// (or is Succeeded, for completed jobs) and STAYS ready across consecutive
// polls. Zero pods is a failure — an empty namespace proves nothing.
func podsReady(ctx context.Context, cli kubernetes.Interface, ns string, c *spec.PodsReadyCheck) Result {
	if cli == nil {
		return Result{Name: "podsReady", Passed: false, Detail: "no kubernetes client configured"}
	}
	deadline := time.Now().Add(c.Timeout.Duration)
	var lastDetail string
	stable := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return Result{Name: "podsReady", Passed: false, Detail: ctx.Err().Error()}
		}
		pods, err := cli.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		switch {
		case err != nil:
			stable = 0
			lastDetail = "listing pods: " + err.Error()
		case len(pods.Items) == 0:
			stable = 0
			lastDetail = "no pods in restored namespace"
		default:
			notReady := 0
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodSucceeded {
					continue
				}
				if !podIsReady(&p) {
					notReady++
				}
			}
			if notReady == 0 {
				stable++
				if stable >= stablePolls {
					return Result{Name: "podsReady", Passed: true,
						Detail: fmt.Sprintf("%d pod(s) ready (stable over %d checks)", len(pods.Items), stablePolls)}
				}
			} else {
				stable = 0
				lastDetail = fmt.Sprintf("%d/%d pod(s) not ready", notReady, len(pods.Items))
			}
		}
		time.Sleep(2 * time.Second)
	}
	return Result{Name: "podsReady", Passed: false,
		Detail: fmt.Sprintf("timeout after %s: %s", c.Timeout, lastDetail)}
}

func podIsReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// resourceCount asserts a minimum number of restored objects of one kind.
func resourceCount(ctx context.Context, cli kubernetes.Interface, ns string, c *spec.ResourceCountCheck) Result {
	if cli == nil {
		return Result{Name: "resourceCount", Passed: false, Detail: "no kubernetes client configured"}
	}
	var n int
	var err error
	opts := metav1.ListOptions{}
	switch c.Kind {
	case "deployments":
		if l, e := cli.AppsV1().Deployments(ns).List(ctx, opts); e != nil {
			err = e
		} else {
			n = len(l.Items)
		}
	case "statefulsets":
		if l, e := cli.AppsV1().StatefulSets(ns).List(ctx, opts); e != nil {
			err = e
		} else {
			n = len(l.Items)
		}
	case "services":
		if l, e := cli.CoreV1().Services(ns).List(ctx, opts); e != nil {
			err = e
		} else {
			n = len(l.Items)
		}
	case "configmaps":
		if l, e := cli.CoreV1().ConfigMaps(ns).List(ctx, opts); e != nil {
			err = e
		} else {
			n = len(l.Items)
		}
	case "secrets":
		if l, e := cli.CoreV1().Secrets(ns).List(ctx, opts); e != nil {
			err = e
		} else {
			n = len(l.Items)
		}
	case "pods":
		if l, e := cli.CoreV1().Pods(ns).List(ctx, opts); e != nil {
			err = e
		} else {
			n = len(l.Items)
		}
	default:
		return Result{Name: "resourceCount", Passed: false, Detail: fmt.Sprintf("unsupported kind %q", c.Kind)}
	}
	if err != nil {
		return Result{Name: "resourceCount", Passed: false, Detail: "listing " + c.Kind + ": " + err.Error()}
	}
	return Result{
		Name:   "resourceCount",
		Passed: n >= c.Min,
		Detail: fmt.Sprintf("%d %s (min %d)", n, c.Kind, c.Min),
	}
}

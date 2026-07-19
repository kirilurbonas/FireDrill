package verify

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

func pod(name string, ready bool) *corev1.Pod {
	const ns = "drill"
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func TestPodsReady(t *testing.T) {
	// The stability debounce needs stablePolls consecutive samples at 2s
	// intervals, so the pass case needs a timeout comfortably above that.
	cli := fake.NewClientset(pod("a", true), pod("b", true))
	r := podsReady(context.Background(), cli, "drill", &spec.PodsReadyCheck{Timeout: spec.Duration{Duration: 15 * time.Second}})
	if !r.Passed || !strings.Contains(r.Detail, "stable") {
		t.Errorf("expected stable pass: %+v", r)
	}

	cli = fake.NewClientset(pod("a", true), pod("b", false))
	r = podsReady(context.Background(), cli, "drill", &spec.PodsReadyCheck{Timeout: spec.Duration{Duration: 3 * time.Second}})
	if r.Passed {
		t.Errorf("expected fail with unready pod: %+v", r)
	}

	// Nil client must fail gracefully, never panic.
	r = podsReady(context.Background(), nil, "drill", &spec.PodsReadyCheck{Timeout: spec.Duration{Duration: time.Second}})
	if r.Passed {
		t.Errorf("nil client must fail: %+v", r)
	}

	// Empty namespace must fail — it proves nothing.
	cli = fake.NewClientset()
	r = podsReady(context.Background(), cli, "drill", &spec.PodsReadyCheck{Timeout: spec.Duration{Duration: 2 * time.Second}})
	if r.Passed {
		t.Errorf("expected fail with zero pods: %+v", r)
	}
}

func TestResourceCount(t *testing.T) {
	cli := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "drill"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "drill"}},
	)
	r := resourceCount(context.Background(), cli, "drill", &spec.ResourceCountCheck{Kind: "deployments", Min: 1})
	if !r.Passed {
		t.Errorf("deployments: %+v", r)
	}
	r = resourceCount(context.Background(), cli, "drill", &spec.ResourceCountCheck{Kind: "configmaps", Min: 2})
	if r.Passed {
		t.Errorf("expected fail, only 1 configmap: %+v", r)
	}
	r = resourceCount(context.Background(), cli, "drill", &spec.ResourceCountCheck{Kind: "secrets", Min: 0})
	if !r.Passed {
		t.Errorf("secrets min 0: %+v", r)
	}
}

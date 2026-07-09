package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

func failedEvidence() *report.Evidence {
	e := &report.Evidence{
		Drill:    "payments-db",
		Verified: false,
		Checks: []verify.Result{
			{Name: "restoreSucceeded", Passed: true},
			{Name: "rowCount", Passed: false, Detail: "10 rows (min 100)"},
		},
	}
	e.Objectives.RTO = "15m0s"
	e.Objectives.RPO = "24h0m0s"
	e.Measured.RTOMet = true
	return e
}

func TestSlackPostsFailure(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer srv.Close()
	t.Setenv("TEST_SLACK_HOOK", srv.URL)

	err := Slack(context.Background(), failedEvidence(), spec.Sink{Type: "slack", WebhookEnv: "TEST_SLACK_HOOK"})
	if err != nil {
		t.Fatalf("Slack: %v", err)
	}
	for _, want := range []string{"NOT verified", "payments-db", "rowCount", "10 rows (min 100)", "missed"} {
		if !strings.Contains(got, want) {
			t.Errorf("payload missing %q:\n%s", want, got)
		}
	}
}

func TestSlackOnlyFailuresSkipsVerified(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	t.Setenv("TEST_SLACK_HOOK", srv.URL)

	e := failedEvidence()
	e.Verified = true
	if err := Slack(context.Background(), e, spec.Sink{Type: "slack", WebhookEnv: "TEST_SLACK_HOOK", OnlyFailures: true}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("verified drill should not notify with onlyFailures")
	}
}

func TestSlackMissingEnv(t *testing.T) {
	if err := Slack(context.Background(), failedEvidence(), spec.Sink{Type: "slack", WebhookEnv: "DOES_NOT_EXIST_XYZ"}); err == nil {
		t.Error("expected error for unset env var")
	}
}

func TestSlackNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("TEST_SLACK_HOOK", srv.URL)
	if err := Slack(context.Background(), failedEvidence(), spec.Sink{Type: "slack", WebhookEnv: "TEST_SLACK_HOOK"}); err == nil {
		t.Error("expected error for 403 response")
	}
}

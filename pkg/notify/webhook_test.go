package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

func TestWebhookPostsEvidence(t *testing.T) {
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	}))
	defer srv.Close()
	t.Setenv("TEST_WEBHOOK", srv.URL)

	sink := spec.Sink{Type: "webhook", URLEnv: "TEST_WEBHOOK"}
	if err := Webhook(context.Background(), failedEvidence(), sink); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	// The body is the evidence record itself, so a receiver can store it.
	var got report.Evidence
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not evidence JSON: %v (%s)", err, body)
	}
	if got.Drill != "payments-db" || got.Verified {
		t.Errorf("unexpected evidence posted: %+v", got)
	}
	if hdr.Get("X-FireDrill-Event") != "drill.failed" || hdr.Get("X-FireDrill-Drill") != "payments-db" {
		t.Errorf("routing headers = %q / %q", hdr.Get("X-FireDrill-Event"), hdr.Get("X-FireDrill-Drill"))
	}
}

func TestWebhookOnlyFailuresAndErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("TEST_WEBHOOK", srv.URL)

	verified := failedEvidence()
	verified.Verified = true
	sink := spec.Sink{Type: "webhook", URLEnv: "TEST_WEBHOOK", OnlyFailures: true}
	if err := Webhook(context.Background(), verified, sink); err != nil || calls != 0 {
		t.Errorf("verified drill should not notify: err=%v calls=%d", err, calls)
	}

	// A non-2xx response is surfaced (the caller downgrades it to a warning).
	if err := Webhook(context.Background(), failedEvidence(), sink); err == nil {
		t.Error("expected an error for a 500 response")
	}

	// An unset env var is a configuration error, not a silent no-op.
	if err := Webhook(context.Background(), failedEvidence(), spec.Sink{Type: "webhook", URLEnv: "FIREDRILL_UNSET_HOOK"}); err == nil {
		t.Error("expected an error when the env var is unset")
	}
}

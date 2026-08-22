package notify

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Webhook posts the drill's evidence to an arbitrary HTTP endpoint — the
// escape hatch for Teams, Discord, PagerDuty, or an internal service. The
// body is the same canonical evidence JSON that gets signed, so a receiver
// can store it verbatim; the event type is in a header so simple consumers
// can route on failures without parsing.
//
// As with Slack, the URL is read from the environment variable named by the
// sink so the secret never appears in a spec or in evidence.
func Webhook(ctx context.Context, e *report.Evidence, sink spec.Sink) error {
	if sink.OnlyFailures && e.Verified {
		return nil
	}
	url := os.Getenv(sink.URLEnv)
	if url == "" {
		return fmt.Errorf("webhook sink: env var %s is empty or unset", sink.URLEnv)
	}

	body, err := report.Canonical(e)
	if err != nil {
		return fmt.Errorf("webhook sink: %w", err)
	}
	// #nosec G704 -- the webhook URL is operator-configured via environment, not request input
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "firedrill")
	req.Header.Set("X-FireDrill-Event", event(e))
	req.Header.Set("X-FireDrill-Drill", e.Drill)

	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Do(req) // #nosec G704 -- operator-configured webhook URL
	if err != nil {
		return fmt.Errorf("webhook sink: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook sink: %s returned %s", sink.URLEnv, resp.Status)
	}
	return nil
}

func event(e *report.Evidence) string {
	if e.Verified {
		return "drill.verified"
	}
	return "drill.failed"
}

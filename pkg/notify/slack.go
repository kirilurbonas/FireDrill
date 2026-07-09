// Package notify sends drill outcomes to notification sinks (Slack).
// Notification failures are warnings, never drill failures.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/report"
	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Slack posts the drill outcome to a Slack incoming webhook. The webhook URL
// is read from the environment variable named by sink.WebhookEnv so the
// secret never appears in specs or evidence.
func Slack(ctx context.Context, e *report.Evidence, sink spec.Sink) error {
	if sink.OnlyFailures && e.Verified {
		return nil
	}
	url := os.Getenv(sink.WebhookEnv)
	if url == "" {
		return fmt.Errorf("slack sink: env var %s is empty or unset", sink.WebhookEnv)
	}

	payload := map[string]any{"text": Message(e)}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// #nosec G704 -- the webhook URL is operator-configured via environment, not request input
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Do(req) // #nosec G704 -- operator-configured webhook URL
	if err != nil {
		return fmt.Errorf("slack sink: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack sink: webhook returned %s", resp.Status)
	}
	return nil
}

// Message renders the human-readable drill summary posted to Slack.
func Message(e *report.Evidence) string {
	var b strings.Builder
	if e.Verified {
		fmt.Fprintf(&b, ":white_check_mark: *FireDrill: recovery verified* — drill `%s`\n", e.Drill)
	} else {
		fmt.Fprintf(&b, ":rotating_light: *FireDrill: recovery NOT verified* — drill `%s`\n", e.Drill)
	}
	rto := time.Duration(e.Measured.RestoreSeconds * float64(time.Second)).Round(time.Second)
	age := time.Duration(e.Backup.AgeSecs * float64(time.Second)).Round(time.Minute)
	fmt.Fprintf(&b, "> RTO %s (target %s, %s) · RPO %s (target %s, %s)\n",
		rto, e.Objectives.RTO, met(e.Measured.RTOMet), age, e.Objectives.RPO, met(e.Measured.RPOMet))
	var failed []string
	for _, c := range e.Checks {
		if !c.Passed && !c.Skipped {
			failed = append(failed, fmt.Sprintf("`%s` (%s)", c.Name, c.Detail))
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "> failed checks: %s\n", strings.Join(failed, ", "))
	}
	if e.Error != "" {
		fmt.Fprintf(&b, "> error: %s\n", truncate(e.Error, 300))
	}
	return b.String()
}

func met(ok bool) string {
	if ok {
		return "met"
	}
	return "missed"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

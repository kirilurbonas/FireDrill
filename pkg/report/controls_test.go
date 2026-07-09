package report

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

func writeEvidence(t *testing.T, dir, drill string, verified bool, controls []string, sign bool) {
	t.Helper()
	e := &Evidence{
		Drill:      drill,
		FinishedAt: time.Unix(1770000000, 0).UTC(),
		Verified:   verified,
		Controls:   controls,
		Checks:     []verify.Result{{Name: "restoreSucceeded", Passed: verified}},
	}
	e.Measured.RestoreSeconds = 90
	e.Measured.RTOMet = verified
	e.Measured.RPOMet = true
	path, err := e.Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sign {
		keyDir := t.TempDir()
		if _, _, err := GenerateKeypair(keyDir); err != nil {
			t.Fatal(err)
		}
		priv, err := LoadPrivateKey(keyDir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Sign(path, priv); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildControlReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evidence")
	writeEvidence(t, dir, "payments-db", true, []string{"ISO27001-A.8.13", "SOC2-A1.2"}, true)
	writeEvidence(t, dir, "orders-db", false, []string{"ISO27001-A.8.13"}, false)
	writeEvidence(t, dir, "no-controls", true, nil, false)

	rep, err := BuildControlReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Controls["ISO27001-A.8.13"]) != 2 {
		t.Errorf("ISO control entries = %d, want 2", len(rep.Controls["ISO27001-A.8.13"]))
	}
	if len(rep.Controls["SOC2-A1.2"]) != 1 {
		t.Errorf("SOC2 control entries = %d, want 1", len(rep.Controls["SOC2-A1.2"]))
	}
	if rep.Unattributed != 1 {
		t.Errorf("unattributed = %d, want 1", rep.Unattributed)
	}
	for _, e := range rep.Controls["ISO27001-A.8.13"] {
		if e.Drill == "payments-db" && !e.Signed {
			t.Error("payments-db evidence should be signed")
		}
		if e.Drill == "orders-db" && e.Signed {
			t.Error("orders-db evidence should not be marked signed")
		}
	}

	var md strings.Builder
	if err := rep.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	out := md.String()
	for _, want := range []string{"## ISO27001-A.8.13", "## SOC2-A1.2", "✅ verified", "❌ failed", "1m30s", "declare no controls"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q\n%s", want, out)
		}
	}
}

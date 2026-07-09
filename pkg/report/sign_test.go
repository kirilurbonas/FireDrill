package report

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

func TestSignAndVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := GenerateKeypair(dir); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if _, _, err := GenerateKeypair(dir); err == nil {
		t.Fatal("keygen should refuse to overwrite")
	}
	priv, err := LoadPrivateKey(dir)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}

	e := &Evidence{Drill: "test", FinishedAt: time.Now(), Checks: []verify.Result{{Name: "smoke", Passed: true}}}
	path, err := e.Write(filepath.Join(dir, "evidence"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Sign(path, priv); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := Verify(path, nil); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Tampering must break verification.
	data, _ := os.ReadFile(path) // #nosec G304 -- test-owned temp path
	data[len(data)-3] ^= 0xff
	// #nosec G703 -- test-owned temp path
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(path, nil); err == nil {
		t.Fatal("verify should fail on tampered evidence")
	}
}

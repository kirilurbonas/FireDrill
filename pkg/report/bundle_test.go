package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

// signedEvidence writes a signed + attested evidence file in its own
// directory and returns its path along with the signing key dir.
func signedEvidence(t *testing.T, drill string) (path, keyDir string) {
	t.Helper()
	keyDir = t.TempDir()
	if _, _, err := GenerateKeypair(keyDir); err != nil {
		t.Fatal(err)
	}
	key, err := LoadPrivateKey(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	e := &Evidence{Drill: drill, FinishedAt: time.Unix(1770000000, 0), Verified: true,
		Checks: []verify.Result{{Name: "rowCount", Passed: true}}}
	path, err = e.Write(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(path, key); err != nil {
		t.Fatal(err)
	}
	if _, err := Attest(path, key); err != nil {
		t.Fatal(err)
	}
	return path, keyDir
}

// TestVerifyBundleForAnAuditor is the scenario the tool exists for: evidence
// arrives on a machine that has never seen the signer, and that machine has
// its own unrelated firedrill key lying around. That key must not be used to
// judge someone else's evidence.
func TestVerifyBundleForAnAuditor(t *testing.T) {
	path, _ := signedEvidence(t, "payments-db")

	strangerDir := t.TempDir()
	if _, _, err := GenerateKeypair(strangerDir); err != nil {
		t.Fatal(err)
	}

	res, err := VerifyBundle(path, nil, strangerDir)
	if err != nil {
		t.Fatalf("evidence from another machine must verify: %v", err)
	}
	if !res.AttestationChecked || res.KeySource != "signature envelope" {
		t.Errorf("attestation should be checked against the evidence's own signer, got %+v", res)
	}
	if res.SignerFingerprint == "" {
		t.Error("the signer should be identified in the result")
	}
}

func TestVerifyBundlePinnedKey(t *testing.T) {
	path, keyDir := signedEvidence(t, "payments-db")
	pub, err := LoadPublicKey(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := VerifyBundle(path, pub, "")
	if err != nil {
		t.Fatalf("pinned key that signed the evidence must verify: %v", err)
	}
	if res.KeySource != "pinned key" {
		t.Errorf("KeySource = %q, want pinned key", res.KeySource)
	}

	// A pinned key that did NOT sign this evidence is a hard failure — that
	// is the whole point of pinning.
	otherDir := t.TempDir()
	if _, _, err := GenerateKeypair(otherDir); err != nil {
		t.Fatal(err)
	}
	other, err := LoadPublicKey(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(path, other, ""); err == nil {
		t.Error("expected a failure when the pinned key did not sign the evidence")
	}
}

func TestVerifyBundleTamperedAndMissing(t *testing.T) {
	path, _ := signedEvidence(t, "payments-db")

	data, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"verified": true`, `"verified": false`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil { // #nosec G703 -- path is from t.TempDir()
		t.Fatal(err)
	}
	if _, err := VerifyBundle(path, nil, ""); err == nil {
		t.Fatal("tampered evidence must not verify")
	}

	// Evidence with no attestation (pre-v0.6) verifies, and says so.
	path2, _ := signedEvidence(t, "orders-db")
	if err := os.Remove(path2 + ".intoto.jsonl"); err != nil {
		t.Fatal(err)
	}
	res, err := VerifyBundle(path2, nil, "")
	if err != nil {
		t.Fatalf("evidence without an attestation should still verify: %v", err)
	}
	if res.AttestationChecked || len(res.Notes) == 0 {
		t.Errorf("expected a note that no attestation was present, got %+v", res)
	}
}

func TestSignerKey(t *testing.T) {
	path, keyDir := signedEvidence(t, "payments-db")
	got, err := SignerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := LoadPublicKey(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Error("SignerKey did not return the key that signed the evidence")
	}
	if _, err := SignerKey(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected an error when the signature envelope is missing")
	}
}

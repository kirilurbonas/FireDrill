package report

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kirilurbonas/FireDrill/pkg/verify"
)

func TestAttestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := GenerateKeypair(dir); err != nil {
		t.Fatal(err)
	}
	priv, err := LoadPrivateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := LoadPublicKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	e := &Evidence{Drill: "attest-test", FinishedAt: time.Unix(1770000000, 0),
		Checks: []verify.Result{{Name: "smoke", Passed: true}}}
	path, err := e.Write(filepath.Join(dir, "evidence"))
	if err != nil {
		t.Fatal(err)
	}

	attPath, err := Attest(path, priv)
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if !strings.HasSuffix(attPath, ".intoto.jsonl") {
		t.Errorf("attestation path = %s", attPath)
	}
	if err := VerifyAttestation(path, pub); err != nil {
		t.Fatalf("VerifyAttestation: %v", err)
	}

	// The statement must carry the evidence as predicate and a sha256 subject.
	envData, _ := os.ReadFile(attPath) // #nosec G304 -- test temp dir
	var env envelope
	if err := json.Unmarshal(envData, &env); err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.StdEncoding.DecodeString(env.Payload)
	var st statement
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatal(err)
	}
	if st.PredicateType != PredicateType || len(st.Subject) != 1 || st.Subject[0].Digest["sha256"] == "" {
		t.Errorf("statement malformed: %+v", st)
	}
}

func TestAttestTamperDetection(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := GenerateKeypair(dir); err != nil {
		t.Fatal(err)
	}
	priv, _ := LoadPrivateKey(dir)
	pub, _ := LoadPublicKey(dir)

	e := &Evidence{Drill: "tamper-test", FinishedAt: time.Unix(1770000000, 0), Verified: true}
	path, err := e.Write(filepath.Join(dir, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Attest(path, priv); err != nil {
		t.Fatal(err)
	}

	// 1. Tamper with the evidence file → digest mismatch.
	orig, _ := os.ReadFile(path) // #nosec G304 -- test temp dir
	tampered := []byte(strings.Replace(string(orig), `"verified": true`, `"verified": false`, 1))
	if err := os.WriteFile(path, tampered, 0o600); err != nil { // #nosec G703 -- test temp dir
		t.Fatal(err)
	}
	if err := VerifyAttestation(path, pub); err == nil {
		t.Fatal("expected digest mismatch for tampered evidence")
	}
	if err := os.WriteFile(path, orig, 0o600); err != nil { // #nosec G703 -- test temp dir
		t.Fatal(err)
	}

	// 2. Tamper with the envelope payload → signature failure.
	attPath := path + ".intoto.jsonl"
	envData, _ := os.ReadFile(attPath) // #nosec G304 -- test temp dir
	var env envelope
	if err := json.Unmarshal(envData, &env); err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.StdEncoding.DecodeString(env.Payload)
	payload = []byte(strings.Replace(string(payload), "tamper-test", "tamper-hack", 1))
	env.Payload = base64.StdEncoding.EncodeToString(payload)
	out, _ := json.Marshal(env)
	if err := os.WriteFile(attPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAttestation(path, pub); err == nil {
		t.Fatal("expected signature failure for tampered payload")
	}

	// 3. Wrong key → signature failure.
	otherDir := t.TempDir()
	if _, _, err := GenerateKeypair(otherDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Attest(path, priv); err != nil { // restore valid attestation
		t.Fatal(err)
	}
	otherPub, _ := LoadPublicKey(otherDir)
	if err := VerifyAttestation(path, otherPub); err == nil {
		t.Fatal("expected failure with wrong public key")
	}
}

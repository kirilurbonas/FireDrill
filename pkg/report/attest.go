package report

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PredicateType identifies FireDrill drill evidence in in-toto Statements.
const PredicateType = "https://firedrill.dev/drill-evidence/v1"

// statement is an in-toto Statement (v1) whose predicate is the evidence.
type statement struct {
	Type          string          `json:"_type"`
	Subject       []subject       `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     json.RawMessage `json:"predicate"`
}

type subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// envelope is a DSSE envelope (https://github.com/secure-systems-lab/dsse).
type envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"` // base64(statement JSON)
	Signatures  []signature `json:"signatures"`
}

type signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"` // base64(ed25519 over PAE)
}

const payloadType = "application/vnd.in-toto+json"

// pae computes the DSSE Pre-Authentication Encoding.
func pae(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s",
		len(payloadType), payloadType, len(payload), payload))
}

// Attest wraps the evidence file at path in an in-toto Statement, signs it
// as a DSSE envelope, and writes <path>.intoto.jsonl. Verifiable with
// `firedrill verify-evidence` or `cosign verify-blob-attestation`.
func Attest(path string, priv ed25519.PrivateKey) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path produced by Evidence.Write
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)

	st := statement{
		Type: "https://in-toto.io/Statement/v1",
		Subject: []subject{{
			Name:   filepath.Base(path),
			Digest: map[string]string{"sha256": hex.EncodeToString(sum[:])},
		}},
		PredicateType: PredicateType,
		Predicate:     json.RawMessage(data),
	}
	payload, err := json.Marshal(st)
	if err != nil {
		return "", err
	}

	pub := priv.Public().(ed25519.PublicKey)
	env := envelope{
		PayloadType: payloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures: []signature{{
			KeyID: Fingerprint(pub),
			Sig:   base64.StdEncoding.EncodeToString(ed25519.Sign(priv, pae(payloadType, payload))),
		}},
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	attPath := path + ".intoto.jsonl"
	// #nosec G306 -- attestations ship alongside shareable evidence
	if err := os.WriteFile(attPath, append(out, '\n'), 0o644); err != nil {
		return "", err
	}
	return attPath, nil
}

// VerifyAttestation checks the DSSE envelope at <path>.intoto.jsonl against
// the evidence file at path: signature over the PAE, and the statement's
// subject digest against the file's current content. pub is required (the
// envelope carries only a key fingerprint, not the key).
func VerifyAttestation(path string, pub ed25519.PublicKey) error {
	envData, err := os.ReadFile(path + ".intoto.jsonl") // #nosec G304 -- user-supplied evidence path
	if err != nil {
		return fmt.Errorf("missing attestation: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(envData, &env); err != nil {
		return fmt.Errorf("malformed attestation envelope: %w", err)
	}
	if env.PayloadType != payloadType {
		return fmt.Errorf("unexpected payloadType %q", env.PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return errors.New("malformed attestation payload")
	}
	if len(env.Signatures) == 0 {
		return errors.New("attestation has no signatures")
	}
	verified := false
	for _, s := range env.Signatures {
		raw, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			continue
		}
		if ed25519.Verify(pub, pae(env.PayloadType, payload), raw) {
			verified = true
			break
		}
	}
	if !verified {
		return errors.New("ATTESTATION INVALID — signature does not verify")
	}

	var st statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return fmt.Errorf("malformed statement: %w", err)
	}
	if st.PredicateType != PredicateType {
		return fmt.Errorf("unexpected predicateType %q", st.PredicateType)
	}
	data, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	for _, sub := range st.Subject {
		if strings.EqualFold(sub.Digest["sha256"], want) {
			return nil
		}
	}
	return errors.New("ATTESTATION INVALID — evidence digest does not match subject")
}

// LoadPublicKey reads the drill public key from dir (firedrill.pub).
func LoadPublicKey(dir string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(filepath.Join(dir, "firedrill.pub")) // #nosec G304 -- user-owned key dir
	if err != nil {
		return nil, fmt.Errorf("loading public key: %w", err)
	}
	return ParsePublicKeyPEM(data)
}

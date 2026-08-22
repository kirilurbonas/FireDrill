package report

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// BundleResult describes what verifying an evidence bundle actually proved,
// so the caller can report it honestly rather than implying more or less
// than was checked.
type BundleResult struct {
	// SignerFingerprint identifies the key that signed the evidence.
	SignerFingerprint string
	// AttestationChecked is false when no attestation is present (pre-v0.6
	// evidence) or no key was available to check it with.
	AttestationChecked bool
	// KeySource says where the attestation key came from: "pinned key",
	// "signature envelope" or "key directory".
	KeySource string
	// Notes carry anything the operator should know but that is not a failure.
	Notes []string
}

// SignerKey returns the public key embedded in an evidence file's detached
// signature envelope. Evidence travels with its signer's key, which is what
// makes a bundle verifiable on a machine that has never seen the signer.
func SignerKey(path string) (ed25519.PublicKey, error) {
	sigData, err := os.ReadFile(path + ".sig") // #nosec G304 -- user-supplied evidence path
	if err != nil {
		return nil, fmt.Errorf("missing signature file: %w", err)
	}
	var sig Signature
	if err := json.Unmarshal(sigData, &sig); err != nil {
		return nil, fmt.Errorf("malformed signature envelope: %w", err)
	}
	pub, err := hex.DecodeString(sig.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("malformed public key in signature")
	}
	return ed25519.PublicKey(pub), nil
}

// VerifyBundle checks an evidence file's signature and, when present, its
// in-toto attestation.
//
// The attestation is checked against, in order: the pinned key when the
// caller supplied one; otherwise the key embedded in the evidence's own
// signature envelope, which proves the attestation and the signature come
// from the same signer; and only failing both, a local key directory.
//
// Reaching for a local key that has nothing to do with the evidence is what
// this order exists to prevent: an auditor whose machine happens to hold an
// unrelated firedrill key must not be told the evidence is invalid.
func VerifyBundle(path string, trusted ed25519.PublicKey, keyDir string) (*BundleResult, error) {
	if err := Verify(path, trusted); err != nil {
		return nil, err
	}
	res := &BundleResult{}
	if signer, err := SignerKey(path); err == nil {
		res.SignerFingerprint = Fingerprint(signer)
	}

	if _, err := os.Stat(path + ".intoto.jsonl"); err != nil {
		res.Notes = append(res.Notes, "no attestation present (pre-v0.6 evidence)")
		return res, nil
	}

	att, source := trusted, "pinned key"
	if att == nil {
		var err error
		if att, err = SignerKey(path); err == nil {
			source = "signature envelope"
		} else if keyDir != "" {
			if att, err = LoadPublicKey(keyDir); err != nil {
				res.Notes = append(res.Notes, "attestation not checked (no public key available)")
				return res, nil
			}
			source = "key directory"
		} else {
			res.Notes = append(res.Notes, "attestation not checked (no public key available)")
			return res, nil
		}
	}
	if err := VerifyAttestation(path, att); err != nil {
		return nil, err
	}
	res.AttestationChecked = true
	res.KeySource = source
	return res, nil
}

package report

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Signature is written alongside evidence as <evidence>.sig — a small JSON
// envelope binding the evidence bytes to a public key.
type Signature struct {
	Algorithm      string `json:"algorithm"` // ed25519
	PublicKey      string `json:"publicKey"` // hex
	KeyFingerprint string `json:"keyFingerprint"`
	Signature      string `json:"signature"` // hex, over the evidence file bytes
}

const (
	privPEMType = "FIREDRILL ED25519 PRIVATE KEY"
	pubPEMType  = "FIREDRILL ED25519 PUBLIC KEY"
)

// DefaultKeyDir is where keygen stores the drill signing keypair.
func DefaultKeyDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "firedrill"), nil
}

// GenerateKeypair creates and stores an ed25519 keypair in dir.
// It refuses to overwrite an existing key.
func GenerateKeypair(dir string) (privPath, pubPath string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	privPath = filepath.Join(dir, "firedrill.key")
	pubPath = filepath.Join(dir, "firedrill.pub")
	if _, err := os.Stat(privPath); err == nil {
		return "", "", fmt.Errorf("key already exists at %s (remove it first to rotate)", privPath)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: privPEMType, Bytes: priv})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: pubPEMType, Bytes: pub})
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return "", "", err
	}
	// #nosec G306 -- the public key is public by definition
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return "", "", err
	}
	// Also write a PKIX-encoded copy that cosign and openssl understand:
	// cosign verify-blob-attestation --key firedrill.cosign.pub …
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	cosignPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})
	// #nosec G306 -- public key
	if err := os.WriteFile(filepath.Join(dir, "firedrill.cosign.pub"), cosignPEM, 0o644); err != nil {
		return "", "", err
	}
	return privPath, pubPath, nil
}

// LoadPrivateKey reads the signing key from dir.
func LoadPrivateKey(dir string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(filepath.Join(dir, "firedrill.key")) // #nosec G304 -- user-owned key dir
	if err != nil {
		return nil, fmt.Errorf("loading signing key (run `firedrill keygen`?): %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != privPEMType || len(block.Bytes) != ed25519.PrivateKeySize {
		return nil, errors.New("malformed signing key")
	}
	return ed25519.PrivateKey(block.Bytes), nil
}

// Sign signs the evidence file at path and writes <path>.sig.
func Sign(path string, priv ed25519.PrivateKey) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path produced by Evidence.Write
	if err != nil {
		return "", err
	}
	pub := priv.Public().(ed25519.PublicKey)
	sig := Signature{
		Algorithm:      "ed25519",
		PublicKey:      hex.EncodeToString(pub),
		KeyFingerprint: Fingerprint(pub),
		Signature:      hex.EncodeToString(ed25519.Sign(priv, data)),
	}
	out, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return "", err
	}
	sigPath := path + ".sig"
	// #nosec G306 -- signatures ship alongside shareable evidence
	if err := os.WriteFile(sigPath, out, 0o644); err != nil {
		return "", err
	}
	return sigPath, nil
}

// Verify checks the evidence file against its .sig envelope. If trustedPub
// is non-nil the signature must additionally come from that key.
func Verify(path string, trustedPub ed25519.PublicKey) error {
	data, err := os.ReadFile(path) // #nosec G304 -- user-supplied evidence path
	if err != nil {
		return err
	}
	sigData, err := os.ReadFile(path + ".sig") // #nosec G304
	if err != nil {
		return fmt.Errorf("missing signature file: %w", err)
	}
	var sig Signature
	if err := json.Unmarshal(sigData, &sig); err != nil {
		return fmt.Errorf("malformed signature envelope: %w", err)
	}
	if sig.Algorithm != "ed25519" {
		return fmt.Errorf("unsupported algorithm %q", sig.Algorithm)
	}
	pub, err := hex.DecodeString(sig.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("malformed public key in signature")
	}
	if trustedPub != nil && !trustedPub.Equal(ed25519.PublicKey(pub)) {
		return errors.New("signature key does not match trusted public key")
	}
	raw, err := hex.DecodeString(sig.Signature)
	if err != nil {
		return errors.New("malformed signature")
	}
	if !ed25519.Verify(pub, data, raw) {
		return errors.New("SIGNATURE INVALID — evidence has been modified")
	}
	return nil
}

// ParsePublicKeyPEM decodes a firedrill.pub PEM block.
func ParsePublicKeyPEM(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || len(block.Bytes) != ed25519.PublicKeySize {
		return nil, errors.New("malformed public key PEM")
	}
	return ed25519.PublicKey(block.Bytes), nil
}

// Fingerprint is a short identifier for a public key (sha256, first 16 hex).
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

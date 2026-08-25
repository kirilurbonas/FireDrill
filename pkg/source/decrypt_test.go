package source

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// ageEncrypt encrypts data to the given recipients the way a backup pipeline
// would, optionally ASCII-armored.
func ageEncrypt(t *testing.T, data []byte, armored bool, recipients ...age.Recipient) []byte {
	t.Helper()
	var buf bytes.Buffer
	var dst io.Writer = &buf
	var ac io.WriteCloser
	if armored {
		ac = armor.NewWriter(&buf)
		dst = ac
	}
	w, err := age.Encrypt(dst, recipients...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if ac != nil {
		if err := ac.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestFetchAgeEncrypted(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("PGDMP fake dump payload\n")
	mod := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	cases := []struct {
		name    string
		armored bool
		file    string
	}{
		{"binary", false, "payments.dump.age"},
		{"armored", true, "payments.dump.age"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := writeAt(t, dir, c.file, ageEncrypt(t, payload, c.armored, id.Recipient()), mod)

			keyFile := filepath.Join(dir, "age.key")
			if err := os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			b, err := Fetch(context.Background(), spec.From{
				Type: "file", URI: src,
				Decrypt: &spec.Decrypt{Type: "age", IdentityFile: keyFile},
			})
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			defer func() { _ = b.Cleanup() }()

			got, err := os.ReadFile(b.Path) // #nosec G304 -- test temp dir
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("plaintext = %q (%v), want %q", got, err, payload)
			}
			if b.Encryption != Age {
				t.Errorf("encryption = %q, want age", b.Encryption)
			}
			if b.Path == src {
				t.Error("plaintext must go to a temp file, not the encrypted source")
			}
			// RPO still measures when the backup was taken.
			if !b.ModTime.Equal(mod) {
				t.Errorf("modTime = %s, want %s", b.ModTime, mod)
			}
		})
	}
}

// TestFetchAgeEncryptedCompressed covers what pipelines actually produce:
// dump | gzip | age. Decryption has to happen first.
func TestFetchAgeEncryptedCompressed(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("ledger row\n", 500))
	dir := t.TempDir()
	src := writeAt(t, dir, "payments.dump.gz.age",
		ageEncrypt(t, gzipBytes(t, payload), false, id.Recipient()), time.Now())

	t.Setenv("FIREDRILL_TEST_AGE_KEY", id.String())
	b, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: src,
		Decrypt: &spec.Decrypt{Type: "age", IdentityEnv: "FIREDRILL_TEST_AGE_KEY"},
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = b.Cleanup() }()

	got, err := os.ReadFile(b.Path) // #nosec G304 -- test temp dir
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("plaintext mismatch (%d bytes, err %v)", len(got), err)
	}
	if b.Encryption != Age || b.Compression != Gzip {
		t.Errorf("layers = %q/%q, want age/gzip", b.Encryption, b.Compression)
	}
	if b.Layers() != "age, gzip" {
		t.Errorf("Layers() = %q", b.Layers())
	}
}

func TestFetchAgePassphrase(t *testing.T) {
	r, err := age.NewScryptRecipient("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkFactor(10) // keep the test fast; production artifacts use age's default
	payload := []byte("dump\n")
	dir := t.TempDir()
	src := writeAt(t, dir, "b.dump.age", ageEncrypt(t, payload, false, r), time.Now())

	t.Setenv("FIREDRILL_TEST_PASS", "correct horse battery staple")
	b, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: src,
		Decrypt: &spec.Decrypt{Type: "age", PassphraseEnv: "FIREDRILL_TEST_PASS"},
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = b.Cleanup() }()
	got, _ := os.ReadFile(b.Path) // #nosec G304 -- test temp dir
	if !bytes.Equal(got, payload) {
		t.Errorf("plaintext = %q", got)
	}
}

func TestDecryptFailures(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	other, _ := age.GenerateX25519Identity()
	dir := t.TempDir()
	src := writeAt(t, dir, "b.dump.age", ageEncrypt(t, []byte("secret"), false, id.Recipient()), time.Now())

	// The wrong key must fail loudly, not produce garbage the restore then
	// reports as a corrupt backup.
	t.Setenv("FIREDRILL_TEST_AGE_KEY", other.String())
	_, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: src,
		Decrypt: &spec.Decrypt{Type: "age", IdentityEnv: "FIREDRILL_TEST_AGE_KEY"},
	})
	if err == nil || !strings.Contains(err.Error(), "wrong identity") {
		t.Errorf("expected a wrong-identity error, got %v", err)
	}

	// An encrypted artifact with no decrypt block: the error must say what
	// to do, not leave pg_restore to choke on ciphertext.
	_, err = Fetch(context.Background(), spec.From{Type: "file", URI: src})
	if err == nil || !strings.Contains(err.Error(), "source.from.decrypt") {
		t.Errorf("expected an actionable error, got %v", err)
	}

	// An unset key variable is a configuration error.
	_, err = Fetch(context.Background(), spec.From{
		Type: "file", URI: src,
		Decrypt: &spec.Decrypt{Type: "age", IdentityEnv: "FIREDRILL_TEST_UNSET"},
	})
	if err == nil || !strings.Contains(err.Error(), "FIREDRILL_TEST_UNSET") {
		t.Errorf("expected an unset-env error, got %v", err)
	}
}

func TestDetectEncryption(t *testing.T) {
	cases := map[string]string{
		"age-encryption.org/v1\n-> X25519 abc": Age,
		armor.Header + "\nYWdlLWVuY3J5":        Age,
		"-----BEGIN PGP MESSAGE-----\nhQIMA":   GPG,
		"\x85\x01\x0c\x03":                     GPG,
		"PGDMP\x01\x0e":                        "",
		"":                                     "",
	}
	for in, want := range cases {
		if got := detectEncryption([]byte(in)); got != want {
			t.Errorf("detectEncryption(%.24q) = %q, want %q", in, got, want)
		}
	}
}

// TestFetchGPGEncrypted exercises the gpg path against the real binary, the
// way an operator with an existing GPG pipeline would.
func TestFetchGPGEncrypted(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	dir := t.TempDir()
	payload := []byte("PGDMP gpg payload\n")
	plain := filepath.Join(dir, "plain.dump")
	if err := os.WriteFile(plain, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	enc := filepath.Join(dir, "payments.dump.gpg")
	// #nosec G204 -- fixed args, test-local paths
	cmd := exec.Command("gpg", "--batch", "--yes", "--quiet",
		"--pinentry-mode", "loopback", "--passphrase", "drill-pass",
		"--symmetric", "--cipher-algo", "AES256", "-o", enc, plain)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gpg could not encrypt in this environment: %v (%s)", err, out)
	}

	t.Setenv("GNUPGHOME", dir)
	t.Setenv("FIREDRILL_TEST_GPG_PASS", "drill-pass")
	b, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: enc,
		Decrypt: &spec.Decrypt{Type: "gpg", PassphraseEnv: "FIREDRILL_TEST_GPG_PASS"},
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = b.Cleanup() }()

	got, err := os.ReadFile(b.Path) // #nosec G304 -- test temp dir
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("plaintext = %q (%v), want %q", got, err, payload)
	}
	if b.Encryption != GPG {
		t.Errorf("encryption = %q, want gpg", b.Encryption)
	}
}

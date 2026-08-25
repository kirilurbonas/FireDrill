package source

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Encryption names a supported at-rest encryption for backup artifacts.
// Backups are routinely encrypted before they are shipped anywhere —
// `pg_dump | gzip | age -r …` — and a recovery drill that cannot read them
// is a drill of the wrong pipeline.
const (
	Age = "age"
	GPG = "gpg"
)

// encryptionMagics identify an encrypted artifact so a missing `decrypt:`
// block produces an actionable error instead of a restore tool choking on
// ciphertext.
var encryptionMagics = []struct {
	name  string
	magic []byte
}{
	{Age, []byte("age-encryption.org/v1")},
	{Age, []byte(armor.Header)},
	{GPG, []byte("-----BEGIN PGP MESSAGE-----")},
}

// pgpPacketTags are the first bytes of a binary OpenPGP message: a
// public-key- or symmetric-key-encrypted session key packet, old or new
// format.
var pgpPacketTags = []byte{0x84, 0x85, 0x8c, 0xc1, 0xc3}

// detectEncryption reports the encryption a stream's leading bytes identify,
// or "" when the artifact is not encrypted.
func detectEncryption(head []byte) string {
	for _, m := range encryptionMagics {
		if len(head) >= len(m.magic) && bytes.Equal(head[:len(m.magic)], m.magic) {
			return m.name
		}
	}
	if len(head) > 0 && bytes.IndexByte(pgpPacketTags, head[0]) >= 0 {
		return GPG
	}
	return ""
}

// maxEncryptionMagic is how many bytes detectEncryption needs — derived from
// the table so adding a longer signature cannot silently stop matching.
var maxEncryptionMagic = func() int {
	n := 1
	for _, m := range encryptionMagics {
		if len(m.magic) > n {
			n = len(m.magic)
		}
	}
	return n
}()

// decrypt wraps r in a decrypting reader per cfg. When cfg is nil it only
// checks that the artifact is not encrypted, so a spec that forgot its
// decrypt block fails with an explanation rather than a corrupt restore.
//
// The returned closer releases decryption resources; it never closes r.
func decrypt(ctx context.Context, r io.Reader, cfg *spec.Decrypt) (io.Reader, string, func(), error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, err := br.Peek(maxEncryptionMagic)
	if err != nil && len(head) == 0 && err != io.EOF {
		return nil, "", nil, err
	}
	detected := detectEncryption(head)

	if cfg == nil {
		if detected != "" {
			return nil, "", nil, fmt.Errorf(
				"backup looks %s-encrypted — add source.from.decrypt to the spec so FireDrill can read it", detected)
		}
		return br, "", func() {}, nil
	}

	switch cfg.Type {
	case Age:
		rd, err := decryptAge(br, head, cfg)
		if err != nil {
			return nil, "", nil, err
		}
		return rd, Age, func() {}, nil
	case GPG:
		return decryptGPG(ctx, br, cfg)
	default:
		return nil, "", nil, fmt.Errorf("unsupported decrypt.type %q", cfg.Type)
	}
}

// decryptAge decrypts an age artifact, binary or ASCII-armored.
func decryptAge(r io.Reader, head []byte, cfg *spec.Decrypt) (io.Reader, error) {
	ids, err := ageIdentities(cfg)
	if err != nil {
		return nil, err
	}
	in := r
	if bytes.HasPrefix(head, []byte(armor.Header)) {
		in = armor.NewReader(r)
	}
	out, err := age.Decrypt(in, ids...)
	if err != nil {
		return nil, fmt.Errorf("decrypting age backup (wrong identity?): %w", err)
	}
	return out, nil
}

// ageIdentities assembles the identities to try, from a key file, an
// environment variable, or a passphrase. Key material is referenced, never
// inlined in the spec.
func ageIdentities(cfg *spec.Decrypt) ([]age.Identity, error) {
	var ids []age.Identity
	if cfg.IdentityFile != "" {
		f, err := os.Open(cfg.IdentityFile) // #nosec G304 -- operator-designated key path
		if err != nil {
			return nil, fmt.Errorf("age identity file: %w", err)
		}
		defer func() { _ = f.Close() }()
		parsed, err := age.ParseIdentities(f)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", cfg.IdentityFile, err)
		}
		ids = append(ids, parsed...)
	}
	if cfg.IdentityEnv != "" {
		v := os.Getenv(cfg.IdentityEnv)
		if v == "" {
			return nil, fmt.Errorf("age identity: env var %s is empty or unset", cfg.IdentityEnv)
		}
		parsed, err := age.ParseIdentities(strings.NewReader(v))
		if err != nil {
			return nil, fmt.Errorf("parsing identity from %s: %w", cfg.IdentityEnv, err)
		}
		ids = append(ids, parsed...)
	}
	if cfg.PassphraseEnv != "" {
		v := os.Getenv(cfg.PassphraseEnv)
		if v == "" {
			return nil, fmt.Errorf("age passphrase: env var %s is empty or unset", cfg.PassphraseEnv)
		}
		id, err := age.NewScryptIdentity(v)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("decrypt.type age needs identityFile, identityEnv or passphraseEnv")
	}
	return ids, nil
}

// decryptGPG streams the artifact through the host's gpg. GPG deployments
// carry their own keyrings, agents and smartcards, so the local gpg is the
// only thing that can reliably open these; a passphrase, when needed, is
// passed on a dedicated file descriptor rather than argv.
func decryptGPG(ctx context.Context, r io.Reader, cfg *spec.Decrypt) (io.Reader, string, func(), error) {
	bin, err := exec.LookPath("gpg")
	if err != nil {
		return nil, "", nil, fmt.Errorf("decrypt.type gpg needs the gpg binary on PATH: %w", err)
	}
	args := []string{"--batch", "--yes", "--quiet", "--decrypt"}

	var extra []*os.File
	if cfg.PassphraseEnv != "" {
		pass := os.Getenv(cfg.PassphraseEnv)
		if pass == "" {
			return nil, "", nil, fmt.Errorf("gpg passphrase: env var %s is empty or unset", cfg.PassphraseEnv)
		}
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, "", nil, err
		}
		go func() {
			_, _ = io.WriteString(pw, pass)
			_ = pw.Close()
		}()
		// Fd 3 in the child: the first entry of ExtraFiles.
		extra = append(extra, pr)
		args = append(args, "--pinentry-mode", "loopback", "--passphrase-fd", "3")
	}

	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- fixed args, path resolved from PATH
	cmd.Stdin = r
	cmd.ExtraFiles = extra
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", nil, fmt.Errorf("starting gpg: %w", err)
	}

	done := func() {
		_ = out.Close()
		_ = cmd.Wait()
		for _, f := range extra {
			_ = f.Close()
		}
	}
	return &gpgReader{out: out, cmd: cmd, errBuf: &errBuf}, GPG, done, nil
}

// gpgReader surfaces gpg's exit status as a read error: a decryption that
// fails midway must not look like a short but valid backup.
type gpgReader struct {
	out    io.ReadCloser
	cmd    *exec.Cmd
	errBuf *strings.Builder
	waited bool
}

func (g *gpgReader) Read(p []byte) (int, error) {
	n, err := g.out.Read(p)
	if err == io.EOF && !g.waited {
		g.waited = true
		if werr := g.cmd.Wait(); werr != nil {
			return n, fmt.Errorf("gpg decryption failed: %s", strings.TrimSpace(g.errBuf.String()))
		}
	}
	return n, err
}

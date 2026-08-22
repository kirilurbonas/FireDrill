package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

func TestParseS3URI(t *testing.T) {
	b, k, err := parseS3URI("s3://acme-backups/payments/latest.dump", false)
	if err != nil || b != "acme-backups" || k != "payments/latest.dump" {
		t.Fatalf("got %q %q %v", b, k, err)
	}
	for _, bad := range []string{"http://x/y", "s3://", "s3://bucket", "s3://bucket/"} {
		if _, _, err := parseS3URI(bad, false); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	// Discovery treats the key as a prefix, which may be empty or absent.
	for _, ok := range []string{"s3://bucket", "s3://bucket/", "s3://bucket/pg/"} {
		if _, _, err := parseS3URI(ok, true); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
}

// writeAt writes a file with a specific mod time, so "newest wins" is tested
// deterministically rather than by sleeping.
func writeAt(t *testing.T, dir, name string, data []byte, mod time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFetchFileSelectLatest(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-72 * time.Hour)
	writeAt(t, dir, "payments-2026-08-20.dump", []byte("old"), base)
	want := writeAt(t, dir, "payments-2026-08-22.dump", []byte("new"), base.Add(48*time.Hour))
	writeAt(t, dir, "orders-2026-08-23.dump", []byte("other"), base.Add(60*time.Hour))
	writeAt(t, dir, "payments-empty.dump", nil, base.Add(71*time.Hour))
	if err := os.Mkdir(filepath.Join(dir, "payments-archive.dump"), 0o750); err != nil {
		t.Fatal(err)
	}

	// The glob must exclude the other drill's backups; the empty file and the
	// directory are not candidates even though they are newer.
	b, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: dir, Select: "latest", Match: "payments-*.dump",
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if b.Path != want || b.ResolvedURI != want {
		t.Errorf("selected %q (resolved %q), want %q", b.Path, b.ResolvedURI, want)
	}

	// Without a glob the newest file overall wins.
	b, err = Fetch(context.Background(), spec.From{Type: "file", URI: dir, Select: "latest"})
	if err != nil || filepath.Base(b.ResolvedURI) != "orders-2026-08-23.dump" {
		t.Errorf("got %q, %v; want orders-2026-08-23.dump", b.ResolvedURI, err)
	}
}

func TestFetchFileSelectNoMatch(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, dir, "payments.dump", []byte("x"), time.Now())
	_, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: dir, Select: "latest", Match: "ledger-*.dump",
	})
	// A drill must fail loudly rather than silently restoring the wrong file.
	if err == nil || !strings.Contains(err.Error(), "ledger-*.dump") {
		t.Fatalf("expected a no-match error naming the glob, got %v", err)
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchFileDecompresses(t *testing.T) {
	payload := []byte("PGDMP fake dump payload\n")
	mod := time.Now().Add(-3 * time.Hour).Truncate(time.Second)

	cases := []struct {
		name, file string
		data       []byte
		wantComp   string
	}{
		{"gzip", "payments.dump.gz", gzipBytes(t, payload), Gzip},
		{"zstd", "payments.dump.zst", zstdBytes(t, payload), Zstd},
		{"plain", "payments.dump", payload, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := writeAt(t, dir, c.file, c.data, mod)

			b, err := Fetch(context.Background(), spec.From{Type: "file", URI: src})
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			defer func() { _ = b.Cleanup() }()

			got, err := os.ReadFile(b.Path)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("restored payload = %q (%v), want %q", got, err, payload)
			}
			if b.Compression != c.wantComp {
				t.Errorf("compression = %q, want %q", b.Compression, c.wantComp)
			}
			// RPO must reflect when the backup was taken, not when we expanded it.
			if !b.ModTime.Equal(mod) {
				t.Errorf("modTime = %s, want %s", b.ModTime, mod)
			}
			if b.Size != int64(len(c.data)) {
				t.Errorf("size = %d, want %d (bytes as stored)", b.Size, len(c.data))
			}
			if c.wantComp == "" {
				if b.Path != src {
					t.Errorf("plain artifacts should restore in place, got %q", b.Path)
				}
			} else if b.UncompressedBytes != int64(len(payload)) {
				t.Errorf("uncompressedBytes = %d, want %d", b.UncompressedBytes, len(payload))
			}
		})
	}
}

func TestFetchFileRejectsBomb(t *testing.T) {
	dir := t.TempDir()
	// 2 MiB of zeros compresses to a couple of KiB — over a 100x expansion.
	data := gzipBytes(t, bytes.Repeat([]byte{0}, 2<<20))
	src := writeAt(t, dir, "bomb.dump.gz", data, time.Now())

	_, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: src, MaxBytes: int64(len(data)),
	})
	if err == nil || !strings.Contains(err.Error(), "decompressed backup exceeds") {
		t.Fatalf("expected the decompression guard to trip, got %v", err)
	}

	// An explicit allowance lets the same artifact through.
	b, err := Fetch(context.Background(), spec.From{
		Type: "file", URI: src, MaxBytes: int64(len(data)), MaxUncompressedBytes: 4 << 20,
	})
	if err != nil {
		t.Fatalf("with maxUncompressedBytes: %v", err)
	}
	_ = b.Cleanup()
}

func TestFetchFileRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	src := writeAt(t, dir, "big.dump", bytes.Repeat([]byte("x"), 4096), time.Now())
	if _, err := Fetch(context.Background(), spec.From{Type: "file", URI: src, MaxBytes: 1024}); err == nil {
		t.Fatal("expected maxBytes to reject the artifact")
	}
}

func TestUncompressedLimit(t *testing.T) {
	cases := []struct{ maxBytes, maxUncomp, want int64 }{
		{0, 0, 0}, {100, 0, 10000}, {100, 50, 50}, {0, 4096, 4096},
	}
	for _, c := range cases {
		if got := uncompressedLimit(c.maxBytes, c.maxUncomp); got != c.want {
			t.Errorf("uncompressedLimit(%d, %d) = %d, want %d", c.maxBytes, c.maxUncomp, got, c.want)
		}
	}
}

func TestPlainExt(t *testing.T) {
	cases := map[string]string{
		"payments.dump.gz": ".dump", "a/b/base.tar.zst": ".tar",
		"dump.bz2": "", "payments.dump": ".dump", "noext": "",
	}
	for in, want := range cases {
		if got := plainExt(in); got != want {
			t.Errorf("plainExt(%q) = %q, want %q", in, got, want)
		}
	}
}

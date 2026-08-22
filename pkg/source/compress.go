package source

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Compression names a supported backup-artifact compression, as detected
// from the stream's magic bytes. "" means the artifact is not compressed.
//
// Backup pipelines almost always compress (`pg_dump | gzip`,
// `mongodump --archive --gzip`, `pg_basebackup -Ft -z`), so FireDrill
// decompresses transparently instead of making users pre-expand artifacts.
const (
	Gzip  = "gzip"
	Zstd  = "zstd"
	Bzip2 = "bzip2"
)

// magics maps a compression to its file signature. Longest match wins, so
// the table is checked in order of specificity by detect().
var magics = []struct {
	name  string
	magic []byte
}{
	{Zstd, []byte{0x28, 0xb5, 0x2f, 0xfd}},
	{Gzip, []byte{0x1f, 0x8b}},
	{Bzip2, []byte("BZh")},
}

// maxMagic is how many bytes detect() needs to peek.
const maxMagic = 4

// decompress sniffs r's leading bytes and, when they identify a supported
// compression, returns a reader over the expanded stream. The returned
// reader is always positioned at the start of the (logical) artifact — a
// plain artifact is passed through unchanged and costs nothing.
//
// The returned closer releases decoder resources; it never closes r.
func decompress(r io.Reader) (io.Reader, string, func(), error) {
	br := bufio.NewReaderSize(r, 64<<10)
	// Peek is best-effort: an artifact shorter than the magic window is
	// simply not compressed.
	head, err := br.Peek(maxMagic)
	if err != nil && len(head) == 0 {
		if err == io.EOF {
			return br, "", func() {}, nil
		}
		return nil, "", nil, err
	}

	switch detect(head) {
	case Gzip:
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, "", nil, fmt.Errorf("gzip: %w", err)
		}
		return zr, Gzip, func() { _ = zr.Close() }, nil
	case Zstd:
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, "", nil, fmt.Errorf("zstd: %w", err)
		}
		return zr.IOReadCloser(), Zstd, zr.Close, nil
	case Bzip2:
		return bzip2.NewReader(br), Bzip2, func() {}, nil
	}
	return br, "", func() {}, nil
}

func detect(head []byte) string {
	for _, m := range magics {
		if len(head) >= len(m.magic) && bytes.Equal(head[:len(m.magic)], m.magic) {
			return m.name
		}
	}
	return ""
}

// uncompressedLimit derives the decompression-bomb guard from the spec's
// transfer cap: a backup that expands more than 100x is either not a backup
// or not one this runner has disk for. 0 means no cap was configured.
func uncompressedLimit(maxBytes, maxUncompressed int64) int64 {
	if maxUncompressed > 0 {
		return maxUncompressed
	}
	if maxBytes > 0 {
		return maxBytes * 100
	}
	return 0
}

// copyLimited copies src to dst, refusing to write more than limit bytes
// (limit 0 = unlimited). It reports the number of bytes written.
func copyLimited(dst io.Writer, src io.Reader, limit int64, what string) (int64, error) {
	if limit <= 0 {
		return io.Copy(dst, src)
	}
	// +1 so an at-limit stream is distinguishable from an over-limit one.
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("%s exceeds %d bytes", what, limit)
	}
	return n, nil
}

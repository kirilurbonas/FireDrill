// Package source fetches backup artifacts from their storage location so a
// driver can restore them. Sources are read-only by design: FireDrill only
// ever downloads.
package source

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// maxListKeys bounds prefix discovery: a misconfigured prefix pointing at a
// bucket with millions of objects must fail loudly, not page forever.
const maxListKeys = 10000

// Backup is a locally available backup artifact.
type Backup struct {
	Path    string    // local filesystem path to the artifact (decompressed)
	ModTime time.Time // when the backup was produced (drives freshness/RPO)
	Size    int64     // bytes of the stored artifact, as fetched
	// ResolvedURI is the artifact actually selected. It differs from the
	// spec URI when discovery picked one object out of a prefix — evidence
	// must record which backup was drilled, not which pattern matched it.
	ResolvedURI string
	// Encryption is "" for a plaintext artifact, else age/gpg.
	Encryption string
	// Compression is "" for a plain artifact, else gzip/zstd/bzip2.
	Compression string
	// UncompressedBytes is the expanded size; 0 when not compressed.
	UncompressedBytes int64

	cleanup func() error
}

// Layers describes what FireDrill had to undo to read the artifact, e.g.
// "age, gzip" — shown on the restore line so an operator sees the pipeline
// their backup actually came through.
func (b *Backup) Layers() string {
	var parts []string
	if b.Encryption != "" {
		parts = append(parts, b.Encryption)
	}
	if b.Compression != "" {
		parts = append(parts, b.Compression)
	}
	return strings.Join(parts, ", ")
}

// Cleanup removes any temporary download. Safe on nil / no-op sources.
func (b *Backup) Cleanup() error {
	if b == nil || b.cleanup == nil {
		return nil
	}
	return b.cleanup()
}

// Fetch resolves a spec source to a local file, selecting the newest
// matching object when discovery is configured and transparently
// decompressing gzip/zstd/bzip2 artifacts.
func Fetch(ctx context.Context, from spec.From) (*Backup, error) {
	switch from.Type {
	case "file":
		return fetchFile(ctx, from)
	case "s3":
		return fetchS3(ctx, from)
	default:
		return nil, fmt.Errorf("unsupported source type %q", from.Type)
	}
}

func fetchFile(ctx context.Context, from spec.From) (*Backup, error) {
	p := from.URI
	if from.Select != "" {
		var err error
		if p, err = latestFile(from); err != nil {
			return nil, err
		}
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("backup file: %w", err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("backup path %s is a directory (use select: latest to pick from it)", p)
	}
	if from.MaxBytes > 0 && fi.Size() > from.MaxBytes {
		return nil, fmt.Errorf("backup is %d bytes, exceeding maxBytes %d", fi.Size(), from.MaxBytes)
	}

	f, err := os.Open(p) // #nosec G304 -- user-supplied backup path is the CLI's input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// Decrypt first: pipelines encrypt the compressed artifact, so the
	// ciphertext is the outer layer.
	plain, enc, closeDecrypt, err := decrypt(ctx, f, from.Decrypt)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	defer closeDecrypt()

	r, comp, closeDec, err := decompress(plain)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	defer closeDec()

	b := &Backup{Path: p, ModTime: fi.ModTime(), Size: fi.Size(), ResolvedURI: p,
		Encryption: enc, Compression: comp}
	if comp == "" && enc == "" {
		return b, nil // plain artifact: restore straight from the source file
	}

	// Encrypted or compressed: materialize the plaintext in a private temp
	// file (0600, removed on cleanup). The backup's own mod time is
	// preserved — RPO measures when the backup was taken, not when we
	// unpacked it.
	tmp, n, err := spool(r, "firedrill-backup-*"+plainExt(p),
		uncompressedLimit(from.MaxBytes, from.MaxUncompressedBytes))
	if err != nil {
		return nil, err
	}
	b.Path = tmp
	if comp != "" {
		b.UncompressedBytes = n
	}
	b.cleanup = func() error { return os.Remove(tmp) }
	return b, nil
}

// latestFile picks the newest file in the URI directory matching from.Match.
func latestFile(from spec.From) (string, error) {
	entries, err := os.ReadDir(from.URI)
	if err != nil {
		return "", fmt.Errorf("listing backup directory: %w", err)
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !globMatch(from.Match, e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() == 0 {
			continue
		}
		if newest == "" || fi.ModTime().After(newestMod) {
			newest, newestMod = filepath.Join(from.URI, e.Name()), fi.ModTime()
		}
	}
	if newest == "" {
		return "", noMatchErr(from.URI, from.Match)
	}
	return newest, nil
}

func fetchS3(ctx context.Context, from spec.From) (*Backup, error) {
	bucket, key, err := parseS3URI(from.URI, from.Select != "")
	if err != nil {
		return nil, err
	}
	cli, err := s3Client(ctx, from)
	if err != nil {
		return nil, err
	}
	if from.Select != "" {
		if key, err = latestObject(ctx, cli, bucket, key, from.Match); err != nil {
			return nil, err
		}
	}
	resolved := "s3://" + bucket + "/" + key

	obj, err := cli.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", resolved, err)
	}
	defer func() { _ = obj.Body.Close() }()

	if from.MaxBytes > 0 && obj.ContentLength != nil && *obj.ContentLength > from.MaxBytes {
		return nil, fmt.Errorf("backup is %d bytes, exceeding maxBytes %d", *obj.ContentLength, from.MaxBytes)
	}

	// Cap the transferred stream, then decompress on the fly: a compressed
	// artifact is expanded while it downloads, never staged twice on disk.
	transferred := &capReader{r: obj.Body, limit: from.MaxBytes, what: "backup"}
	plain, enc, closeDecrypt, err := decrypt(ctx, transferred, from.Decrypt)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", resolved, err)
	}
	defer closeDecrypt()

	r, comp, closeDec, err := decompress(plain)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", resolved, err)
	}
	defer closeDec()

	name, n, err := spool(r, "firedrill-backup-*"+plainExt(key),
		uncompressedLimit(from.MaxBytes, from.MaxUncompressedBytes))
	if err != nil {
		return nil, fmt.Errorf("downloading backup: %w", err)
	}

	modTime := time.Now()
	if obj.LastModified != nil {
		modTime = *obj.LastModified
	}
	b := &Backup{
		Path:        name,
		ModTime:     modTime,
		Size:        transferred.n,
		ResolvedURI: resolved,
		Encryption:  enc,
		Compression: comp,
		cleanup:     func() error { return os.Remove(name) },
	}
	if comp != "" {
		b.UncompressedBytes = n
	}
	return b, nil
}

func s3Client(ctx context.Context, from spec.From) (*s3.Client, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if from.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(from.Region))
	}
	// credentialsRef maps to a shared-config profile; the default AWS
	// credential chain applies otherwise. Secrets never enter the spec.
	if from.CredentialsRef != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(from.CredentialsRef))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if from.Endpoint != "" {
			// S3-compatible stores (MinIO, Ceph, …): custom endpoint with
			// path-style addressing (virtual-hosted style needs DNS per bucket).
			o.BaseEndpoint = &from.Endpoint
			o.UsePathStyle = true
		}
	}), nil
}

// latestObject returns the key of the newest object under prefix matching
// the glob. Backup pipelines write timestamped keys, so "newest wins" is the
// selection every drill actually wants.
func latestObject(ctx context.Context, cli *s3.Client, bucket, prefix, match string) (string, error) {
	var newest s3types.Object
	seen := 0
	pager := s3.NewListObjectsV2Paginator(cli, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("listing s3://%s/%s: %w", bucket, prefix, err)
		}
		for _, o := range page.Contents {
			seen++
			if seen > maxListKeys {
				return "", fmt.Errorf("more than %d objects under s3://%s/%s — narrow the prefix or add a match glob",
					maxListKeys, bucket, prefix)
			}
			k := aws.ToString(o.Key)
			if strings.HasSuffix(k, "/") || aws.ToInt64(o.Size) == 0 {
				continue // folder placeholder or empty object
			}
			if !globMatch(match, path.Base(k)) {
				continue
			}
			if newest.Key == nil || o.LastModified.After(*newest.LastModified) {
				newest = o
			}
		}
	}
	if newest.Key == nil {
		return "", noMatchErr("s3://"+bucket+"/"+prefix, match)
	}
	return aws.ToString(newest.Key), nil
}

func noMatchErr(location, match string) error {
	if match != "" {
		return fmt.Errorf("no backup matching %q found under %s", match, location)
	}
	return fmt.Errorf("no backup found under %s", location)
}

// globMatch reports whether name satisfies the glob; an empty glob matches
// everything. Malformed patterns are rejected at spec-validation time.
func globMatch(glob, name string) bool {
	if glob == "" {
		return true
	}
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

// spool writes r to a temp file, enforcing limit (0 = unlimited), and
// returns the path and the number of bytes written.
func spool(r io.Reader, pattern string, limit int64) (string, int64, error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", 0, err
	}
	n, err := copyLimited(tmp, r, limit, "decompressed backup")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", 0, err
	}
	return tmp.Name(), n, nil
}

// plainExt strips a compression suffix so the temp file keeps a meaningful
// extension (payments.dump.gz → .dump).
func plainExt(name string) string {
	base := path.Base(filepath.ToSlash(name))
	switch ext := path.Ext(base); ext {
	case ".gz", ".zst", ".zstd", ".bz2":
		return path.Ext(strings.TrimSuffix(base, ext))
	default:
		return ext
	}
}

// capReader counts bytes read and fails past limit (0 = unlimited).
type capReader struct {
	r     io.Reader
	n     int64
	limit int64
	what  string
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.limit > 0 && c.n > c.limit {
		return n, fmt.Errorf("%s exceeds maxBytes %d", c.what, c.limit)
	}
	return n, err
}

// parseS3URI splits s3://bucket/key. With allowEmptyKey the key part is a
// prefix and may be empty (the whole bucket).
func parseS3URI(uri string, allowEmptyKey bool) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		return "", "", fmt.Errorf("s3 uri must start with s3://, got %q", uri)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if bucket == "" || (!allowEmptyKey && (!ok || key == "")) {
		return "", "", fmt.Errorf("invalid s3 uri %q (want s3://bucket/key)", uri)
	}
	return bucket, key, nil
}

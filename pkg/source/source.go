// Package source fetches backup artifacts from their storage location so a
// driver can restore them. Sources are read-only by design: FireDrill only
// ever downloads.
package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/kirilurbonas/FireDrill/pkg/spec"
)

// Backup is a locally available backup artifact.
type Backup struct {
	Path    string    // local filesystem path to the artifact
	ModTime time.Time // when the backup was produced (drives freshness/RPO)
	Size    int64
	cleanup func() error
}

// Cleanup removes any temporary download. Safe on nil / no-op sources.
func (b *Backup) Cleanup() error {
	if b == nil || b.cleanup == nil {
		return nil
	}
	return b.cleanup()
}

// Fetch resolves a spec source to a local file.
func Fetch(ctx context.Context, from spec.From) (*Backup, error) {
	switch from.Type {
	case "file":
		return fetchFile(from.URI)
	case "s3":
		return fetchS3(ctx, from)
	default:
		return nil, fmt.Errorf("unsupported source type %q", from.Type)
	}
}

func fetchFile(path string) (*Backup, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("backup file: %w", err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("backup path %s is a directory", path)
	}
	return &Backup{Path: path, ModTime: fi.ModTime(), Size: fi.Size()}, nil
}

func fetchS3(ctx context.Context, from spec.From) (*Backup, error) {
	bucket, key, err := parseS3URI(from.URI)
	if err != nil {
		return nil, err
	}

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
	cli := s3.NewFromConfig(cfg)

	obj, err := cli.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", from.URI, err)
	}
	defer func() { _ = obj.Body.Close() }()

	tmp, err := os.CreateTemp("", "firedrill-backup-*"+filepath.Ext(key))
	if err != nil {
		return nil, err
	}
	size, err := tmp.ReadFrom(obj.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("downloading backup: %w", err)
	}

	modTime := time.Now()
	if obj.LastModified != nil {
		modTime = *obj.LastModified
	}
	name := tmp.Name()
	return &Backup{
		Path:    name,
		ModTime: modTime,
		Size:    size,
		cleanup: func() error { return os.Remove(name) },
	}, nil
}

func parseS3URI(uri string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		return "", "", fmt.Errorf("s3 uri must start with s3://, got %q", uri)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("invalid s3 uri %q (want s3://bucket/key)", uri)
	}
	return bucket, key, nil
}

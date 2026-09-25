package backup

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// Downloader is the read side of the backup path (FU-3 Phase C): it lists and
// fetches snapshots from the same bucket the Uploader writes to, for the
// `cronomicon restore` subcommand. The uploader is otherwise put-only.
type Downloader struct {
	client *minio.Client
	bucket string
}

// Snapshot describes one stored backup object.
type Snapshot struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// NewDownloader builds a restore Downloader from config. It errors when no
// bucket is configured — restore has nothing to read without CRONOMICON_BACKUP_S3_*.
func NewDownloader(cfg *config.Config, log *slog.Logger) (*Downloader, error) {
	if cfg.BackupS3Bucket == "" {
		return nil, fmt.Errorf("no backup bucket configured — set CRONOMICON_BACKUP_S3_BUCKET (and the other CRONOMICON_BACKUP_S3_* vars)")
	}
	client, err := newClient(cfg, log)
	if err != nil {
		return nil, err
	}
	return &Downloader{client: client, bucket: cfg.BackupS3Bucket}, nil
}

// Bucket returns the configured bucket name (for operator-facing messages).
func (d *Downloader) Bucket() string { return d.bucket }

// List returns the stored .db snapshots, newest first.
func (d *Downloader) List(ctx context.Context) ([]Snapshot, error) {
	var out []Snapshot
	for obj := range d.client.ListObjects(ctx, d.bucket, minio.ListObjectsOptions{Prefix: backupPrefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", d.bucket, backupPrefix, obj.Err)
		}
		if !strings.HasSuffix(obj.Key, ".db") {
			continue
		}
		out = append(out, Snapshot{Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastModified.After(out[j].LastModified) })
	return out, nil
}

// LatestKey returns the newest snapshot's object key.
func (d *Downloader) LatestKey(ctx context.Context) (string, error) {
	snaps, err := d.List(ctx)
	if err != nil {
		return "", err
	}
	if len(snaps) == 0 {
		return "", fmt.Errorf("no snapshots found under s3://%s/%s", d.bucket, backupPrefix)
	}
	return snaps[0].Key, nil
}

// Download fetches key into localPath. The key may be a bare filename (it is
// resolved under the backup prefix) or a full prefixed key.
func (d *Downloader) Download(ctx context.Context, key, localPath string) error {
	if !strings.Contains(key, "/") {
		key = backupPrefix + key
	}
	if err := d.client.FGetObject(ctx, d.bucket, key, localPath, minio.GetObjectOptions{}); err != nil {
		return fmt.Errorf("download s3://%s/%s: %w", d.bucket, key, err)
	}
	return nil
}

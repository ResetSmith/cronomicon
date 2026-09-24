// Package backup uploads nightly database snapshots to S3-compatible storage
// (A4). It supports AWS S3 and any S3-compatible endpoint (MinIO, Ceph, R2) via
// the configurable endpoint. Credentials come from the environment (config), so
// the retention worker in package db stays free of secret-decryption and avoids
// a db↔settings import cycle — main wires the uploader in as a callback.
package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
)

// Uploader puts a local snapshot file into the configured bucket.
type Uploader func(ctx context.Context, localPath string) error

// backupPrefix is the object-key prefix all snapshots live under. Shared by the
// uploader and the restore Downloader so both agree on where snapshots are.
const backupPrefix = "amadeus-backups/"

// newClient builds the SSRF-guarded minio client shared by the uploader and the
// restore Downloader. Credentials default to AWS S3 when no endpoint is given;
// an explicit endpoint targets any S3-compatible service.
//
// Since SL-1 the construction itself lives in internal/logarchive.NewClient —
// one MinIO construction site for backups and the run-log archive, so the SU-7
// egress guard and the IMDS carve-out are applied in exactly one place. This
// wrapper only maps env config onto it.
func newClient(cfg *config.Config, log *slog.Logger) (*minio.Client, error) {
	// SU-7: guard bucket egress (SSRF) against the operator-configured S3
	// endpoint. Sourced from config; loopback stays blocked unless explicitly
	// re-permitted. The IAM/IMDS credential probe is a deliberate carve-out and
	// is NOT guarded (see logarchive.ResolveCreds).
	policy := httpx.EgressPolicy{
		AllowPrivate:  cfg.OutboundAllowPrivate,
		AllowLoopback: cfg.OutboundAllowLoopback,
	}
	caPEM := ""
	if cfg.BackupS3CAFile != "" {
		b, rerr := os.ReadFile(cfg.BackupS3CAFile)
		if rerr != nil {
			return nil, fmt.Errorf("read AMADEUS_BACKUP_S3_CA_FILE: %w", rerr)
		}
		caPEM = string(b)
	}
	client, credMode, err := logarchive.NewClient(logarchive.ClientParams{
		Endpoint:  cfg.BackupS3Endpoint,
		Region:    cfg.BackupS3Region,
		AccessKey: cfg.BackupS3AccessKey,
		SecretKey: cfg.BackupS3SecretKey,
		UseSSL:    cfg.BackupS3UseSSL,
		CAPEM:     caPEM,
	}, policy)
	if err != nil {
		return nil, err
	}
	log.Info("s3 backup client initialized", "creds", credMode, "bucket", cfg.BackupS3Bucket, "endpoint", client.EndpointURL().Host)
	return client, nil
}

// New builds an Uploader from config. enabled is false when no bucket is set, in
// which case the retention worker simply skips the upload step (graceful — A4
// backup is optional, T12).
func New(cfg *config.Config, log *slog.Logger) (up Uploader, enabled bool, err error) {
	if cfg.BackupS3Bucket == "" {
		return nil, false, nil
	}

	client, err := newClient(cfg, log)
	if err != nil {
		return nil, false, err
	}

	bucket := cfg.BackupS3Bucket
	up = func(ctx context.Context, localPath string) error {
		key := backupPrefix + filepath.Base(localPath)
		// Retry with exponential backoff (PP-M4): transient network errors should
		// not silently fail the nightly backup. Three attempts: immediate, 30s, 2m.
		delays := []time.Duration{0, 30 * time.Second, 2 * time.Minute}
		var lastErr error
		for i, delay := range delays {
			if delay > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
				}
			}
			info, err := client.FPutObject(ctx, bucket, key, localPath, minio.PutObjectOptions{
				ContentType: "application/x-sqlite3",
			})
			if err == nil {
				log.Info("backup uploaded to S3", "bucket", bucket, "key", key, "bytes", info.Size)
				return nil
			}
			lastErr = err
			log.Warn("backup upload attempt failed", "attempt", i+1, "of", len(delays), "error", err)
		}
		return fmt.Errorf("upload %s to s3://%s/%s after %d attempts: %w", localPath, bucket, key, len(delays), lastErr)
	}
	return up, true, nil
}

// resolveS3Creds picks static V4 credentials when both keys are configured,
// otherwise falls back to the ambient AWS credential chain (V1.1-11). Delegates
// to logarchive.ResolveCreds since SL-1; kept as the env-config adapter the
// tests pin.
func resolveS3Creds(cfg *config.Config) (creds *credentials.Credentials, mode string) {
	return logarchive.ResolveCreds(cfg.BackupS3AccessKey, cfg.BackupS3SecretKey)
}

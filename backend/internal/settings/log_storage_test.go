package settings

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/logarchive/fakes3"
)

// testKEK is the 32-byte base64 KEK the api suite uses; the S3 secret key is
// envelope-encrypted on write and decrypted for the save-time probe.
const testKEK = "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="

func logStorageCfg() *config.Config {
	// The fake S3 runs on loopback, which the SU-7 guard blocks by default.
	return &config.Config{SecretKEKEnv: testKEK, OutboundAllowPrivate: true, OutboundAllowLoopback: true}
}

func boolp(b bool) *bool { return &b }

func s3Input(fake *fakes3.Server, bucket string) LogStorageConfig {
	return LogStorageConfig{
		Backend: "s3",
		Local:   &LocalLogConfig{Path: "/var/lib/amadeus/logs"},
		S3: &S3LogConfig{
			Endpoint: fake.Endpoint(), Bucket: bucket, Region: "us-east-1",
			AccessKey: "AKIAFAKE", SecretKey: "fakesecret", Prefix: "/amadeus/", UseSSL: boolp(false),
		},
	}
}

func wantValidation(t *testing.T, err error, field, contains string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError on %s, got %v", field, err)
	}
	if ve.Field != field {
		t.Fatalf("ValidationError field = %q, want %q (%s)", ve.Field, field, ve.Message)
	}
	if contains != "" && !strings.Contains(ve.Message, contains) {
		t.Fatalf("ValidationError message %q does not contain %q", ve.Message, contains)
	}
}

// TestLogStorageDefaultsAndLocalRoundTrip pins the pre-SL-1 behaviour: an
// unset row reads as local with the default sync, and a local save writes only
// the local half.
func TestLogStorageDefaultsAndLocalRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := logStorageCfg()

	got, err := GetLogStorageConfig(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != "local" || got.S3 != nil || got.Archive != nil {
		t.Fatalf("fresh row: %+v", got)
	}
	if got.Sync == nil || got.Sync.Mode != "interval" || got.Sync.IntervalSeconds != 900 {
		t.Fatalf("fresh sync defaults: %+v", got.Sync)
	}

	got, err = UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{Backend: "local", Local: &LocalLogConfig{Path: "/srv/logs"}}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if got.Local.Path != "/srv/logs" || got.Backend != "local" || got.Archive != nil {
		t.Fatalf("local save: %+v", got)
	}
	if ResolveLogDir(ctx, pool) != "/srv/logs" {
		t.Fatal("ResolveLogDir did not follow the save")
	}
	if store, err := BuildLogArchive(ctx, pool, cfg, nil); err != nil || store != nil {
		t.Fatalf("BuildLogArchive on local = (%v, %v), want (nil, nil)", store, err)
	}
}

// TestLogStorageS3SaveProbesAndPersists is the SL-1 core: the 422 gate is gone,
// a reachable bucket saves, the secret never comes back, and BuildLogArchive
// yields a working store.
func TestLogStorageS3SaveProbesAndPersists(t *testing.T) {
	fake := fakes3.New("logs")
	defer fake.Close()
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := logStorageCfg()

	in := s3Input(fake, "logs")
	in.Sync = &LogSyncConfig{Mode: "interval", IntervalSeconds: 300}
	got, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "tester")
	if err != nil {
		t.Fatalf("s3 save: %v", err)
	}
	if got.Backend != "s3" || got.S3 == nil || got.S3.Bucket != "logs" {
		t.Fatalf("s3 save result: %+v", got)
	}
	if got.S3.SecretKey != "" {
		t.Fatal("secret key must never be returned")
	}
	if got.S3.Prefix != "amadeus/" {
		t.Fatalf("prefix not normalised: %q", got.S3.Prefix)
	}
	if got.S3.UseSSL == nil || *got.S3.UseSSL {
		t.Fatalf("useSsl = %v, want false", got.S3.UseSSL)
	}
	if got.Sync.IntervalSeconds != 300 || got.Sync.Mode != "interval" {
		t.Fatalf("sync: %+v", got.Sync)
	}
	if got.Archive == nil || got.Archive.Count != 0 || got.Archive.Pending != 0 || got.Archive.InProgress {
		t.Fatalf("archive status: %+v", got.Archive)
	}
	// The probe hit the fake.
	var probed bool
	for _, r := range fake.Requests {
		if strings.HasPrefix(r, "HEAD /logs") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("save did not probe the bucket; requests: %v", fake.Requests)
	}

	store, err := BuildLogArchive(ctx, pool, cfg, nil)
	if err != nil || store == nil {
		t.Fatalf("BuildLogArchive = (%v, %v)", store, err)
	}
	if store.Bucket() != "logs" || store.Prefix() != "amadeus/" || store.CredMode() != "static" {
		t.Fatalf("store = %s/%s creds=%s", store.Bucket(), store.Prefix(), store.CredMode())
	}
	if err := store.Probe(ctx); err != nil {
		t.Fatalf("built store cannot reach the bucket: %v", err)
	}

	// Omit-to-keep: a second save with no secret and no useSsl keeps both — the
	// probe must still pass with the stored (decrypted) secret.
	in2 := s3Input(fake, "logs")
	in2.S3.SecretKey = ""
	in2.S3.UseSSL = nil
	in2.Sync = nil
	got, err = UpdateLogStorageConfig(ctx, pool, cfg, in2, "tester")
	if err != nil {
		t.Fatalf("omit-to-keep save: %v", err)
	}
	if got.S3.UseSSL == nil || *got.S3.UseSSL {
		t.Fatal("omitted useSsl did not keep the stored false")
	}
	if got.Sync.IntervalSeconds != 300 {
		t.Fatalf("omitted sync did not keep the stored interval: %+v", got.Sync)
	}
	var enc sql.NullString
	_ = pool.QueryRowContext(ctx, `SELECT s3_secret_key_enc FROM log_storage_config WHERE id=1`).Scan(&enc)
	if !enc.Valid || enc.String == "" {
		t.Fatal("stored secret was cleared by an omit-to-keep save")
	}

	// Switching back to local stops the tier but keeps the S3 fields and reports
	// no archive (count is 0) — SL-Q14.
	got, err = UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{Backend: "local", Local: &LocalLogConfig{Path: "/var/lib/amadeus/logs"}, S3: in2.S3}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != "local" || got.S3 == nil || got.S3.Bucket != "logs" {
		t.Fatalf("back to local: %+v", got)
	}
	if store, _ := BuildLogArchive(ctx, pool, cfg, nil); store != nil {
		t.Fatal("BuildLogArchive must be nil once the backend is local")
	}
}

// TestLogStorageS3ValidationMatrix: every refusal is a 422-shaped
// ValidationError naming the field, and none of them writes the row.
func TestLogStorageS3ValidationMatrix(t *testing.T) {
	fake := fakes3.New("logs")
	defer fake.Close()
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := logStorageCfg()

	rowUnchanged := func(t *testing.T) {
		t.Helper()
		var backend sql.NullString
		err := pool.QueryRowContext(ctx, `SELECT backend FROM log_storage_config WHERE id=1`).Scan(&backend)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("a refused save wrote the row (backend=%q, err=%v)", backend.String, err)
		}
	}

	t.Run("bad backend", func(t *testing.T) {
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{Backend: "tape"}, "t")
		wantValidation(t, err, "backend", "")
		rowUnchanged(t)
	})
	t.Run("s3 without block", func(t *testing.T) {
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, LogStorageConfig{Backend: "s3"}, "t")
		wantValidation(t, err, "s3", "")
		rowUnchanged(t)
	})
	t.Run("bucket required", func(t *testing.T) {
		in := s3Input(fake, "")
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "s3.bucket", "required")
		rowUnchanged(t)
	})
	t.Run("region required without endpoint", func(t *testing.T) {
		in := s3Input(fake, "logs")
		in.S3.Endpoint, in.S3.Region = "", ""
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "s3.region", "")
		rowUnchanged(t)
	})
	t.Run("one key without the other", func(t *testing.T) {
		in := s3Input(fake, "logs")
		in.S3.SecretKey = ""
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "s3.accessKey", "together")
		rowUnchanged(t)
	})
	t.Run("missing bucket fails the probe", func(t *testing.T) {
		in := s3Input(fake, "absent")
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "s3.bucket", "does not exist")
		rowUnchanged(t)
	})
	t.Run("access denied fails the probe with the S3 code", func(t *testing.T) {
		fake.Fail("AccessDenied", 403)
		defer fake.Fail("", 0)
		in := s3Input(fake, "logs")
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "s3.bucket", "Access Denied")
		rowUnchanged(t)
	})
	t.Run("unreachable endpoint fails the probe", func(t *testing.T) {
		in := s3Input(fake, "logs")
		in.S3.Endpoint = "127.0.0.1:1" // nothing listens
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "s3.bucket", "probe failed")
		rowUnchanged(t)
	})
	t.Run("sync mode", func(t *testing.T) {
		in := s3Input(fake, "logs")
		in.Sync = &LogSyncConfig{Mode: "cron"}
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "sync.mode", "")
		rowUnchanged(t)
	})
	t.Run("daily needs HH:MM", func(t *testing.T) {
		in := s3Input(fake, "logs")
		in.Sync = &LogSyncConfig{Mode: "daily", At: "2am"}
		_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
		wantValidation(t, err, "sync.at", "HH:MM")
		rowUnchanged(t)
	})
}

// TestLogStorageSyncClampAndDaily: the interval is clamped to [60, 86400] rather
// than refused (SL-Q3), daily stores its time, and the ambient credential mode
// is accepted when both keys are blank (SL-Q5).
func TestLogStorageSyncClampAndDaily(t *testing.T) {
	fake := fakes3.New("logs")
	defer fake.Close()
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := logStorageCfg()

	in := s3Input(fake, "logs")
	in.Sync = &LogSyncConfig{Mode: "interval", IntervalSeconds: 5}
	got, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sync.IntervalSeconds != 60 {
		t.Fatalf("floor: %d", got.Sync.IntervalSeconds)
	}

	in.Sync = &LogSyncConfig{Mode: "interval", IntervalSeconds: 999999}
	got, _ = UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
	if got.Sync.IntervalSeconds != 86400 {
		t.Fatalf("ceiling: %d", got.Sync.IntervalSeconds)
	}

	in.Sync = &LogSyncConfig{Mode: "daily", At: " 02:30 "}
	got, err = UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sync.Mode != "daily" || got.Sync.At != "02:30" {
		t.Fatalf("daily: %+v", got.Sync)
	}

	// Ambient chain: both keys blank is accepted. The chain's first provider is
	// the AWS env vars; set them so the chain resolves there and never reaches
	// the IAM provider, whose IMDS probe would hang the test for its 10s timeout.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "envsecret")
	in.S3.AccessKey, in.S3.SecretKey = "", ""
	// Clear the stored secret first or omit-to-keep would pair it with an empty
	// access key and trip the both-or-neither rule.
	if _, err := pool.ExecContext(ctx, `UPDATE log_storage_config SET s3_secret_key_enc=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	got, err = UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
	if err != nil {
		t.Fatalf("ambient save: %v", err)
	}
	store, err := BuildLogArchive(ctx, pool, cfg, nil)
	if err != nil || store == nil {
		t.Fatalf("BuildLogArchive ambient = (%v, %v)", store, err)
	}
	if !strings.Contains(store.CredMode(), "ambient") {
		t.Fatalf("cred mode = %q", store.CredMode())
	}
}

// The CA bundle round-trips (a certificate is not a secret), gates the probe
// against a TLS node, and a bundle without a certificate is a 422.
func TestLogStorageS3CABundle(t *testing.T) {
	fake := fakes3.NewTLS("logs")
	defer fake.Close()
	pool := openTestPool(t)
	ctx := context.Background()
	cfg := logStorageCfg()

	in := s3Input(fake, "logs")
	in.S3.UseSSL = boolp(true)
	in.S3.CAPEM = "garbage"
	_, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
	wantValidation(t, err, "s3.caPem", "certificate")

	in.S3.CAPEM = ""
	_, err = UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
	wantValidation(t, err, "s3.bucket", "probe failed") // TLS refused without the CA

	in.S3.CAPEM = fake.CertPEM()
	got, err := UpdateLogStorageConfig(ctx, pool, cfg, in, "t")
	if err != nil {
		t.Fatalf("save with CA: %v", err)
	}
	if got.S3 == nil || !strings.Contains(got.S3.CAPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("caPem did not round-trip: %+v", got.S3)
	}
	store, err := BuildLogArchive(ctx, pool, cfg, nil)
	if err != nil || store == nil {
		t.Fatalf("BuildLogArchive = (%v, %v)", store, err)
	}
	if err := store.Probe(ctx); err != nil {
		t.Fatalf("built store should trust the node: %v", err)
	}
}

package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// defaultLogDir mirrors runner.DefaultLogDir (and the migration default).
// Duplicated as a literal so settings does not import the runner package.
const defaultLogDir = "/var/lib/cronomicon/logs"

// File names this package must recognise but does not own. Duplicated as
// literals for the same reason defaultLogDir is: importing cmd/cronomicon (which
// owns the process log) or internal/runner (which owns the sidecar) from here
// would invert the dependency direction. Kept in sync by comment, like
// defaultLogDir and runner.DefaultLogDir already are.
//
// auditLogName is claimed now although nothing writes it until the audit-stream
// phase: the classifier is correct either way, and an unrecognised file would
// otherwise be miscounted as a run log the moment that lands.
const (
	processLogName = "cronomicon.log" // cmd/cronomicon.processLogName
	auditLogName   = "audit.log"
	metaFileName   = "_meta.json" // runner.MetaFileName
)

// LogStorageConfig is the wire shape of GET/PUT /settings/log-storage.
// Field names follow the spec's LogStorageConfig schema — the SPA form reads
// these exact keys (local.path, s3.accessKey/secretKey/prefix, stats.*).
//
// Since SL-1 (the s3-logging plan) backend=s3 means "local AND an S3
// archive tier": local disk stays the only write target during a run, and the
// scheduled sync (Sync) copies sealed logs to the bucket afterwards. Archive is
// the read-only status of that tier.
type LogStorageConfig struct {
	Backend        string            `json:"backend"` // local | s3
	Local          *LocalLogConfig   `json:"local,omitempty"`
	S3             *S3LogConfig      `json:"s3,omitempty"`
	Sync           *LogSyncConfig    `json:"sync,omitempty"`
	Stats          *LogStorageStats  `json:"stats,omitempty"`   // readOnly
	Archive        *LogArchiveStatus `json:"archive,omitempty"` // readOnly
	LastModifiedBy string            `json:"lastModifiedBy,omitempty"`
	LastModifiedAt string            `json:"lastModifiedAt,omitempty"`
}

type LocalLogConfig struct {
	Path string `json:"path"`
}

type S3LogConfig struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey,omitempty"` // write-only; never returned
	Prefix    string `json:"prefix"`
	// UseSSL decides the endpoint scheme (060 had no way to say http://; MinIO
	// on a LAN commonly is). Defaults true. A pointer so an omitted field on
	// write keeps the stored value rather than silently flipping to false.
	UseSSL *bool `json:"useSsl,omitempty"`
	// CAPEM is an optional PEM CA bundle for a private S3 node with an internal
	// CA. Stored and returned (a certificate is not a secret); empty means the
	// process trust store. Validated on save to hold at least one certificate.
	CAPEM string `json:"caPem,omitempty"`
}

// LogSyncConfig is the archive timetable (SL-Q3): an interval with a 60s floor,
// or once a day at a UTC wall-clock time — the CRONOMICON_BACKUP_AT shape.
type LogSyncConfig struct {
	Mode            string `json:"mode"` // interval | daily
	IntervalSeconds int    `json:"intervalSeconds"`
	At              string `json:"at,omitempty"` // "HH:MM" UTC, daily mode
}

// LogArchiveStatus is what the Settings card reads about the archive tier.
// Count/Bytes are maintained at upload time, never by listing the bucket on
// GET (SL-Q13). Pending is the size of the sweep's own query — the one number
// that says whether the interval keeps up.
type LogArchiveStatus struct {
	Count              int64   `json:"count"`
	Bytes              int64   `json:"bytes"`
	Pending            int64   `json:"pending"`
	InProgress         bool    `json:"inProgress"`
	LastSyncStartedAt  *string `json:"lastSyncStartedAt"`
	LastSyncFinishedAt *string `json:"lastSyncFinishedAt"`
	LastSyncError      *string `json:"lastSyncError"`
}

const (
	logSyncModeInterval = "interval"
	logSyncModeDaily    = "daily"
	// logSyncMinInterval is the floor (SL-Q3): below a minute the log noise and
	// status churn outweigh any benefit — a run's log is already readable locally
	// the moment it finishes.
	logSyncMinInterval = 60
	logSyncMaxInterval = 24 * 60 * 60
	logSyncDefaultSecs = 15 * 60
	// logArchiveProbeTimeout bounds the save-time bucket probe.
	logArchiveProbeTimeout = 10 * time.Second
)

// pendingArchiveWhere is the sweep's predicate over runs (SL-1 partial index
// idx_runs_log_pending plus the terminal filter). `skipped` rows are excluded:
// they never had a process and have no file; SL-2 stamps them `missing` up
// front. The 60s grace period (SL-Q12) is the sweep's own concern and is not
// applied to the count — an operator reading "pending" wants everything not yet
// archived, not everything the next tick will take.
const pendingArchiveWhere = `log_archived_at IS NULL AND log_archive_state IS NULL
	AND status IN ('success','failure','warning','killed')`

type LogStorageStats struct {
	// TotalSizeBytes is every file under the log dir — the disk-usage answer, so
	// it deliberately spans all classes.
	TotalSizeBytes int64 `json:"totalSizeBytes"`
	// FileCount is RUN LOGS only, so it reads as "roughly how many runs are on
	// disk". Before the breakdown below it counted every file indiscriminately,
	// which quietly folded the process log and the folder sidecars into a number
	// labelled "log files".
	FileCount int `json:"fileCount"`
	// OldestLogAt is the oldest RUN log. Including the process log here would pin
	// it to a file that is rewritten continuously, making the value meaningless
	// exactly when an operator is trying to judge whether retention is working.
	OldestLogAt *string `json:"oldestLogAt"`
	// Classes breaks the tree down by what each file actually is (LU-11). The
	// Settings panel exists to answer "what is filling my disk", and a single
	// total cannot.
	Classes LogClasses `json:"classes"`
}

// LogClasses is the per-class split of the log directory.
type LogClasses struct {
	RunLogs    LogClassStat `json:"runLogs"`
	ProcessLog LogClassStat `json:"processLog"`
	AuditLog   LogClassStat `json:"auditLog"`
	// Other catches the per-folder _meta.json sidecars and anything unexpected.
	// Reporting it rather than dropping it is the honest choice: a silently
	// discarded remainder makes the class totals disagree with TotalSizeBytes for
	// no visible reason.
	Other LogClassStat `json:"other"`
}

type LogClassStat struct {
	FileCount      int   `json:"fileCount"`
	TotalSizeBytes int64 `json:"totalSizeBytes"`
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}

// GetLogStorageConfig reads the log_storage_config singleton.
func GetLogStorageConfig(ctx context.Context, database *sql.DB, appCfg *config.Config) (*LogStorageConfig, error) {
	row := database.QueryRowContext(ctx, `
		SELECT backend, local_path, s3_bucket, s3_endpoint, s3_region,
		       s3_access_key, s3_secret_key_enc, s3_prefix, s3_use_ssl, s3_ca_pem,
		       sync_mode, sync_interval_sec, sync_at,
		       archived_count, archived_bytes,
		       last_sync_started_at, last_sync_finished_at, last_sync_error,
		       last_modified_by, last_modified_at
		FROM log_storage_config WHERE id=1`)
	var backend, localPath, s3Bucket, s3Endpoint, s3Region, s3AccessKey, s3SecretKeyEnc, s3Prefix, lastModBy, lastModAt sql.NullString
	var syncMode, syncAt, lastStarted, lastFinished, lastErr, s3CAPEM sql.NullString
	var useSSL, syncInterval, archivedCount, archivedBytes sql.NullInt64

	dbExists := true
	if err := row.Scan(&backend, &localPath, &s3Bucket, &s3Endpoint, &s3Region,
		&s3AccessKey, &s3SecretKeyEnc, &s3Prefix, &useSSL, &s3CAPEM,
		&syncMode, &syncInterval, &syncAt,
		&archivedCount, &archivedBytes, &lastStarted, &lastFinished, &lastErr,
		&lastModBy, &lastModAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			dbExists = false
		} else {
			return nil, fmt.Errorf("read log_storage_config: %w", err)
		}
	}

	cfg := &LogStorageConfig{
		Backend: "local",
		Local:   &LocalLogConfig{Path: defaultLogDir},
		Sync:    &LogSyncConfig{Mode: logSyncModeInterval, IntervalSeconds: logSyncDefaultSecs},
	}

	if dbExists {
		cfg.Backend = kvStrOr(backend.String, "local")
		cfg.Local = &LocalLogConfig{Path: kvStrOr(localPath.String, defaultLogDir)}
		cfg.LastModifiedBy = lastModBy.String
		cfg.LastModifiedAt = lastModAt.String

		if s3Bucket.Valid && s3Bucket.String != "" {
			ssl := !useSSL.Valid || useSSL.Int64 != 0
			cfg.S3 = &S3LogConfig{
				Endpoint:  s3Endpoint.String,
				Bucket:    s3Bucket.String,
				Region:    s3Region.String,
				AccessKey: s3AccessKey.String,
				Prefix:    s3Prefix.String,
				UseSSL:    &ssl,
				CAPEM:     s3CAPEM.String,
			}
		}
		cfg.Sync = &LogSyncConfig{
			Mode:            kvStrOr(syncMode.String, logSyncModeInterval),
			IntervalSeconds: int(syncInterval.Int64),
			At:              syncAt.String,
		}
		if cfg.Sync.IntervalSeconds <= 0 {
			cfg.Sync.IntervalSeconds = logSyncDefaultSecs
		}
		// Archive status is reported while the tier is on, and after it is
		// switched off for as long as objects remain — an operator who went back
		// to local still has bytes in a bucket to account for (SL-Q14).
		if cfg.Backend == "s3" || archivedCount.Int64 > 0 {
			cfg.Archive = &LogArchiveStatus{
				Count:              archivedCount.Int64,
				Bytes:              archivedBytes.Int64,
				LastSyncStartedAt:  nullStr(lastStarted),
				LastSyncFinishedAt: nullStr(lastFinished),
				LastSyncError:      nullStr(lastErr),
			}
			if cfg.Backend == "s3" {
				_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE `+pendingArchiveWhere).Scan(&cfg.Archive.Pending)
			}
		}
	}

	cfg.Stats = computeLogStats(cfg.Local.Path)
	return cfg, nil
}

func nullStr(v sql.NullString) *string {
	if !v.Valid || v.String == "" {
		return nil
	}
	s := v.String
	return &s
}

// computeLogStats walks the local run-log dir. A missing dir is zero stats,
// not an error — the dir is created lazily on first run.
//
// The walk is recursive, so the per-entity folders LU-7 introduces are traversed
// correctly and FileCount becomes ≈ the run count rather than a flat-directory
// artefact. What it did NOT do before LU-11 is distinguish the files it found:
// the process log, its rotated generations and the folder sidecars were all
// counted as run logs, so an operator reading "1,204 log files · 5.1 GB" could
// not tell run output from a single runaway process log.
func computeLogStats(dir string) *LogStorageStats {
	stats := &LogStorageStats{}
	var oldest time.Time
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // unstattable entry; keep walking
		}
		name := d.Name()
		stats.TotalSizeBytes += info.Size()

		var class *LogClassStat
		switch {
		case name == processLogName || strings.HasPrefix(name, processLogName+"."):
			class = &stats.Classes.ProcessLog
		case name == auditLogName || strings.HasPrefix(name, auditLogName+"."):
			class = &stats.Classes.AuditLog
		case name == metaFileName:
			class = &stats.Classes.Other
		case strings.HasSuffix(name, ".log"):
			class = &stats.Classes.RunLogs
			// Only run logs move the oldest marker.
			stats.FileCount++
			if oldest.IsZero() || info.ModTime().Before(oldest) {
				oldest = info.ModTime()
			}
		default:
			class = &stats.Classes.Other
		}
		class.FileCount++
		class.TotalSizeBytes += info.Size()
		return nil
	})
	if !oldest.IsZero() {
		s := oldest.UTC().Format(time.RFC3339)
		stats.OldestLogAt = &s
	}
	return stats
}

// UpdateLogStorageConfig updates the log_storage_config singleton.
//
// backend=s3 was persisted-but-rejected from the beta endpoint-update until
// SL-1: the row round-tripped but the save answered 422 because no writer
// existed. Now the save validates the S3 block, probes the bucket with the
// credentials it would store (refuse-don't-persist, the stance GitLab settings
// take), and writes the row only when the probe passes. A failed probe is
// validation, not refusal — the S3 error code is in the message.
//
// The stored secret key is used for the probe when the request omits one
// (omit-to-keep), so an operator can change the schedule without re-typing it.
func UpdateLogStorageConfig(ctx context.Context, database *sql.DB, appCfg *config.Config, inp LogStorageConfig, actor string) (*LogStorageConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	if inp.Backend != "local" && inp.Backend != "s3" {
		return nil, &ValidationError{Field: "backend", Message: "backend must be 'local' or 's3'"}
	}

	localPath := defaultLogDir
	if inp.Local != nil && inp.Local.Path != "" {
		if !strings.HasPrefix(inp.Local.Path, "/") {
			return nil, &ValidationError{Field: "local.path", Message: "local.path must be an absolute path"}
		}
		localPath = inp.Local.Path
	}

	// Sync: absent means "keep what is stored" so a form that predates the
	// field (or a PUT that only moves the local path) does not reset the
	// timetable. Present means validated and clamped.
	syncMode, syncInterval, syncAt, err := resolveSync(ctx, database, inp.Sync)
	if err != nil {
		return nil, err
	}

	var s3Bucket, s3Endpoint, s3Region, s3AccessKey, s3Prefix, s3SecretKeyEnc, s3CAPEM *string
	s3UseSSL := 1
	if inp.S3 != nil {
		// Stored fields the request may want to keep: the secret (always
		// omit-to-keep) and UseSSL (pointer, omit-to-keep).
		var existingSecretEnc sql.NullString
		var existingUseSSL sql.NullInt64
		qerr := database.QueryRowContext(ctx, `SELECT s3_secret_key_enc, s3_use_ssl FROM log_storage_config WHERE id=1`).Scan(&existingSecretEnc, &existingUseSSL)
		if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
			return nil, fmt.Errorf("read existing s3 settings: %w", qerr)
		}

		bucket := strings.TrimSpace(inp.S3.Bucket)
		endpoint := strings.TrimSpace(inp.S3.Endpoint)
		region := strings.TrimSpace(inp.S3.Region)
		accessKey := strings.TrimSpace(inp.S3.AccessKey)
		prefix := logarchive.NormalizePrefix(inp.S3.Prefix)
		s3Bucket, s3Endpoint, s3Region, s3AccessKey, s3Prefix = &bucket, &endpoint, &region, &accessKey, &prefix
		caPEM := strings.TrimSpace(inp.S3.CAPEM)
		if caPEM != "" {
			if _, perr := logarchive.ParseCAPEM(caPEM); perr != nil {
				return nil, &ValidationError{Field: "s3.caPem", Message: "s3.caPem must be a PEM bundle holding at least one certificate"}
			}
			s3CAPEM = &caPEM
		}

		if inp.S3.UseSSL != nil {
			if !*inp.S3.UseSSL {
				s3UseSSL = 0
			}
		} else if existingUseSSL.Valid {
			s3UseSSL = int(existingUseSSL.Int64)
		}

		// The plaintext secret this save will store — either the new one or the
		// decrypted stored one — is what the probe must use.
		secretPlain := inp.S3.SecretKey
		if secretPlain != "" {
			token, err := secrets.EncryptString(appCfg, secretPlain)
			if err != nil {
				return nil, fmt.Errorf("encrypt S3 secret key: %w", err)
			}
			s3SecretKeyEnc = &token
		} else if existingSecretEnc.Valid && existingSecretEnc.String != "" {
			s3SecretKeyEnc = &existingSecretEnc.String
			if inp.Backend == "s3" {
				plain, err := secrets.DecryptString(appCfg, existingSecretEnc.String)
				if err != nil {
					return nil, fmt.Errorf("decrypt stored S3 secret key: %w", err)
				}
				secretPlain = plain
			}
		}

		if inp.Backend == "s3" {
			if bucket == "" {
				return nil, &ValidationError{Field: "s3.bucket", Message: "s3.bucket is required for the S3 archive backend"}
			}
			if endpoint == "" && region == "" {
				return nil, &ValidationError{Field: "s3.region", Message: "s3.region is required when s3.endpoint is empty (AWS regional endpoint)"}
			}
			// SL-Q5: static keys are both-or-neither; neither means the ambient
			// AWS chain (env / shared file / IAM).
			if (accessKey == "") != (secretPlain == "") {
				return nil, &ValidationError{Field: "s3.accessKey", Message: "s3.accessKey and s3.secretKey must be set together (leave both empty to use the ambient AWS credential chain)"}
			}
			store, err := logarchive.New(logarchive.Params{
				ClientParams: logarchive.ClientParams{
					Endpoint: endpoint, Region: region, AccessKey: accessKey, SecretKey: secretPlain, UseSSL: s3UseSSL == 1, CAPEM: caPEM,
				},
				Bucket: bucket, Prefix: prefix,
			}, egressPolicy(appCfg), nil)
			if err != nil {
				return nil, &ValidationError{Field: "s3.endpoint", Message: err.Error()}
			}
			pctx, cancel := context.WithTimeout(ctx, logArchiveProbeTimeout)
			defer cancel()
			if err := store.Probe(pctx); err != nil {
				return nil, &ValidationError{Field: "s3.bucket", Message: "S3 bucket probe failed: " + err.Error()}
			}
		}
	} else if inp.Backend == "s3" {
		return nil, &ValidationError{Field: "s3", Message: "s3 connection settings are required for the S3 archive backend"}
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO log_storage_config
		  (id, backend, local_path, s3_bucket, s3_endpoint, s3_region, s3_access_key, s3_secret_key_enc, s3_prefix, s3_use_ssl, s3_ca_pem,
		   sync_mode, sync_interval_sec, sync_at,
		   last_modified_by, last_modified_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  backend=excluded.backend, local_path=excluded.local_path,
		  s3_bucket=excluded.s3_bucket, s3_endpoint=excluded.s3_endpoint,
		  s3_region=excluded.s3_region, s3_access_key=excluded.s3_access_key,
		  s3_secret_key_enc=excluded.s3_secret_key_enc, s3_prefix=excluded.s3_prefix,
		  s3_use_ssl=excluded.s3_use_ssl, s3_ca_pem=excluded.s3_ca_pem,
		  sync_mode=excluded.sync_mode, sync_interval_sec=excluded.sync_interval_sec, sync_at=excluded.sync_at,
		  last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at`,
		inp.Backend, localPath, s3Bucket, s3Endpoint, s3Region, s3AccessKey, s3SecretKeyEnc, s3Prefix, s3UseSSL, s3CAPEM,
		syncMode, syncInterval, syncAt, actor, now,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert log_storage_config: %w", err)
	}

	secrets.RedactionSourceChanged() // AM-4b
	_ = WriteChangeLog(ctx, database, actor, "Settings", "updated", "Log Storage Settings", "")
	return GetLogStorageConfig(ctx, database, appCfg)
}

// resolveSync validates a submitted timetable, or returns the stored one (or
// the defaults) when the request carried none.
func resolveSync(ctx context.Context, database *sql.DB, in *LogSyncConfig) (mode string, interval int, at *string, err error) {
	if in == nil {
		var m, a sql.NullString
		var i sql.NullInt64
		qerr := database.QueryRowContext(ctx, `SELECT sync_mode, sync_interval_sec, sync_at FROM log_storage_config WHERE id=1`).Scan(&m, &i, &a)
		if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
			return "", 0, nil, fmt.Errorf("read existing sync settings: %w", qerr)
		}
		mode = kvStrOr(m.String, logSyncModeInterval)
		interval = int(i.Int64)
		if interval <= 0 {
			interval = logSyncDefaultSecs
		}
		if a.Valid && a.String != "" {
			s := a.String
			at = &s
		}
		return mode, interval, at, nil
	}
	switch in.Mode {
	case logSyncModeInterval, "":
		mode = logSyncModeInterval
	case logSyncModeDaily:
		mode = logSyncModeDaily
	default:
		return "", 0, nil, &ValidationError{Field: "sync.mode", Message: "sync.mode must be 'interval' or 'daily'"}
	}
	interval = in.IntervalSeconds
	if interval <= 0 {
		interval = logSyncDefaultSecs
	}
	// Clamped, not refused (SL-Q3): the floor is a guard against a typo, not a
	// contract the caller has to know.
	interval = max(logSyncMinInterval, min(logSyncMaxInterval, interval))
	if mode == logSyncModeDaily {
		hhmm := strings.TrimSpace(in.At)
		if _, perr := time.Parse("15:04", hhmm); perr != nil {
			return "", 0, nil, &ValidationError{Field: "sync.at", Message: "sync.at must be a UTC wall-clock time in HH:MM form"}
		}
		at = &hhmm
	} else if a := strings.TrimSpace(in.At); a != "" {
		// Kept so switching back to daily remembers the time, but only if valid.
		if _, perr := time.Parse("15:04", a); perr == nil {
			at = &a
		}
	}
	return mode, interval, at, nil
}

// egressPolicy is the SU-7 posture for the archive client, sourced from config
// exactly as the backup uploader's is.
func egressPolicy(appCfg *config.Config) httpx.EgressPolicy {
	if appCfg == nil {
		return httpx.EgressPolicy{AllowPrivate: true}
	}
	return httpx.EgressPolicy{AllowPrivate: appCfg.OutboundAllowPrivate, AllowLoopback: appCfg.OutboundAllowLoopback}
}

// BuildLogArchive constructs the archive Store from the stored settings, or
// (nil, nil) when the backend is local. It decrypts the stored secret, which
// is why it lives here and not in logarchive. Called at mount and after every
// successful save (Server.applyLogArchive), the way ResolveLogDir seeds the
// log dir — no restart required.
func BuildLogArchive(ctx context.Context, database *sql.DB, appCfg *config.Config, log *slog.Logger) (*logarchive.Store, error) {
	var backend, bucket, endpoint, region, accessKey, secretEnc, prefix, caPEM sql.NullString
	var useSSL sql.NullInt64
	err := database.QueryRowContext(ctx, `
		SELECT backend, s3_bucket, s3_endpoint, s3_region, s3_access_key, s3_secret_key_enc, s3_prefix, s3_use_ssl, s3_ca_pem
		FROM log_storage_config WHERE id=1`).Scan(&backend, &bucket, &endpoint, &region, &accessKey, &secretEnc, &prefix, &useSSL, &caPEM)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read log_storage_config: %w", err)
	}
	if backend.String != "s3" || bucket.String == "" {
		return nil, nil
	}
	secret := ""
	if secretEnc.Valid && secretEnc.String != "" {
		secret, err = secrets.DecryptString(appCfg, secretEnc.String)
		if err != nil {
			return nil, fmt.Errorf("decrypt stored S3 secret key: %w", err)
		}
	}
	return logarchive.New(logarchive.Params{
		ClientParams: logarchive.ClientParams{
			Endpoint: endpoint.String, Region: region.String, AccessKey: accessKey.String, SecretKey: secret,
			UseSSL: !useSSL.Valid || useSSL.Int64 != 0, CAPEM: caPEM.String,
		},
		Bucket: bucket.String, Prefix: prefix.String,
	}, egressPolicy(appCfg), log)
}

// ResolveLogDir returns the effective run-log directory: the DB-configured
// local path when set, else the default.
//
// Read at startup by the runner mount to seed the in-process value, and by the
// nightly retention sweep. Since LU-5 a path change no longer needs a restart:
// the settings handler pushes the new directory to every writer via
// Server.applyLogDir, so this is a seed-and-refresh source rather than the
// per-request lookup it used to be.
func ResolveLogDir(ctx context.Context, database *sql.DB) string {
	var localPath sql.NullString
	err := database.QueryRowContext(ctx, `SELECT local_path FROM log_storage_config WHERE id=1`).Scan(&localPath)
	if err == nil && localPath.Valid && localPath.String != "" {
		return localPath.String
	}
	return defaultLogDir
}

// LogSyncSchedule is what the archive sweep (internal/logsync) needs to decide
// when to fire, read without the disk walk GetLogStorageConfig performs.
type LogSyncSchedule struct {
	Enabled         bool // backend == s3 with a bucket stored
	Mode            string
	IntervalSeconds int
	At              string
	LastFinishedAt  time.Time // zero when never
}

// ReadLogSyncSchedule reads the timetable half of log_storage_config.
func ReadLogSyncSchedule(ctx context.Context, database *sql.DB) (LogSyncSchedule, error) {
	var backend, bucket, mode, at, finished sql.NullString
	var interval sql.NullInt64
	err := database.QueryRowContext(ctx, `
		SELECT backend, s3_bucket, sync_mode, sync_interval_sec, sync_at, last_sync_finished_at
		FROM log_storage_config WHERE id=1`).Scan(&backend, &bucket, &mode, &interval, &at, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return LogSyncSchedule{Mode: logSyncModeInterval, IntervalSeconds: logSyncDefaultSecs}, nil
	}
	if err != nil {
		return LogSyncSchedule{}, fmt.Errorf("read log sync schedule: %w", err)
	}
	sch := LogSyncSchedule{
		Enabled:         backend.String == "s3" && bucket.String != "",
		Mode:            kvStrOr(mode.String, logSyncModeInterval),
		IntervalSeconds: int(interval.Int64),
		At:              at.String,
	}
	if sch.IntervalSeconds <= 0 {
		sch.IntervalSeconds = logSyncDefaultSecs
	}
	if finished.Valid && finished.String != "" {
		if t, perr := time.Parse(time.RFC3339, finished.String); perr == nil {
			sch.LastFinishedAt = t
		}
	}
	return sch, nil
}

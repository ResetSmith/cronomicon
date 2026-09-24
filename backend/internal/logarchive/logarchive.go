// Package logarchive is the S3 side of the run-log archive tier (SL band,
// the s3-logging plan).
//
// Run logs are written to local disk by the runner ingest and the SSH executor
// and stay there for the life of a run — live tailing depends on an append-only
// file with stable byte offsets. This package owns the OTHER copy: a Store over
// an S3-compatible bucket that a scheduled sweep (SL-2) copies sealed logs
// into, the reader (SL-3) falls back to when the local file is gone, and the
// retention sweep (SL-4) expires on its own knob.
//
// It is a leaf: it takes plaintext connection parameters and an egress policy,
// never *config.Config or the database. The settings package decrypts the
// stored secret key and builds the Params; internal/backup builds its
// env-sourced client through NewClient so there is exactly one MinIO
// construction site (and one place the SU-7 egress guard is applied).
package logarchive

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// ClientParams is what it takes to reach a bucket. Endpoint may be empty, in
// which case the AWS regional endpoint for Region is used; an explicit endpoint
// targets any S3-compatible service (MinIO, Ceph, R2). A scheme prefix on the
// endpoint is stripped — UseSSL decides the scheme.
type ClientParams struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// CAPEM is an optional PEM bundle of CA certificates to trust for the
	// endpoint, for a private S3 node signed by an internal CA. Empty means the
	// process trust store. Ignored when UseSSL is false.
	CAPEM string
}

// Params configures a Store: a client plus the bucket and key prefix.
type Params struct {
	ClientParams
	Bucket string
	// Prefix is the key prefix every object lives under. Normalised by New to no
	// leading slash and exactly one trailing slash; empty means the bucket root.
	Prefix string
}

// Store is one bucket + prefix, with the operations the archive tier needs and
// nothing else. Every method takes a context so the sweep can put per-file
// deadlines on it (SL-Q11).
type Store struct {
	client *minio.Client
	bucket string
	prefix string
	// credMode is a label for logging ("static" | "ambient chain (env/file/IAM)"),
	// never the key bytes.
	credMode string
}

// Object is one listed key.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ErrNoSuchBucket is returned by Probe when the bucket does not exist (as
// opposed to being unreachable or refusing the credentials).
var ErrNoSuchBucket = errors.New("bucket does not exist")

// NewClient builds the SSRF-guarded MinIO client. Shared with internal/backup.
//
// Credentials: both keys present → static V4; otherwise the ambient AWS chain —
// env vars, the shared credentials file, then IAM (EC2 IMDS / ECS task role /
// EKS web-identity). Returns the client and a short credential-mode label for
// logging (never the key bytes). (V1.1-11, SL-Q5)
func NewClient(p ClientParams, egress httpx.EgressPolicy) (*minio.Client, string, error) {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		if p.Region == "" {
			return nil, "", fmt.Errorf("s3: region is required when no endpoint is given")
		}
		endpoint = fmt.Sprintf("s3.%s.amazonaws.com", p.Region)
	}
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	endpoint = strings.TrimSuffix(endpoint, "/")

	creds, credMode := ResolveCreds(p.AccessKey, p.SecretKey)
	// SU-7: guard bucket egress (SSRF) against the operator-configured endpoint.
	// The IAM/IMDS credential probe inside ResolveCreds is a deliberate carve-out
	// and is NOT guarded (see there).
	var base *http.Transport
	if p.UseSSL && strings.TrimSpace(p.CAPEM) != "" {
		pool, err := ParseCAPEM(p.CAPEM)
		if err != nil {
			return nil, "", err
		}
		base = http.DefaultTransport.(*http.Transport).Clone()
		base.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:     creds,
		Secure:    p.UseSSL,
		Region:    p.Region,
		Transport: httpx.SafeTransport(base, egress),
	})
	if err != nil {
		return nil, "", fmt.Errorf("init s3 client: %w", err)
	}
	return client, credMode, nil
}

// ParseCAPEM builds a cert pool from a PEM bundle, refusing a bundle that
// holds no certificate — a pasted key or garbage would otherwise silently
// produce an empty pool that trusts nothing and fails every handshake with a
// message that names the server, not the setting.
func ParseCAPEM(pemText string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pemText)) {
		return nil, fmt.Errorf("s3: CA bundle contains no PEM certificate")
	}
	return pool, nil
}

// ResolveCreds picks static V4 credentials when both keys are set, otherwise the
// ambient AWS credential chain. Exported so internal/backup's tests can keep
// pinning the selection branch.
func ResolveCreds(accessKey, secretKey string) (*credentials.Credentials, string) {
	if accessKey != "" && secretKey != "" {
		return credentials.NewStaticV4(accessKey, secretKey, ""), "static"
	}
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{},
		// SU-7 carve-out: this client is intentionally NOT wrapped in the SSRF
		// egress guard. IAM credential resolution legitimately dials the EC2/ECS
		// metadata endpoint 169.254.169.254, which the guard blocks by design.
		// Leave it unguarded so link-local IMDS keeps working.
		&credentials.IAM{Client: &http.Client{Timeout: 10 * time.Second}},
	}), "ambient chain (env/file/IAM)"
}

// New builds a Store. It does not touch the network — call Probe for that.
func New(p Params, egress httpx.EgressPolicy, log *slog.Logger) (*Store, error) {
	if strings.TrimSpace(p.Bucket) == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	client, credMode, err := NewClient(p.ClientParams, egress)
	if err != nil {
		return nil, err
	}
	s := &Store{client: client, bucket: p.Bucket, prefix: NormalizePrefix(p.Prefix), credMode: credMode}
	if log != nil {
		log.Info("s3 log archive client initialized", "creds", credMode, "bucket", s.bucket, "prefix", s.prefix)
	}
	return s, nil
}

// NormalizePrefix strips leading slashes and guarantees exactly one trailing
// slash, so a prefix of "logs", "/logs", "logs/" and "/logs//" all mean the same
// folder. Empty stays empty (the bucket root).
func NormalizePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// Bucket returns the bucket name (for operator-facing messages).
func (s *Store) Bucket() string { return s.bucket }

// Prefix returns the normalised key prefix.
func (s *Store) Prefix() string { return s.prefix }

// CredMode returns the credential-mode label for logging.
func (s *Store) CredMode() string { return s.credMode }

// Key is the object key a run's log lives under (SL-Q9): the same two-shape
// rule as runner.LogPath — {prefix}{code}/{traceId}.log with an entity code,
// {prefix}{traceId}.log without one — so a human browsing the bucket sees the
// tree they already know from disk.
func (s *Store) Key(entityCode, traceID string) string {
	if entityCode == "" {
		return s.prefix + traceID + ".log"
	}
	return s.prefix + path.Join(entityCode, traceID+".log")
}

// MetaKey is the key of a folder's _meta.json sidecar (SL-Q9).
func (s *Store) MetaKey(entityCode string) string {
	return s.prefix + path.Join(entityCode, "_meta.json")
}

// Probe checks the bucket is reachable with these credentials. ErrNoSuchBucket
// when the endpoint answers but the bucket is not there; any other error is
// returned as-is (the S3 error code is in the message via minio's
// ErrorResponse, which is what the settings 422 surfaces).
func (s *Store) Probe(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("probe s3://%s: %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("probe s3://%s: %w", s.bucket, ErrNoSuchBucket)
	}
	return nil
}

// Put uploads localPath under key and returns the size the service reports.
// The caller compares it with the size it read before the upload began and
// stamps the archive marker only on a match (SL-Q11). Multipart above minio's
// default part size, so a multi-gigabyte SSH-executor log is not one request.
func (s *Store) Put(ctx context.Context, key, localPath, contentType string) (int64, error) {
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	info, err := s.client.FPutObject(ctx, s.bucket, key, localPath, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return 0, fmt.Errorf("put s3://%s/%s: %w", s.bucket, key, err)
	}
	return info.Size, nil
}

// Get opens key for reading from byte offset. size is the object's full size,
// so the reader can hand back the same X-Log-Offset a local read would. An
// offset at or past the end returns an empty reader and the size, mirroring the
// local handler's "past EOF is not an error" rule.
func (s *Store) Get(ctx context.Context, key string, offset int64) (io.ReadCloser, int64, error) {
	size, found, err := s.Head(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, ErrNotFound)
	}
	if offset >= size {
		return io.NopCloser(strings.NewReader("")), size, nil
	}
	opts := minio.GetObjectOptions{}
	if offset > 0 {
		if err := opts.SetRange(offset, 0); err != nil {
			return nil, 0, fmt.Errorf("get s3://%s/%s: range: %w", s.bucket, key, err)
		}
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, 0, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	return obj, size, nil
}

// ErrNotFound is returned by Get for a key that is not in the bucket.
var ErrNotFound = errors.New("object not found")

// Head returns the object's size and whether it exists. A missing key is
// (0, false, nil), not an error — the sweep and the reader both branch on it.
func (s *Store) Head(ctx context.Context, key string) (int64, bool, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("head s3://%s/%s: %w", s.bucket, key, err)
	}
	return info.Size, true, nil
}

// Delete removes key. Deleting a key that is already gone is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil && !IsNotFound(err) {
		return fmt.Errorf("delete s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// List returns every object under the store's prefix plus sub, sorted by key.
// Deliberately not called on any read path — a large bucket makes it slow
// (SL-Q13); it exists for the reconcile and for the folder-empty check.
func (s *Store) List(ctx context.Context, sub string) ([]Object, error) {
	prefix := s.prefix + strings.TrimPrefix(sub, "/")
	var out []Object
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, obj.Err)
		}
		out = append(out, Object{Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// IsNotFound reports whether err is S3's NoSuchKey / NotFound (a 404).
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	resp, ok := s3Response(err)
	if !ok {
		return false
	}
	return resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey" || resp.Code == "NotFound"
}

// s3Response unwraps err to minio's ErrorResponse. minio.ToErrorResponse does a
// bare type assertion, so a %w-wrapped error (every error this package returns)
// would read as "not an S3 error" without this.
func s3Response(err error) (minio.ErrorResponse, bool) {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp, true
	}
	return minio.ErrorResponse{}, false
}

// IsPermanent reports whether err is an S3 refusal that retrying cannot fix —
// the SL-Q6 "stamp failed and abort the tick" class. Network errors, timeouts
// and 5xx are transient and return false.
func IsPermanent(err error) bool {
	resp, ok := s3Response(err)
	if !ok {
		return false
	}
	switch resp.Code {
	case "AccessDenied", "NoSuchBucket", "InvalidAccessKeyId", "SignatureDoesNotMatch",
		"InvalidBucketName", "AllAccessDisabled", "AuthorizationHeaderMalformed":
		return true
	}
	return resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized
}

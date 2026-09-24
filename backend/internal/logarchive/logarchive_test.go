package logarchive

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/logarchive/fakes3"
)

// loopback: the fake runs on 127.0.0.1, which the SU-7 guard blocks by default.
var loopback = httpx.EgressPolicy{AllowPrivate: true, AllowLoopback: true}

func newStore(t *testing.T, fake *fakes3.Server, bucket, prefix string) *Store {
	t.Helper()
	s, err := New(Params{
		ClientParams: ClientParams{Endpoint: fake.Endpoint(), Region: "us-east-1", AccessKey: "AKIAFAKE", SecretKey: "fakesecret", UseSSL: false},
		Bucket:       bucket,
		Prefix:       prefix,
	}, loopback, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"": "", "/": "", "logs": "logs/", "/logs": "logs/", "logs/": "logs/", "/logs//": "logs/", " a/b ": "a/b/",
	}
	for in, want := range cases {
		if got := NormalizePrefix(in); got != want {
			t.Errorf("NormalizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyShapes(t *testing.T) {
	s := &Store{prefix: "amadeus/"}
	if got := s.Key("0a1b2c3d", "trace-1"); got != "amadeus/0a1b2c3d/trace-1.log" {
		t.Errorf("with code: %q", got)
	}
	if got := s.Key("", "trace-1"); got != "amadeus/trace-1.log" {
		t.Errorf("without code: %q", got)
	}
	if got := s.MetaKey("0a1b2c3d"); got != "amadeus/0a1b2c3d/_meta.json" {
		t.Errorf("meta: %q", got)
	}
	bare := &Store{prefix: ""}
	if got := bare.Key("", "t"); got != "t.log" {
		t.Errorf("bare: %q", got)
	}
}

func TestNewRequiresBucketAndRegionOrEndpoint(t *testing.T) {
	if _, err := New(Params{ClientParams: ClientParams{Endpoint: "x:1"}}, loopback, nil); err == nil {
		t.Fatal("expected bucket-required error")
	}
	if _, err := New(Params{Bucket: "b"}, loopback, nil); err == nil {
		t.Fatal("expected region-required error when endpoint is empty")
	}
	if _, err := New(Params{ClientParams: ClientParams{Region: "eu-west-1"}, Bucket: "b"}, loopback, nil); err != nil {
		t.Fatalf("region only should build the AWS endpoint: %v", err)
	}
}

func TestProbe(t *testing.T) {
	fake := fakes3.New("present")
	defer fake.Close()
	ctx := context.Background()

	if err := newStore(t, fake, "present", "").Probe(ctx); err != nil {
		t.Fatalf("probe present bucket: %v", err)
	}
	err := newStore(t, fake, "absent", "").Probe(ctx)
	if !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("probe absent bucket: want ErrNoSuchBucket, got %v", err)
	}

	fake.Fail("AccessDenied", 403)
	err = newStore(t, fake, "present", "").Probe(ctx)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("probe under AccessDenied: want permanent error, got %v", err)
	}
	if errors.Is(err, ErrNoSuchBucket) {
		t.Fatal("AccessDenied must not be reported as a missing bucket")
	}
}

func TestPutHeadGetListDelete(t *testing.T) {
	fake := fakes3.New("logs")
	defer fake.Close()
	ctx := context.Background()
	s := newStore(t, fake, "logs", "/amadeus/")

	dir := t.TempDir()
	local := filepath.Join(dir, "trace-1.log")
	content := []byte("line one\nline two\nline three\n")
	if err := os.WriteFile(local, content, 0o640); err != nil {
		t.Fatal(err)
	}

	key := s.Key("0a1b2c3d", "trace-1")
	size, err := s.Put(ctx, key, local, "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("Put reported size %d, want %d", size, len(content))
	}
	// The aws-chunked framing must have been stripped: the stored bytes are the
	// file's bytes, nothing more.
	if got, ok := fake.Get("logs", key); !ok || string(got) != string(content) {
		t.Fatalf("stored object = %q (ok=%v), want the file content", got, ok)
	}

	if sz, found, err := s.Head(ctx, key); err != nil || !found || sz != size {
		t.Fatalf("Head = (%d,%v,%v), want (%d,true,nil)", sz, found, err, size)
	}
	if _, found, err := s.Head(ctx, s.Key("0a1b2c3d", "nope")); err != nil || found {
		t.Fatalf("Head missing = (found=%v, err=%v), want (false,nil)", found, err)
	}

	// Full read.
	rc, total, err := s.Get(ctx, key, 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(content) || total != size {
		t.Fatalf("Get(0) = %q/%d, want full content/%d", got, total, size)
	}

	// Offset read mirrors the local handler's tail contract.
	rc, total, err = s.Get(ctx, key, 9)
	if err != nil {
		t.Fatalf("Get(9): %v", err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "line two\nline three\n" || total != size {
		t.Fatalf("Get(9) = %q/%d", got, total)
	}

	// Past EOF is empty, not an error.
	rc, total, err = s.Get(ctx, key, size+100)
	if err != nil {
		t.Fatalf("Get(past): %v", err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if len(got) != 0 || total != size {
		t.Fatalf("Get(past) = %q/%d, want empty/%d", got, total, size)
	}

	// Missing key is ErrNotFound.
	if _, _, err := s.Get(ctx, s.Key("", "missing"), 0); !errors.Is(err, ErrNotFound) || !IsNotFound(err) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	// List under the folder.
	fake.Put("logs", "amadeus/0a1b2c3d/_meta.json", []byte("{}"))
	fake.Put("logs", "other/x.log", []byte("x"))
	objs, err := s.List(ctx, "0a1b2c3d/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 || objs[0].Key != "amadeus/0a1b2c3d/_meta.json" || objs[1].Key != key {
		t.Fatalf("List = %+v", objs)
	}
	all, _ := s.List(ctx, "")
	if len(all) != 2 {
		t.Fatalf("List(all under prefix) = %d, want 2 (other/ excluded)", len(all))
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := fake.Get("logs", key); ok {
		t.Fatal("object still present after Delete")
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete of a gone key must be nil, got %v", err)
	}
}

func TestIsPermanent(t *testing.T) {
	fake := fakes3.New("logs")
	defer fake.Close()
	s := newStore(t, fake, "logs", "")
	ctx := context.Background()

	fake.Fail("AccessDenied", 403)
	_, _, err := s.Head(ctx, "k")
	if !IsPermanent(err) {
		t.Fatalf("AccessDenied should be permanent: %v", err)
	}
	fake.Fail("InternalError", 500)
	_, _, err = s.Head(ctx, "k")
	if err == nil || IsPermanent(err) {
		t.Fatalf("500 should be transient: %v", err)
	}
	fake.Fail("", 0)
	if IsPermanent(nil) {
		t.Fatal("nil is not permanent")
	}
	// Unreachable endpoint: transient.
	fake.Close()
	_, _, err = s.Head(ctx, "k")
	if err == nil || IsPermanent(err) {
		t.Fatalf("connection refused should be transient: %v", err)
	}
}

// A private S3 node with an internal CA: the pasted bundle makes the handshake
// succeed; without it the process trust store refuses httptest's self-signed
// certificate; a bundle with no certificate is refused at construction.
func TestPrivateCABundle(t *testing.T) {
	fake := fakes3.NewTLS("logs")
	defer fake.Close()
	ctx := context.Background()
	params := func(ca string) Params {
		return Params{
			ClientParams: ClientParams{Endpoint: fake.Endpoint(), Region: "us-east-1", AccessKey: "AK", SecretKey: "SK", UseSSL: true, CAPEM: ca},
			Bucket:       "logs",
		}
	}
	withCA, err := New(params(fake.CertPEM()), loopback, nil)
	if err != nil {
		t.Fatalf("New with CA: %v", err)
	}
	if err := withCA.Probe(ctx); err != nil {
		t.Fatalf("probe with the CA bundle should pass: %v", err)
	}
	without, err := New(params(""), loopback, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := without.Probe(ctx); err == nil {
		t.Fatal("probe without the CA bundle should fail the TLS handshake")
	}
	if _, err := New(params("not a certificate"), loopback, nil); err == nil {
		t.Fatal("a bundle with no certificate must be refused")
	}
	if _, err := ParseCAPEM(fake.CertPEM()); err != nil {
		t.Fatalf("ParseCAPEM: %v", err)
	}
}

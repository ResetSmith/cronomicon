package runner

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/logarchive"
	"github.com/ResetSmith/cronomicon/internal/logarchive/fakes3"
)

// SL-3 (the s3-logging plan) — the operator log read falls back to
// the S3 archive tier when the local file is gone and the run row says the log
// was archived. Pinned here, in the order that matters:
//
//  1. Local present → byte-for-byte the pre-SL-3 read (the archive is never
//     consulted while the file exists, even when the row is stamped).
//  2. Local reaped + archived → the body comes from the bucket, with the same
//     X-Log-Offset contract: full read, mid-object offset, past-EOF = 200-empty.
//  3. Local reaped + archived + no store (settings blanked) → 503
//     log_archive_unavailable, NOT 404: the log exists and the server knows where.
//  4. Local reaped + NOT archived → today's answer (200-empty, offset 0).
//  5. Archived per the row, but the object is gone from the bucket → 200-empty,
//     offset 0 (a reaped log's answer), never 5xx.

const archTrace = "0199a000-0000-7000-8000-00000000a3c1"

func archivedStore(t *testing.T, fake *fakes3.Server) *logarchive.Store {
	t.Helper()
	st, err := logarchive.New(logarchive.Params{
		ClientParams: logarchive.ClientParams{Endpoint: fake.Endpoint(), Region: "us-east-1", AccessKey: "AK", SecretKey: "SK"},
		Bucket:       "logs", Prefix: "cronomicon/",
	}, httpx.EgressPolicy{AllowPrivate: true, AllowLoopback: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func stampArchived(t *testing.T, svc *Service, traceID string) {
	t.Helper()
	if _, err := svc.db.Exec(`UPDATE runs SET status='success', log_archived_at='2026-09-09T12:00:00Z' WHERE id=?`, traceID); err != nil {
		t.Fatal(err)
	}
}

func TestGetLogArchiveFallback(t *testing.T) {
	fake := fakes3.New("logs")
	defer fake.Close()
	store := archivedStore(t, fake)
	const body = "alpha\nbeta\ngamma\n"
	size := strconv.Itoa(len(body))

	t.Run("local present wins even when stamped archived", func(t *testing.T) {
		svc := newTestService(t).WithLogArchive(func() *logarchive.Store { return store })
		seedLogRun(t, svc, archTrace, body)
		stampArchived(t, svc, archTrace)
		fake.Put("logs", "cronomicon/"+archTrace+".log", []byte("STALE"))
		rec := getLog(t, svc, archTrace, "")
		if rec.Code != 200 || rec.Body.String() != body || rec.Header().Get("X-Log-Offset") != size {
			t.Fatalf("local read: %d %q offset=%s", rec.Code, rec.Body.String(), rec.Header().Get("X-Log-Offset"))
		}
		if len(fake.Requests) != 0 {
			t.Fatalf("archive consulted while the local file exists: %v", fake.Requests)
		}
	})

	t.Run("local reaped + archived streams from the bucket with the offset contract", func(t *testing.T) {
		svc := newTestService(t).WithLogArchive(func() *logarchive.Store { return store })
		path := seedLogRun(t, svc, archTrace, body)
		stampArchived(t, svc, archTrace)
		fake.Put("logs", "cronomicon/"+archTrace+".log", []byte(body))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}

		rec := getLog(t, svc, archTrace, "")
		if rec.Code != 200 || rec.Body.String() != body || rec.Header().Get("X-Log-Offset") != size {
			t.Fatalf("full archived read: %d %q offset=%s", rec.Code, rec.Body.String(), rec.Header().Get("X-Log-Offset"))
		}
		rec = getLog(t, svc, archTrace, "?offset=6")
		if rec.Code != 200 || rec.Body.String() != "beta\ngamma\n" || rec.Header().Get("X-Log-Offset") != size {
			t.Fatalf("offset archived read: %d %q offset=%s", rec.Code, rec.Body.String(), rec.Header().Get("X-Log-Offset"))
		}
		rec = getLog(t, svc, archTrace, "?offset=999")
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("X-Log-Offset") != size {
			t.Fatalf("past-EOF archived read: %d %q offset=%s", rec.Code, rec.Body.String(), rec.Header().Get("X-Log-Offset"))
		}
		if rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("archived with an entity code uses the folder key", func(t *testing.T) {
		svc := newTestService(t).WithLogArchive(func() *logarchive.Store { return store })
		if _, err := svc.db.Exec(`INSERT INTO runs(id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at, entity_code, log_archived_at)
			VALUES(?, 'j', 'bash', '', 'success', 'tester', 'manual', 'ssh', datetime('now'), '0a1b2c3d', '2026-09-09T12:00:00Z')`, archTrace); err != nil {
			t.Fatal(err)
		}
		fake.Put("logs", "cronomicon/0a1b2c3d/"+archTrace+".log", []byte("foldered\n"))
		rec := getLog(t, svc, archTrace, "")
		if rec.Code != 200 || rec.Body.String() != "foldered\n" {
			t.Fatalf("foldered archived read: %d %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("archived but no store is 503, not 404", func(t *testing.T) {
		svc := newTestService(t).WithLogArchive(func() *logarchive.Store { return nil })
		path := seedLogRun(t, svc, archTrace, body)
		stampArchived(t, svc, archTrace)
		_ = os.Remove(path)
		rec := getLog(t, svc, archTrace, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("no store: %d %s", rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); !strings.Contains(got, "log_archive_unavailable") {
			t.Fatalf("body should name log_archive_unavailable: %s", got)
		}
		// And a service with no getter wired at all behaves the same.
		svc2 := newTestService(t)
		path2 := seedLogRun(t, svc2, archTrace, body)
		stampArchived(t, svc2, archTrace)
		_ = os.Remove(path2)
		if rec := getLog(t, svc2, archTrace, ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("no getter: %d", rec.Code)
		}
	})

	t.Run("local reaped and not archived is the pre-SL-3 answer", func(t *testing.T) {
		svc := newTestService(t).WithLogArchive(func() *logarchive.Store { return store })
		path := seedLogRun(t, svc, archTrace, body)
		_ = os.Remove(path)
		rec := getLog(t, svc, archTrace, "")
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("X-Log-Offset") != "0" {
			t.Fatalf("unarchived reaped: %d %q offset=%s", rec.Code, rec.Body.String(), rec.Header().Get("X-Log-Offset"))
		}
	})

	t.Run("archived per the row but gone from the bucket answers as reaped", func(t *testing.T) {
		gone := fakes3.New("logs")
		defer gone.Close()
		svc := newTestService(t).WithLogArchive(func() *logarchive.Store { return archivedStore(t, gone) })
		path := seedLogRun(t, svc, archTrace, body)
		stampArchived(t, svc, archTrace)
		_ = os.Remove(path)
		rec := getLog(t, svc, archTrace, "")
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("X-Log-Offset") != "0" {
			t.Fatalf("object gone: %d %q offset=%s", rec.Code, rec.Body.String(), rec.Header().Get("X-Log-Offset"))
		}
	})

	t.Run("bucket refusing is 503", func(t *testing.T) {
		refusing := fakes3.New("logs")
		defer refusing.Close()
		refusing.Fail("AccessDenied", 403)
		svc := newTestService(t).WithLogArchive(func() *logarchive.Store { return archivedStore(t, refusing) })
		path := seedLogRun(t, svc, archTrace, body)
		stampArchived(t, svc, archTrace)
		_ = os.Remove(path)
		if rec := getLog(t, svc, archTrace, ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("refusing bucket: %d %s", rec.Code, rec.Body.String())
		}
	})
}

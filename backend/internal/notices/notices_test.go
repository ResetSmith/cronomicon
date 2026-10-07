package notices_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/notices"
)

func open(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *sql.DB, q string, a ...any) {
	t.Helper()
	if _, err := pool.Exec(q, a...); err != nil {
		t.Fatalf("exec: %v\n%s", err, q)
	}
}

func openOf(t *testing.T, pool *sql.DB, kind string) map[string]notices.Notice {
	t.Helper()
	all, err := notices.ListOpen(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]notices.Notice{}
	for _, n := range all {
		if n.Kind == kind {
			out[n.Subject] = n
		}
	}
	return out
}

// A notice is a condition keyed by (kind, subject): seeing it again is not a
// second notice, a dismissal sticks while the condition stands, and a condition
// that goes away and comes back is a new occurrence nobody has dismissed.
func TestANoticeIsAConditionNotAnEvent(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	f := notices.Finding{AgencyID: "global", Subject: "secret:s1", Detail: "first wording"}

	for range 3 {
		if err := notices.Upsert(ctx, pool, "k", f); err != nil {
			t.Fatal(err)
		}
	}
	got := openOf(t, pool, "k")
	if len(got) != 1 {
		t.Fatalf("%d notices after three sightings of one condition, want 1", len(got))
	}
	first := got["secret:s1"]

	// A later sighting refreshes what it says and when it was last seen.
	mustExec(t, pool, `UPDATE notices SET first_seen_at = '2026-01-01T00:00:00Z', last_seen_at = '2026-01-01T00:00:00Z' WHERE id = ?`, first.ID)
	f.Detail, f.AgencyID = "second wording", "ag-fin"
	if err := notices.Upsert(ctx, pool, "k", f); err != nil {
		t.Fatal(err)
	}
	n := openOf(t, pool, "k")["secret:s1"]
	if n.ID != first.ID || n.Detail != "second wording" || n.AgencyID != "ag-fin" {
		t.Errorf("after a later sighting: %+v", n)
	}
	if n.FirstSeenAt != "2026-01-01T00:00:00Z" || n.LastSeenAt == "2026-01-01T00:00:00Z" {
		t.Errorf("first/last seen = %s / %s, want first kept and last moved", n.FirstSeenAt, n.LastSeenAt)
	}

	// Dismissed: hidden, and it stays hidden while the condition stands.
	if ok, err := notices.Dismiss(ctx, pool, n.ID, "alice"); err != nil || !ok {
		t.Fatalf("dismiss = %v, %v", ok, err)
	}
	if ok, _ := notices.Dismiss(ctx, pool, n.ID, "bob"); ok {
		t.Error("a second dismissal reported a change")
	}
	if err := notices.Upsert(ctx, pool, "k", f); err != nil {
		t.Fatal(err)
	}
	if len(openOf(t, pool, "k")) != 0 {
		t.Error("a dismissed notice came back while its condition still stood")
	}
	stored, _ := notices.Get(ctx, pool, n.ID)
	if stored == nil || stored.DismissedBy == nil || *stored.DismissedBy != "alice" {
		t.Errorf("dismissal not recorded against the first person: %+v", stored)
	}

	// Resolved, then seen again: a new occurrence, open, with nobody's dismissal.
	if err := notices.Resolve(ctx, pool, "k", "secret:s1"); err != nil {
		t.Fatal(err)
	}
	if err := notices.Upsert(ctx, pool, "k", f); err != nil {
		t.Fatal(err)
	}
	back := openOf(t, pool, "k")["secret:s1"]
	if back.ID != first.ID {
		t.Fatalf("the returning condition is not open again: %+v", openOf(t, pool, "k"))
	}
	if back.FirstSeenAt == "2026-01-01T00:00:00Z" {
		t.Error("the returning condition kept the first occurrence's first-seen time")
	}

	if err := notices.Upsert(ctx, pool, "k", notices.Finding{Subject: "x", Detail: "no agency"}); err == nil {
		t.Error("a notice with no agency was accepted")
	}
}

// Reconcile makes the open notices of a kind equal to what the check found.
func TestReconcileOpensAndResolves(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	fs := func(subjects ...string) []notices.Finding {
		var out []notices.Finding
		for _, s := range subjects {
			out = append(out, notices.Finding{AgencyID: "global", Subject: s, Detail: s})
		}
		return out
	}
	if err := notices.Reconcile(ctx, pool, "k", fs("a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	// Another kind is not this check's to resolve.
	if err := notices.Reconcile(ctx, pool, "other", fs("a")); err != nil {
		t.Fatal(err)
	}
	if err := notices.Reconcile(ctx, pool, "k", fs("b", "d")); err != nil {
		t.Fatal(err)
	}
	got := openOf(t, pool, "k")
	if len(got) != 2 || got["b"].Subject != "b" || got["d"].Subject != "d" {
		t.Errorf("open after the second pass = %v, want b and d", got)
	}
	if len(openOf(t, pool, "other")) != 1 {
		t.Error("reconciling one kind resolved another's notice")
	}
	if err := notices.Reconcile(ctx, pool, "k", nil); err != nil {
		t.Fatal(err)
	}
	if len(openOf(t, pool, "k")) != 0 {
		t.Error("a check that found nothing left notices open")
	}
}

// The checks, against the conditions they exist for. Each appears when the
// condition does and resolves itself when the condition is put right.
func TestChecksFindAndResolveTheirConditions(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	const ts = "2026-01-01T00:00:00Z"
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', ?), ('ag-tax', 'Tax', ?)`, ts, ts)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc1', 'prod', 'cronomicon', ?)`, ts)
	mustExec(t, pool, `INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('r1', 'runner-one', 'online', ?, ?)`, ts, ts)
	// Global's own, an agency's own, and one Global owns that two agencies use.
	mustExec(t, pool, `INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('s-global', 'SHARED', NULL, 'stored', ?)`, ts)
	mustExec(t, pool, `INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('s-fin', 'FIN_ONLY', 'prod', 'stored', ?, 'ag-fin')`, ts)
	mustExec(t, pool, `INSERT INTO secret_agencies VALUES ('s-fin', 'ag-fin')`)
	mustExec(t, pool, `INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('s-both', 'DB_PASSWORD', 'prod', 'stored', ?)`, ts)
	mustExec(t, pool, `INSERT INTO secret_agencies VALUES ('s-both', 'ag-fin'), ('s-both', 'ag-tax')`)
	mustExec(t, pool, `INSERT INTO ssh_credentials (id, label, source, created_at) VALUES ('k-held', 'deploy', 'stored', ?)`, ts)
	mustExec(t, pool, `INSERT INTO ssh_credential_agencies VALUES ('k-held', 'ag-tax')`)

	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	shared := openOf(t, pool, notices.KindSharedOwnership)
	if len(shared) != 2 {
		t.Fatalf("shared_ownership = %v, want the secret two agencies use and the key held back", shared)
	}
	n := shared["secret:s-both"]
	if n.AgencyID != "global" {
		t.Errorf("the notice belongs to %q, want global: only a global administrator can change the row", n.AgencyID)
	}
	for _, want := range []string{"DB_PASSWORD", "prod", "Finance, Tax"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("detail does not name %q: %s", want, n.Detail)
		}
	}
	if !strings.Contains(shared["ssh-credential:k-held"].Detail, "deploy") {
		t.Errorf("the key's notice does not name it: %s", shared["ssh-credential:k-held"].Detail)
	}
	if got := openOf(t, pool, notices.KindOrphaned); len(got) != 0 {
		t.Fatalf("orphaned = %v on a healthy database", got)
	}

	// Settle one (make it Global's), and break two rows around the application.
	mustExec(t, pool, `DELETE FROM secret_agencies WHERE secret_id = 's-both'`)
	mustExec(t, pool, `INSERT INTO secret_agencies VALUES ('s-both', 'global')`)
	mustExec(t, pool, `DELETE FROM runner_agencies WHERE runner_id = 'r1'`)
	mustExec(t, pool, `DELETE FROM scope_agencies WHERE scope_id = 'sc1'`)
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if shared := openOf(t, pool, notices.KindSharedOwnership); len(shared) != 1 || shared["ssh-credential:k-held"].Subject == "" {
		t.Errorf("shared_ownership after settling the secret = %v, want only the key", shared)
	}
	orphans := openOf(t, pool, notices.KindOrphaned)
	if len(orphans) != 2 || !strings.Contains(orphans["runner:r1"].Detail, "runner-one") || !strings.Contains(orphans["scope:sc1"].Detail, "prod") {
		t.Fatalf("orphaned = %v, want the runner and the scope by name", orphans)
	}

	// Re-home them; the notices go by themselves.
	mustExec(t, pool, `INSERT INTO runner_agencies VALUES ('r1', 'ag-fin')`)
	mustExec(t, pool, `INSERT INTO scope_agencies VALUES ('sc1', 'global')`)
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if got := openOf(t, pool, notices.KindOrphaned); len(got) != 0 {
		t.Errorf("orphaned after re-homing = %v", got)
	}
}

// The rename notice is the migration's. The check only ends it: when the
// agency has a name of its own again, or is gone.
func TestTheRenameNoticeResolvesWhenTheAgencyIsNamed(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-old', 'GLOBAL (renamed)', 't'), ('ag-gone', 'x', 't')`)
	for _, id := range []string{"ag-old", "ag-gone-already"} {
		if err := notices.Upsert(ctx, pool, notices.KindAgencyRenamed, notices.Finding{AgencyID: "global", Subject: id, Detail: "renamed"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatal(err)
	}
	got := openOf(t, pool, notices.KindAgencyRenamed)
	if len(got) != 1 || got["ag-old"].Subject == "" {
		t.Fatalf("open = %v, want the still-renamed agency and not the one that is gone", got)
	}
	mustExec(t, pool, `UPDATE agencies SET name = 'Shared Services' WHERE id = 'ag-old'`)
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := openOf(t, pool, notices.KindAgencyRenamed); len(got) != 0 {
		t.Errorf("open after the agency was given its own name = %v", got)
	}
}

// Opening the inbox runs the checks, but not once per reader.
func TestRefresherRunsAtMostOncePerInterval(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('r1', 'one', 'online', 't', 't')`)
	r := &notices.Refresher{Interval: time.Hour}
	if err := r.Refresh(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `DELETE FROM runner_agencies WHERE runner_id = 'r1'`)
	if err := r.Refresh(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := openOf(t, pool, notices.KindOrphaned); len(got) != 0 {
		t.Fatalf("a second refresh inside the interval ran the checks again: %v", got)
	}
	r.Interval = 0
	// A cancelled request must not leave the list half-built for the next reader.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.Refresh(cancelled, pool); err != nil {
		t.Fatalf("refresh on a cancelled request: %v", err)
	}
	if got := openOf(t, pool, notices.KindOrphaned); len(got) != 1 {
		t.Fatalf("orphaned after a refresh past the interval = %v, want the runner", got)
	}
}

package gitlab

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// RX-13 — the Git authoring surface for reactions.
//
// The calendar-binding equivalent of this file exists because a pre-commit
// review found two real bugs living exactly here (first-class schedule bindings
// escaped validation entirely, and were never persisted). Reactions have the
// same shape of risk with a sharper consequence: a binding that silently fails
// to persist means a cascade that silently never happens, and there is no
// screen yet on which its absence would be visible.

func seedRxDefinition(t *testing.T, pool *sql.DB, kind, source, name string) {
	t.Helper()
	q := `INSERT INTO jobs (name, source, run_type, enabled, synced_at) VALUES (?, ?, 'bash', 1, 't')`
	if kind == "workflow" {
		q = `INSERT INTO workflows (name, source, steps, enabled, synced_at) VALUES (?, ?, '[]', 1, 't')`
	}
	if _, err := pool.ExecContext(context.Background(), q, name, source); err != nil {
		t.Fatalf("seed %s %s: %v", kind, name, err)
	}
}

func rxJob(name string, rs ...ReactionEntry) JobYAML {
	j := JobYAML{}
	j.APIVersion = requiredAPIVersion
	j.Kind = "Job"
	j.Metadata.Name = name
	j.Spec.RunType = "bash"
	j.Spec.ConcurrencyPolicy = "Allow"
	j.Spec.Reactions = rs
	return j
}

// A Git-authored reaction reaches the runtime table with every field intact.
func TestReactionsPersistFromJobYAML(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	seedRxDefinition(t, pool, "job", "git", "extract")

	j := rxJob("load", ReactionEntry{
		Name: "after-extract", OnKind: "job", OnName: "extract", OnOutcome: "success",
		DelaySeconds: 30, MinIntervalSeconds: 300, IncludeWorkflowChildren: true,
	})

	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := svc.upsertJobs(context.Background(), tx, []JobYAML{j}, nil, nil, now, "sha1"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var onKind, onName, onSource, outcome string
	var delay, minInt, children, enabled int
	if err := pool.QueryRow(`
		SELECT on_kind, on_name, on_source, on_outcome, delay_seconds,
		       min_interval_seconds, include_workflow_children, enabled
		  FROM reactions WHERE owner_name='load' AND name='after-extract'`).
		Scan(&onKind, &onName, &onSource, &outcome, &delay, &minInt, &children, &enabled); err != nil {
		t.Fatalf("read reaction: %v", err)
	}
	// Every authored field, because this write is field-by-field and a dropped
	// one is invisible until the reaction misbehaves in production.
	if onKind != "job" || onName != "extract" || onSource != "git" || outcome != "success" {
		t.Errorf("edge = %s:%s/%s on %s, want job:git/extract on success", onKind, onSource, onName, outcome)
	}
	if delay != 30 || minInt != 300 || children != 1 || enabled != 1 {
		t.Errorf("fields = (delay %d, minInterval %d, children %d, enabled %d), want (30, 300, 1, 1)",
			delay, minInt, children, enabled)
	}
}

// A Git-authored reaction can watch an IN-APP definition. Defaulting onSource
// to 'git' with no way to override it would make cross-plane edges
// unauthorable from the repo, which is exactly the installation running both
// sources that most needs them.
func TestReactionCanWatchAnAmadeusSourceUpstream(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	seedRxDefinition(t, pool, "job", "amadeus", "in-app-job")

	j := rxJob("load", ReactionEntry{
		Name: "after-inapp", OnKind: "job", OnName: "in-app-job",
		OnSource: "amadeus", OnOutcome: "any",
	})
	tx, _ := pool.BeginTx(context.Background(), nil)
	defer tx.Rollback()
	if err := svc.upsertJobs(context.Background(), tx, []JobYAML{j}, nil, nil,
		time.Now().UTC().Format(time.RFC3339), "sha1"); err != nil {
		t.Fatalf("upsertJobs: %v", err)
	}
	_ = tx.Commit()

	var onSource string
	if err := pool.QueryRow(
		`SELECT on_source FROM reactions WHERE owner_name='load'`).Scan(&onSource); err != nil {
		t.Fatalf("read reaction: %v", err)
	}
	if onSource != "amadeus" {
		t.Errorf("on_source = %q, want amadeus — a repo must be able to author a cross-plane edge", onSource)
	}
}

// The write is a source-scoped REPLACE, so editing the repo's list removes what
// it no longer contains — and leaves an operator's in-app reactions on the same
// definition name untouched (A9).
func TestReactionsReplaceIsSourceScoped(t *testing.T) {
	pool := mustOpenDB(t)
	svc := &Service{db: pool, cloneDir: t.TempDir()}
	seedRxDefinition(t, pool, "job", "git", "up")
	seedRxDefinition(t, pool, "job", "git", "up2")

	// An operator's in-app reaction on a same-named definition.
	if _, err := pool.Exec(`
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome)
		VALUES ('amadeus','job','load','operator-authored','git','job','up','failure')`); err != nil {
		t.Fatal(err)
	}

	write := func(rs ...ReactionEntry) {
		tx, _ := pool.BeginTx(context.Background(), nil)
		defer tx.Rollback()
		if err := svc.upsertJobs(context.Background(), tx, []JobYAML{rxJob("load", rs...)}, nil, nil,
			time.Now().UTC().Format(time.RFC3339), "sha1"); err != nil {
			t.Fatalf("upsertJobs: %v", err)
		}
		_ = tx.Commit()
	}

	write(
		ReactionEntry{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "success"},
		ReactionEntry{Name: "b", OnKind: "job", OnName: "up2", OnOutcome: "failure"},
	)
	// The repo drops one.
	write(ReactionEntry{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "success"})

	var gitCount, amadeusCount int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM reactions WHERE owner_source='git' AND owner_name='load'`).Scan(&gitCount)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM reactions WHERE owner_source='amadeus' AND owner_name='load'`).Scan(&amadeusCount)
	if gitCount != 1 {
		t.Errorf("git reactions = %d, want 1 — the replace must drop what the repo removed", gitCount)
	}
	if amadeusCount != 1 {
		t.Errorf("in-app reactions = %d, want 1 — a git sync must never wipe an operator's own rows", amadeusCount)
	}
}

// Deleting the OWNER takes its reactions with it, via the migration's trigger —
// which is why the sync prune needs no reaction-specific statement.
func TestOwnerDeleteCascadesReactions(t *testing.T) {
	pool := mustOpenDB(t)
	seedRxDefinition(t, pool, "job", "git", "up")
	seedRxDefinition(t, pool, "job", "git", "down")
	if _, err := pool.Exec(`
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome)
		VALUES ('git','job','down','r','git','job','up','success')`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(`DELETE FROM jobs WHERE source='git' AND name='down'`); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM reactions WHERE owner_name='down'`).Scan(&n)
	if n != 0 {
		t.Errorf("owner's reactions survived its delete (%d), want 0", n)
	}

	// The reverse: deleting the WATCHED definition leaves the reaction dangling.
	// This is what makes dangling a supported state rather than an edge case —
	// the prune has no request to fail and no operator to ask.
	if _, err := pool.Exec(`
		INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
		                       on_source, on_kind, on_name, on_outcome)
		VALUES ('git','job','other','r','git','job','up','success')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`DELETE FROM jobs WHERE source='git' AND name='up'`); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM reactions WHERE owner_name='other'`).Scan(&n)
	if n != 1 {
		t.Errorf("a reaction was cascaded away by its UPSTREAM's delete (%d rows, want 1) — that "+
			"would be a silent capability loss", n)
	}
}

// NormalizeReactions is the DB-free shape check `amadeus validate` runs at MR
// time. The table mirrors the calendar refusal table's shape.
func TestNormalizeReactionsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      []ReactionEntry
		wantErr bool
	}{
		{"valid", []ReactionEntry{{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "success"}}, false},
		{"valid any", []ReactionEntry{{Name: "a", OnKind: "workflow", OnName: "w", OnOutcome: "any"}}, false},
		{"no name", []ReactionEntry{{OnKind: "job", OnName: "up", OnOutcome: "success"}}, true},
		{"bad slug", []ReactionEntry{{Name: "Bad Name", OnKind: "job", OnName: "up", OnOutcome: "success"}}, true},
		{"duplicate", []ReactionEntry{
			{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "success"},
			{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "failure"},
		}, true},
		{"bad kind", []ReactionEntry{{Name: "a", OnKind: "script", OnName: "up", OnOutcome: "success"}}, true},
		{"no upstream name", []ReactionEntry{{Name: "a", OnKind: "job", OnOutcome: "success"}}, true},
		{"bad source", []ReactionEntry{{Name: "a", OnKind: "job", OnName: "up", OnSource: "svn", OnOutcome: "success"}}, true},
		// `killed` and `warning` are runs.status values that normalisation has
		// already folded into stopped and success. Accepting either would let an
		// author write a reaction that can never fire.
		{"status value as outcome", []ReactionEntry{{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "killed"}}, true},
		{"warning as outcome", []ReactionEntry{{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "warning"}}, true},
		{"negative delay", []ReactionEntry{{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "success", DelaySeconds: -1}}, true},
		{"negative interval", []ReactionEntry{{Name: "a", OnKind: "job", OnName: "up", OnOutcome: "success", MinIntervalSeconds: -5}}, true},
		{"empty list", nil, false},
	} {
		_, errs := NormalizeReactions(tc.in)
		if got := len(errs) > 0; got != tc.wantErr {
			t.Errorf("%s: errors=%v (%v), want error=%v", tc.name, got, errs, tc.wantErr)
		}
	}
}

// End-to-end through a real repo, real migrations and a real sync: a reaction
// naming an upstream that does not exist drops the whole definition from the
// sync and disables its prune (PP-B2), rather than being written and silently
// never firing. A valid one lands in the table.
func TestSyncValidatesAndPersistsReactions(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)

	// `keep` already exists in the fixture; `reactor` watches it — valid.
	gitCommitFile(t, repo, remote, "jobs/reactor.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: reactor\n"+
			"spec:\n  run_type: bash\n  command: echo hi\n  reactions:\n"+
			"    - name: after-keep\n      onKind: job\n      onName: keep\n      onOutcome: success\n",
		"add reactor")
	if r := svc.SyncBlocking(context.Background(), "t"); r.Status == "failed" {
		t.Fatalf("sync failed: %s", r.ErrorMessage)
	}
	var n int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE owner_name='reactor'`).Scan(&n)
	if n != 1 {
		t.Fatalf("a valid Git-authored reaction did not persist (%d rows, want 1)", n)
	}

	// Now point it at a definition that does not exist.
	gitCommitFile(t, repo, remote, "jobs/reactor.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: reactor\n"+
			"spec:\n  run_type: bash\n  command: echo hi\n  reactions:\n"+
			"    - name: after-ghost\n      onKind: job\n      onName: ghost\n      onOutcome: success\n",
		"break reactor")
	res := svc.SyncBlocking(context.Background(), "t")
	found := false
	for _, ve := range res.Errors {
		if strings.Contains(ve.Message, "ghost") {
			found = true
			// The locator has to name the file and the field, or an MR reviewer
			// cannot tell which of a repo's definitions is broken.
			if !strings.Contains(ve.File, "reactor") || !strings.Contains(ve.Field, "reactions[after-ghost]") {
				t.Errorf("locator = %s / %s, want the file and the named reaction", ve.File, ve.Field)
			}
		}
	}
	if !found {
		t.Errorf("a reaction naming a missing upstream produced no structured error; got %+v", res.Errors)
	}
	// The job was DROPPED, so its old reaction row survives untouched rather
	// than the broken definition half-landing.
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE owner_name='reactor' AND name='after-keep'`).Scan(&n)
	if n != 1 {
		t.Errorf("the previously-good reaction was clobbered by a rejected sync (%d rows, want 1)", n)
	}
}

// Two definitions authored in the SAME repo, one reacting to the other, must
// validate on the FIRST sync.
//
// The upstream check runs before the upsert transaction, so consulting the
// database for a git-source upstream would reject this repo on sync #1 and
// accept the identical repo on sync #2 — a validity rule that depends on how
// many times you have synced. This is the fresh-install case, so it would have
// shipped broken and looked like a flaky first deploy.
func TestReactionToASiblingInTheSameRepoValidatesOnFirstSync(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)

	// Two brand-new jobs, neither in the DB, one watching the other.
	gitCommitFile(t, repo, remote, "jobs/producer.yaml", jobYAML("producer"), "add producer")
	gitCommitFile(t, repo, remote, "jobs/consumer.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: consumer\n"+
			"spec:\n  run_type: bash\n  command: echo hi\n  reactions:\n"+
			"    - name: after-producer\n      onKind: job\n      onName: producer\n      onOutcome: success\n",
		"add consumer")

	res := svc.SyncBlocking(context.Background(), "t")
	for _, ve := range res.Errors {
		if strings.Contains(ve.Message, "producer") {
			t.Errorf("first sync rejected a reaction to a sibling in the same repo: %s", ve.Message)
		}
	}
	var n int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE owner_name='consumer'`).Scan(&n)
	if n != 1 {
		t.Errorf("sibling reaction rows = %d, want 1 — the repo is authoritative for its own "+
			"definitions, so this must not need a second sync", n)
	}
}

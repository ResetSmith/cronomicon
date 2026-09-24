package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// The claim-query benchmark (the agencies plan T3.1/T3.3).
//
// claimRun is THE hot path: one atomic `UPDATE … RETURNING` per runner per poll,
// fleet-wide, forever. Phase 3 replaces its scalar `agency = ?` match with a SET
// INTERSECTION over `runs.agencies_json` (AG-Q2b), adding a THIRD correlated
// `json_each` to a query that already runs two. The plan's gate is explicit: check
// in a benchmark measuring the CURRENT scalar predicate as the baseline BEFORE the
// predicate changes, and accept the new one only if the regression is within a
// stated budget (≤15%).
//
// This file is that gate, and it stays checked in so a later regression is visible
// rather than discovered. Every candidate predicate is kept and measured side by
// side — including the legacy one, purely as the reference point, since after the
// drop migration there is no other way to reproduce the number the decision rests on.
//
// WHAT THE MEASUREMENT ACTUALLY FOUND (10k queued × 50 runners × 20 agencies,
// 300 claims × 5 runs, ns/op):
//
//	                                        before idx      after idx
//	  LegacyScalar  (the baseline)           4,470,000        336,000
//	  Set           (naive json_each ∩)     19,900,000              —   +350%
//	  Indexed       (correlated join)       18,800,000              —   +320%
//	  Probe         (correlated probe)      13,900,000              —   +210%
//	  InSet         (uncorrelated IN)       13,400,000              —   +200%
//	  Shipped       (both branches cheap)    8,400,000        338,000    ~0%
//
// Two findings, in the order they matter:
//
//  1. **claimRun had no usable index.** EXPLAIN QUERY PLAN showed `SCAN runs` plus
//     `USE TEMP B-TREE FOR ORDER BY` — every poll from every runner examined the
//     entire runs table and sorted it. That scan is the whole of the 4.47 ms
//     baseline, and it is why every candidate predicate looked catastrophic: each
//     was multiplying a scan that should not have been happening. Migration 690's
//     `idx_runs_claimable (status, executor, created_at)` removes it, and the hot
//     path gets ~13× faster for BOTH predicates. This was found by benchmarking the
//     predicate change, not by looking for it.
//  2. **The predicate's cost was concentrated in the GENERAL-POOL branch**, not the
//     membership one. A correlated `NOT EXISTS (… WHERE run_id = runs.id)` runs for
//     every row the first branch rejects — most of the queue in a multi-agency
//     fleet — while the legacy predicate short-circuited on a plain column (`agency
//     IS NULL`). The shipped clause keeps that property by testing the compact-JSON
//     column directly (`agencies_json = '[]'`, a byte comparison, no parsing).
//
// Net: the Phase-3 predicate ships at PARITY with the scalar it replaces (~0.6%,
// inside the noise band and far inside the 15% budget), and the hot path is an
// order of magnitude faster than before this work. The `run_agencies` materialized
// index (T3.3's documented fallback) is what makes the membership branch a keyed
// probe rather than a JSON parse.
//
// RT-1 RE-MEASUREMENT (mig. 1070, the runner-tag pin). The pin adds a fifth
// predicate to this query, so the gate above was re-run before it shipped:
//
//	  LegacyScalar (baseline)   361,052 ns/op
//	  Shipped      (with pin)   332,423 ns/op     flat, inside the noise band
//
// It is free for the case that dominates a real fleet — an UNPINNED run, where
// `runs.runner_tag IS NULL` short-circuits before the projection is touched.
// That short-circuit is the reason the pin probes `runner_tags` rather than
// `json_each(runners.tags)`: the JSON form cannot short-circuit cheaply and
// would land in the +200–350% band mapped above. See RT-G1.
//
//	go test ./internal/runner/ -run '^$' -bench BenchmarkClaimPredicate -benchtime 300x

const (
	benchRuns     = 10000
	benchRunners  = 50
	benchAgencies = 20
)

// benchClaimDB seeds the fixture once per benchmark. Uses a real on-disk DB (not
// :memory:) because the production hot path is on disk and page-cache behavior is
// part of what is being measured.
func benchClaimDB(b *testing.B) (*Service, []string) {
	b.Helper()
	pool, err := db.Open(filepath.Join(b.TempDir(), "claimbench.db"))
	if err != nil {
		b.Fatalf("open db: %v", err)
	}
	b.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	svc := New(pool, &config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Migration 700 dropped runs.agency. The LEGACY baseline predicate needs it, so
	// the fixture recreates it locally — that is the only way the number this
	// decision rests on stays reproducible now that production no longer has the
	// column. Nothing but BenchmarkClaimPredicateLegacyScalar reads it.
	if _, err := pool.Exec(`ALTER TABLE runs ADD COLUMN agency TEXT`); err != nil {
		b.Fatalf("recreate legacy column: %v", err)
	}

	tx, err := pool.Begin()
	if err != nil {
		b.Fatal(err)
	}
	const ts = "2026-01-01T00:00:00Z"
	agencyIDs := make([]string, benchAgencies)
	agencyNames := make([]string, benchAgencies)
	for i := range benchAgencies {
		agencyIDs[i] = fmt.Sprintf("ag-%02d", i)
		agencyNames[i] = fmt.Sprintf("agency-%02d", i)
		if _, err := tx.Exec(`INSERT INTO agencies(id, name, created_at) VALUES(?,?,?)`,
			agencyIDs[i], agencyNames[i], ts); err != nil {
			b.Fatal(err)
		}
	}
	runnerIDs := make([]string, benchRunners)
	caps, _ := json.Marshal([]string{"bash", "ansible"})
	for i := range benchRunners {
		runnerIDs[i] = fmt.Sprintf("rn-%03d", i)
		if _, err := tx.Exec(`
			INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, registered_at, created_at)
			VALUES (?,?, 'online', 'Linux', ?, 0, 5, '1.0', ?, ?, ?)`,
			runnerIDs[i], fmt.Sprintf("runner-%03d", i), string(caps), runnerproto.ProtocolVersion, ts, ts); err != nil {
			b.Fatal(err)
		}
		// Each runner belongs to two agencies — a multi-homed fleet, which is the
		// case the set predicate exists for and the worst case for the intersection.
		for _, k := range []int{i % benchAgencies, (i + 7) % benchAgencies} {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO runner_agencies(runner_id, agency_id) VALUES(?,?)`,
				runnerIDs[i], agencyIDs[k]); err != nil {
				b.Fatal(err)
			}
		}
	}
	for i := range benchRuns {
		ag := agencyNames[i%benchAgencies]
		aj, _ := json.Marshal([]string{ag})
		if _, err := tx.Exec(`
			INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor,
			                 created_at, agency, agencies_json, requires_json)
			VALUES (?, 'bench-job', 'bash', 'queued', 'bench', 'manual', 'runner', ?, ?, ?, '[]')`,
			fmt.Sprintf("run-%06d", i), ts, ag, string(aj)); err != nil {
			b.Fatal(err)
		}
		// The migration-690 materialized index, written in lockstep with the snapshot
		// exactly as the enqueue path does.
		if _, err := tx.Exec(`INSERT INTO run_agencies(run_id, agency) VALUES(?,?)`,
			fmt.Sprintf("run-%06d", i), ag); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	// ANALYZE so the planner sees real selectivity — without it SQLite guesses, and
	// the guess is what a production DB will NOT be running on.
	if _, err := pool.Exec(`ANALYZE`); err != nil {
		b.Fatal(err)
	}
	return svc, runnerIDs
}

// claimSQL builds the full claim statement with one of the two agency predicates
// substituted. Everything else — the capability check, the requires⊆ check, the
// injection gate, the ORDER BY — is byte-identical between the two, so the
// difference the benchmark reports is attributable to the agency clause alone.
func claimSQL(agencyClause string) string {
	return `
		UPDATE runs
		SET status = 'running', runner_id = ?, started_at = ?
		WHERE id = (
			SELECT id FROM runs
			WHERE status = 'queued' AND executor = 'runner'
			  AND run_type IN (SELECT value FROM json_each(?))
			  AND NOT EXISTS (
			    SELECT 1 FROM json_each(COALESCE(runs.requires_json, '[]')) je
			    WHERE je.value NOT IN (SELECT value FROM json_each(?)))
			  AND (` + agencyClause + `)
			  -- RT-1 (mig. 1070). Carried here because this harness is a hand-copy
			  -- of claimRun and the header's promise — that everything outside the
			  -- agency clause is byte-identical to production — is what makes the
			  -- reported difference attributable to the agency clause alone. It is
			  -- also what TestClaimQueryPlan asserts the plan of, so omitting it
			  -- would leave the plan guard watching a query nobody runs.
			  AND (runs.runner_tag IS NULL OR runs.runner_tag = ''
			       OR EXISTS (SELECT 1 FROM runner_tags rt
			                  WHERE rt.runner_id = ? AND rt.tag = runs.runner_tag))
			  AND (
			    ? = 1
			    OR NOT EXISTS (
			      SELECT 1 FROM reference_bindings rb
			      WHERE (rb.owner_kind = 'job'
			              AND rb.owner_source = COALESCE(NULLIF(runs.job_source, ''), 'git')
			              AND rb.owner_name = runs.job_name)
			         OR (rb.owner_kind = 'script' AND rb.owner_name = runs.script_ref))
			  )
			-- Mirrors claimRun's sort (QP). If these drift, the benchmark stops
			-- measuring the query production actually runs.
			ORDER BY priority DESC, created_at ASC
			LIMIT 1
		)
		RETURNING id`
}

// legacyAgencyClause is the pre-Phase-3 scalar predicate — the BASELINE. Kept only
// so the number the ≤15% decision rests on stays reproducible after migration 690
// drops runs.agency.
const legacyAgencyClause = `
			    agency IN (SELECT a.name FROM runner_agencies ra JOIN agencies a ON a.id = ra.agency_id WHERE ra.runner_id = ?)
			    OR (agency IS NULL AND NOT EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = ?))`

// setAgencyClause is the Phase-3 predicate: the run's agency SET intersected with
// the runner's membership. The general-pool branch is deliberately UNCHANGED
// (AG-Q3a) — an untagged run stays claimable only by a runner with no agencies.
const setAgencyClause = `
			    EXISTS (
			      SELECT 1 FROM json_each(COALESCE(runs.agencies_json, '[]')) rj
			      JOIN agencies a         ON a.name = rj.value
			      JOIN runner_agencies ra ON ra.agency_id = a.id AND ra.runner_id = ?)
			    OR (json_array_length(COALESCE(runs.agencies_json, '[]')) = 0
			        AND NOT EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = ?))`

// indexedAgencyClause is what actually SHIPS (T3.3): the same set semantics as
// setAgencyClause, resolved through the run_agencies materialized index (migration
// 690) instead of a per-row json_each. run_id is the leading column of that table's
// composite PK, so the correlated probe is an index seek rather than a JSON parse
// of every candidate row.
const indexedAgencyClause = `
			    EXISTS (
			      SELECT 1 FROM run_agencies rag
			      JOIN agencies a         ON a.name = rag.agency
			      JOIN runner_agencies ra ON ra.agency_id = a.id AND ra.runner_id = ?
			      WHERE rag.run_id = runs.id)
			    OR (NOT EXISTS (SELECT 1 FROM run_agencies WHERE run_id = runs.id)
			        AND NOT EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = ?))`

// probeAgencyClause — the shape that actually matters. The legacy predicate is fast
// not because a scalar compare is cheap but because its subquery is UNCORRELATED:
// SQLite materializes the runner's agency names ONCE per statement into an
// ephemeral index, then does a lookup per candidate row. Both variants above lose
// that — they correlate on runs.id, so their whole join is re-planned and re-run
// for every row in the queue.
//
// This keeps the uncorrelated inner subquery exactly as the legacy predicate had
// it, and adds only a run_agencies probe on the composite PK's leading column.
const probeAgencyClause = `
			    EXISTS (
			      SELECT 1 FROM run_agencies rag
			      WHERE rag.run_id = runs.id
			        AND rag.agency IN (SELECT a.name FROM runner_agencies ra
			                           JOIN agencies a ON a.id = ra.agency_id
			                           WHERE ra.runner_id = ?))
			    OR (NOT EXISTS (SELECT 1 FROM run_agencies WHERE run_id = runs.id)
			        AND NOT EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = ?))`

// inSetAgencyClause — fully UNCORRELATED on both sides. SQLite materializes the
// runner's claimable run-id set once per statement, then each candidate row costs a
// single hash probe, which is the closest structural analogue of the legacy scalar
// compare.
const inSetAgencyClause = `
			    runs.id IN (SELECT rag.run_id FROM run_agencies rag
			                WHERE rag.agency IN (SELECT a.name FROM runner_agencies ra
			                                     JOIN agencies a ON a.id = ra.agency_id
			                                     WHERE ra.runner_id = ?))
			    OR (NOT EXISTS (SELECT 1 FROM run_agencies WHERE run_id = runs.id)
			        AND NOT EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = ?))`

// shippedAgencyClause — the predicate that ships.
//
// The remaining cost in every variant above turned out to be the GENERAL-POOL
// branch, not the membership one: `NOT EXISTS (SELECT … WHERE run_id = runs.id)` is
// correlated, and it runs for every row the first branch rejects — which in a
// multi-agency fleet is most of the queue. The legacy predicate never paid that
// because `agency IS NULL` short-circuits on a column.
//
// So both branches are kept uncorrelated and gated on a COLUMN comparison first:
// agencies_json is always compact JSON from execspec.MarshalAgencies, so the
// general-pool test is the byte comparison `= '[]'` — no JSON parsing, no subquery.
// The membership branch is then the same uncorrelated materialized set the legacy
// predicate used, over the migration-690 index.
const shippedAgencyClause = `
			    (COALESCE(runs.agencies_json, '[]') <> '[]'
			     AND EXISTS (SELECT 1 FROM run_agencies rag
			                 WHERE rag.run_id = runs.id
			                   AND rag.agency IN (SELECT a.name FROM runner_agencies ra
			                                      JOIN agencies a ON a.id = ra.agency_id
			                                      WHERE ra.runner_id = ?)))
			    OR (COALESCE(runs.agencies_json, '[]') = '[]'
			        AND NOT EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = ?))`

func benchmarkClaim(b *testing.B, clause string) {
	svc, runnerIDs := benchClaimDB(b)
	caps, _ := json.Marshal([]string{"bash", "ansible"})
	stmt := claimSQL(clause)
	ctx := context.Background()

	// b.Loop() handles timer reset itself; i is kept because the body rotates
	// through the runner fixtures and names the iteration in its failure message.
	i := 0
	for b.Loop() {
		rid := runnerIDs[i%len(runnerIDs)]
		var claimed string
		err := svc.db.QueryRowContext(ctx, stmt,
			rid, "2026-01-01T00:00:00Z", string(caps), string(caps), rid, rid, rid, 1).Scan(&claimed)
		if err == sql.ErrNoRows {
			b.Fatalf("iteration %d claimed nothing — the fixture must always have a claimable run, "+
				"or the benchmark is measuring the empty case", i)
		}
		if err != nil {
			b.Fatal(err)
		}
		// Restore OUTSIDE the timer: without this the queue drains and late
		// iterations measure a progressively smaller candidate set.
		b.StopTimer()
		if _, err := svc.db.ExecContext(ctx,
			`UPDATE runs SET status='queued', runner_id=NULL, started_at=NULL WHERE id=?`, claimed); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		i++
	}
}

// BenchmarkClaimPredicateLegacyScalar is the T3.1 BASELINE — the predicate as it
// stood before Phase 3. Run it alongside the Set benchmark below; the gate is a p99
// regression within 15%.
func BenchmarkClaimPredicateLegacyScalar(b *testing.B) { benchmarkClaim(b, legacyAgencyClause) }

// BenchmarkClaimPredicateSet is the Phase-3 predicate (AG-Q2b). If this exceeds the
// budget against the baseline above, the documented fallback is to add a
// `run_agencies` join table as a MATERIALIZED INDEX maintained alongside
// agencies_json — semantics unchanged, snapshot discipline unchanged, purely a
// lookup structure (T3.3).
func BenchmarkClaimPredicateSet(b *testing.B) { benchmarkClaim(b, setAgencyClause) }

// BenchmarkClaimPredicateIndexed is the SHIPPED predicate. This is the number that
// has to clear the ≤15% budget against the Legacy baseline.
func BenchmarkClaimPredicateIndexed(b *testing.B) { benchmarkClaim(b, indexedAgencyClause) }

// BenchmarkClaimPredicateProbe is the candidate that keeps the legacy predicate's
// uncorrelated-subquery shape.
func BenchmarkClaimPredicateProbe(b *testing.B) { benchmarkClaim(b, probeAgencyClause) }

// BenchmarkClaimPredicateInSet is the fully-uncorrelated candidate.
func BenchmarkClaimPredicateInSet(b *testing.B) { benchmarkClaim(b, inSetAgencyClause) }

// BenchmarkClaimPredicateShipped is the predicate claimRun actually runs. THIS is
// the number that must clear the ≤15% budget against LegacyScalar.
func BenchmarkClaimPredicateShipped(b *testing.B) { benchmarkClaim(b, shippedAgencyClause) }

// TestClaimPredicatesAgree is the correctness half of the gate: over the SAME
// fixture, the two predicates must select the same run for the same runner. A
// faster predicate that claims a different run is not an optimization, it is a
// dispatch bug — and the benchmark alone would not notice.
func TestClaimPredicatesAgree(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "claimagree.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// See benchClaimDB: the legacy predicate's column is gone from production.
	if _, err := pool.Exec(`ALTER TABLE runs ADD COLUMN agency TEXT`); err != nil {
		t.Fatalf("recreate legacy column: %v", err)
	}
	const ts = "2026-01-01T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	caps, _ := json.Marshal([]string{"bash"})
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-a','A',?)`, ts)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-b','B',?)`, ts)
	exec(`INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, registered_at, created_at)
	      VALUES('rn-a','ra','online','Linux',?,0,5,'1.0',?,?,?)`, string(caps), runnerproto.ProtocolVersion, ts, ts)
	exec(`INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, protocol_version, registered_at, created_at)
	      VALUES('rn-none','rn','online','Linux',?,0,5,'1.0',?,?,?)`, string(caps), runnerproto.ProtocolVersion, ts, ts)
	exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('rn-a','ag-a')`)
	mk := func(id, agency, aj, created string) {
		var a any
		if agency != "" {
			a = agency
		}
		exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor,
		                       created_at, agency, agencies_json, requires_json)
		      VALUES(?, 'j','bash','queued','t','manual','runner',?,?,?,'[]')`, id, created, a, aj)
		if agency != "" {
			exec(`INSERT INTO run_agencies(run_id, agency) VALUES(?,?)`, id, agency)
		}
	}
	mk("r-b", "B", `["B"]`, "2026-01-01T00:00:01Z")   // wrong agency for rn-a
	mk("r-general", "", `[]`, "2026-01-01T00:00:02Z") // general pool
	mk("r-a", "A", `["A"]`, "2026-01-01T00:00:03Z")   // rn-a's agency

	claim := func(clause, runnerID string) string {
		t.Helper()
		var got string
		err := pool.QueryRow(claimSQL(clause), runnerID, ts, string(caps), string(caps), runnerID, runnerID, runnerID, 1).Scan(&got)
		if err == sql.ErrNoRows {
			return ""
		}
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		// Roll back so the next predicate sees the identical fixture.
		exec(`UPDATE runs SET status='queued', runner_id=NULL, started_at=NULL WHERE id=?`, got)
		return got
	}

	for _, runner := range []string{"rn-a", "rn-none"} {
		legacy := claim(legacyAgencyClause, runner)
		set := claim(setAgencyClause, runner)
		indexed := claim(indexedAgencyClause, runner)
		if legacy != set || legacy != indexed {
			t.Errorf("runner %s: legacy claimed %q, set claimed %q, indexed claimed %q — all three must agree",
				runner, legacy, set, indexed)
		}
	}
	// The specific properties that agreement alone would not pin down:
	if got := claim(indexedAgencyClause, "rn-a"); got != "r-a" {
		t.Errorf("agency-bound runner claimed %q, want r-a (never the general-pool or wrong-agency run)", got)
	}
	// AG-Q3a — the disjoint general-pool rule is deliberately UNCHANGED: an
	// agency-bound runner must still refuse untagged work, and only a runner with no
	// agencies may take it.
	if got := claim(indexedAgencyClause, "rn-none"); got != "r-general" {
		t.Errorf("agency-less runner claimed %q, want r-general", got)
	}
}

// TestClaimQueryPlan asserts the PLAN, not just the timing. A benchmark tells you
// something got slow; the plan tells you why, and it is the thing that silently
// regresses when a migration adds a column or an index is dropped. The two
// properties below are worth ~13× on the hot path (see the numbers above), and
// neither is visible in any functional test.
func TestClaimQueryPlan(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "claimplan.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := pool.Query("EXPLAIN QUERY PLAN "+claimSQL(shippedAgencyClause),
		"rn", "t", `["bash"]`, `["bash"]`, "rn", "rn", "rn", 1)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	// A full scan of runs means every poll from every runner reads the whole table.
	if strings.Contains(joined, "SCAN runs") {
		t.Errorf("claim query full-scans runs — idx_runs_claimable is not being used:\n%s", joined)
	}
	// A temp B-tree means the ORDER BY is sorting the entire candidate set instead
	// of walking an index and stopping at the first match.
	if strings.Contains(joined, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("claim query sorts via a temp B-tree — the index no longer covers the ORDER BY:\n%s", joined)
	}
	if !strings.Contains(joined, "idx_runs_claimable") {
		t.Errorf("claim query does not reference idx_runs_claimable:\n%s", joined)
	}
}

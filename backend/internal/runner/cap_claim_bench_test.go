package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runcap"
)

// 2.5.0 C6 — the cost of the concurrency cap at the claim (runcap.ClaimSQL),
// measured against BenchmarkClaimPredicateShipped on the same fixture, as
// every claim clause before it was. Two states: no agency with a cap of its
// own (most installations; the agency's count never runs), and every agency
// capped (the count runs for every candidate).
//
// Measured 2026-10-17 (go test -run XXX -bench 'ClaimPredicateShipped$|ClaimWithCap'
// -benchtime 300x -count 5; 10k queued, 50 runners, 20 agencies; mean ns/op):
//
//	                              correlated (shipped)   uncorrelated list
//	  Shipped (no cap clauses)          389,000               402,000
//	  WithCap, no agency capped         451,000 (+16%)        517,000
//	  WithCap, every agency capped      484,000 (+24%)        484,000
//
// Before the partial index idx_runs_running_agency (1560), every-agency-capped
// was 1,480,000 correlated and 3,760,000 uncorrelated: the agency's count
// walked all its runs through (owner_agency, started_at).

func benchmarkClaimWithCap(b *testing.B, everyAgencyCapped bool) {
	svc, runnerIDs := benchClaimDB(b, false)
	if everyAgencyCapped {
		if _, err := svc.db.Exec(`UPDATE agencies SET max_concurrent = 1000000`); err != nil {
			b.Fatal(err)
		}
	}
	caps, _ := json.Marshal([]string{"bash", "ansible"})
	const sortMark = "-- Mirrors claimRun's sort (QP)."
	stmt := claimSQL(shippedAgencyClause)
	if !strings.Contains(stmt, sortMark) {
		b.Fatal("the harness's claim statement changed: place runcap.ClaimSQL again")
	}
	stmt = strings.Replace(stmt, sortMark, runcap.ClaimSQL+"\n\t\t\t"+sortMark, 1)
	ctx := context.Background()
	i := 0
	for b.Loop() {
		rid := runnerIDs[i%len(runnerIDs)]
		args := append(claimArgs(shippedAgencyClause, rid, "2026-01-01T00:00:00Z", string(caps)), 1000000)
		var claimed string
		err := svc.db.QueryRowContext(ctx, stmt, args...).Scan(&claimed)
		if err == sql.ErrNoRows {
			b.Fatalf("iteration %d claimed nothing", i)
		}
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if _, err := svc.db.ExecContext(ctx,
			`UPDATE runs SET status='queued', runner_id=NULL, started_at=NULL WHERE id=?`, claimed); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		i++
	}
}

func BenchmarkClaimWithCapNoAgencyCapped(b *testing.B) { benchmarkClaimWithCap(b, false) }

func BenchmarkClaimWithCapEveryAgencyCapped(b *testing.B) { benchmarkClaimWithCap(b, true) }

package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// 2.5.0 C6 (question 17, present defect 47): the concurrency cap is asked at
// the claim as well as where a run is produced. A waiting run does not count
// there; running runs do, every agency's against the installation's cap and an
// agency's against its own. Without this, an agent that comes back after its
// agency's runs waited for it would start them all at once, past the cap.
//
// The claim does not ask where a run came from, so a workflow's steps, which
// the engine produces without the producer's question, are held here like
// any other run.

func capFixture(t *testing.T) (*Service, func(id, agencyID, agencyName, status string)) {
	t.Helper()
	svc := newTestService(t)
	insertAgencyRow(t, svc, "a1", "alpha")
	insertAgencyRow(t, svc, "b1", "beta")
	insertRunner(t, svc, "agent-a", "agent-a", "online", []string{"bash"})
	insertRunner(t, svc, "agent-b", "agent-b", "online", []string{"bash"})
	addRunnerAgency(t, svc, "agent-a", "a1")
	addRunnerAgency(t, svc, "agent-b", "b1")
	run := func(id, agencyID, agencyName, status string) {
		t.Helper()
		if _, err := svc.db.Exec(`INSERT INTO runs(id, job_name, job_source, run_type, agencies_json, owner_agency, status, triggered_by, trigger_kind, executor, created_at)
		      VALUES (?, 'j', 'cronomicon', 'bash', json_array(?), ?, ?, 'test', 'manual', 'runner', ?)`, id, agencyName, agencyID, status, now()); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`INSERT OR IGNORE INTO run_agencies(run_id, agency) VALUES(?, ?)`, id, agencyName); err != nil {
			t.Fatal(err)
		}
	}
	return svc, run
}

func claimAs(t *testing.T, svc *Service, runner string) string {
	t.Helper()
	got, err := Claim(context.Background(), svc.db, svc.log, ClaimRequest{RunnerID: runner, Caps: []string{"bash"}, Actor: "runner:" + runner})
	if err != nil {
		t.Fatalf("claim as %s: %v", runner, err)
	}
	if got == nil {
		return ""
	}
	return got.TraceID
}

func TestAnAgentThatComesBackTakesItsBacklogWithinTheCap(t *testing.T) {
	svc, run := capFixture(t)
	if _, err := svc.db.Exec(`INSERT INTO settings (key, value) VALUES ('maxConcurrent', '3')`); err != nil {
		t.Fatal(err)
	}
	// Alpha's runs waited while its agent was away; one of Beta's is running.
	for _, id := range []string{"a-1", "a-2", "a-3", "a-4"} {
		run(id, "a1", "alpha", "queued")
	}
	run("b-running", "b1", "beta", "running")

	// Alpha's agent comes back: it takes runs until three are running in all.
	took := 0
	for claimAs(t, svc, "agent-a") != "" {
		took++
	}
	if took != 2 {
		t.Fatalf("the returning agent took %d runs, want 2 (the cap of 3 less Beta's running one)", took)
	}
	// A slot frees: the next waiting run is taken, and no more.
	if _, err := svc.db.Exec(`UPDATE runs SET status = 'success' WHERE id = 'b-running'`); err != nil {
		t.Fatal(err)
	}
	if claimAs(t, svc, "agent-a") == "" || claimAs(t, svc, "agent-a") != "" {
		t.Errorf("after a slot freed, the agent should take exactly one more run")
	}
}

func TestAnAgencysOwnCapHoldsItsClaimsOnly(t *testing.T) {
	svc, run := capFixture(t)
	if _, err := svc.db.Exec(`UPDATE agencies SET max_concurrent = 1 WHERE id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	run("a-1", "a1", "alpha", "queued")
	run("a-2", "a1", "alpha", "queued")
	run("b-1", "b1", "beta", "queued")

	if claimAs(t, svc, "agent-a") == "" {
		t.Fatalf("alpha's first run was not taken")
	}
	if id := claimAs(t, svc, "agent-a"); id != "" {
		t.Errorf("alpha's agent took %s past alpha's cap of 1", id)
	}
	if claimAs(t, svc, "agent-b") != "b-1" {
		t.Errorf("beta's run was held by alpha's cap")
	}

	// The waiting run says why it waits.
	var waiting string
	if err := svc.db.QueryRow(`SELECT id FROM runs WHERE owner_agency = 'a1' AND status = 'queued'`).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	reason, err := execspec.StampUnclaimableReason(context.Background(), svc.db, waiting)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Waiting for a slot: alpha's concurrency cap (1) is reached by its running runs"; reason != want {
		t.Errorf("the held run's reason = %q, want %q", reason, want)
	}
	// And a runner gate still reads as before: no cap sentence where none holds.
	if r, _ := execspec.UnclaimableReason(context.Background(), svc.db, waiting); r != "" {
		t.Errorf("UnclaimableReason for a run a runner could take = %q, want \"\" (the cap is not a runner gate)", r)
	}
}

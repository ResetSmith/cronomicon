package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// An entry that names a schedule and is tied to none, while a schedule of that
// name is live, is reported to the agency of its job's scope, and the notice
// resolves when the entry is tied (which is what saving the job does). What
// looks the same and is not is left alone: an inline entry, an entry that
// knows its schedule, an entry whose schedule has gone altogether, and the
// entry of a job in the recycle bin.
//
// "Tied to none" has two shapes: no uid, and the uid of a schedule that has
// gone while another schedule holds the name. And the owner may be a workflow,
// or may be known by its name only (an entry with no owner uid).
func TestScheduleBindingAmbiguousIsAnEntryTiedToNoSchedule(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-prod', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	// `nightly` twice (Git's and an in-app one), `solo` once.
	mustExec(t, pool, `INSERT INTO schedules (uid, name, source, cron, content_hash) VALUES
		('s-git', 'nightly', 'git', '0 0 1 * * *', 'h'), ('s-app', 'nightly', 'cronomicon', '0 0 3 * * *', 'h'),
		('s-solo', 'solo', 'git', '0 0 4 * * *', 'h')`)
	job := func(uid, name, scope string, deleted any) {
		mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, deleted_at) VALUES (?, ?, 'cronomicon', 'bash', NULLIF(?, ''), 't', ?)`,
			uid, name, scope, deleted)
	}
	entry := func(owner string, ownerUID any, name string, ref, schedUID any) {
		mustExec(t, pool, `INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
			VALUES ('cronomicon', 'job', ?, ?, '0 0 3 * * *', 0, ?, ?, ?)`, owner, name, ref, ownerUID, schedUID)
	}
	job("j-amb", "ambiguous", "fin-prod", nil)
	entry("ambiguous", "j-amb", "nightly", "nightly", nil) // the condition
	job("j-unscoped", "unscoped", "", nil)
	entry("unscoped", "j-unscoped", "nightly", "nightly", nil) // the condition, with no scope: Global's
	job("j-ok", "tied", "fin-prod", nil)
	entry("tied", "j-ok", "nightly", "nightly", "s-app") // knows its schedule
	entry("tied", "j-ok", "inline", nil, nil)            // an inline entry names no schedule
	entry("tied", "j-ok", "vanished", "vanished", nil)   // its schedule has gone altogether
	job("j-bin", "binned", "fin-prod", "2026-01-01T00:00:00Z")
	entry("binned", "j-bin", "nightly", "nightly", nil) // a binned job fires nothing
	// The second shape: the entry's uid names a schedule that has gone (a Git
	// schedule pruned), and a schedule of that name is live.
	job("j-dead", "orphaned", "fin-prod", nil)
	entry("orphaned", "j-dead", "solo", "solo", "s-pruned")
	// An entry with no owner uid, under a name two jobs share: reported by its
	// name, although no uid says whose it is.
	job("j-t1", "twin", "fin-prod", nil)
	job("j-t2", "twin", "fin-prod", nil)
	entry("twin", nil, "solo", "solo", nil)
	// A workflow's entry.
	mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at) VALUES ('w-1', 'flow', 'cronomicon', '[]', 't')`)
	mustExec(t, pool, `INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
		VALUES ('cronomicon', 'workflow', 'flow', 'nightly', '0 0 3 * * *', 0, 'nightly', 'w-1', NULL)`)

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindScheduleBindingAmbiguous)
	}

	got := check()
	if len(got) != 5 {
		t.Fatalf("open notices = %d (%v), want the five untied entries", len(got), got)
	}
	if d, ok := got["job:j-dead:solo"]; !ok {
		t.Errorf("no notice for the entry whose schedule has gone and has a namesake; got %v", got)
	} else if strings.Contains(d.Detail, "2 schedules") {
		t.Errorf("one schedule holds the name solo, and the detail says two: %s", d.Detail)
	}
	if tw, ok := got["job:cronomicon:twin:solo"]; !ok || tw.AgencyID != "global" {
		t.Errorf("the uid-less entry under a name two jobs share: found %v, agency %q; want it filed under Global", ok, tw.AgencyID)
	}
	if w, ok := got["workflow:w-1:nightly"]; !ok || w.AgencyID != "global" {
		t.Errorf("the workflow's notice: found %v, agency %q; want it filed under Global", ok, w.AgencyID)
	} else if !strings.Contains(w.Detail, "The workflow flow") {
		t.Errorf("the workflow's detail does not name it as a workflow: %s", w.Detail)
	}
	n, ok := got["job:j-amb:nightly"]
	if !ok {
		t.Fatalf("no notice for the ambiguous entry; got %v", got)
	}
	if n.AgencyID != "ag-fin" {
		t.Errorf("the notice is filed under %q, want the job's scope's agency ag-fin", n.AgencyID)
	}
	for _, want := range []string{"ambiguous", "nightly", "2 schedules", "save it"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	if u, ok := got["job:j-unscoped:nightly"]; !ok || u.AgencyID != "global" {
		t.Errorf("the unscoped job's notice: found %v, agency %q; want it filed under Global", ok, u.AgencyID)
	}

	// Saving the job ties the entry (the composer stamps the schedule's uid).
	mustExec(t, pool, `UPDATE definition_schedules SET schedule_uid = 's-app' WHERE owner_uid = 'j-amb'`)
	got = check()
	if len(got) != 4 {
		t.Errorf("open notices after the job was saved = %d (%v), want the other four", len(got), got)
	}
	if _, still := got["job:j-amb:nightly"]; still {
		t.Errorf("the saved job's notice did not resolve")
	}
}

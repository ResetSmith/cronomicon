package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// An in-app definition whose entry records a schedule that has gone, with no
// schedule of that name left, is reported to the agency of its job's scope,
// and the notice resolves when the entry is bound elsewhere or stops naming a
// schedule. What looks the same and is not is left alone: an entry whose
// schedule has a namesake (the other notice's), one that never recorded a
// schedule, one whose schedule is only in the recycle bin, a Git definition's,
// and the entry of a definition in the recycle bin.
func TestScheduleGoneIsAnInAppEntryWhoseScheduleLeft(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-prod', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	mustExec(t, pool, `INSERT INTO schedules (uid, name, source, cron, content_hash, deleted_at) VALUES
		('s-live', 'weekly', 'cronomicon', '0 0 3 * * 1', 'h', NULL),
		('s-binned', 'binned-one', 'cronomicon', '0 0 4 * * *', 'h', '2026-01-01T00:00:00Z')`)
	job := func(uid, name, source, scope string, deleted any) {
		mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, deleted_at) VALUES (?, ?, ?, 'bash', NULLIF(?, ''), 't', ?)`,
			uid, name, source, scope, deleted)
	}
	entry := func(source, kind, owner, ownerUID, name string, ref, schedUID any) {
		mustExec(t, pool, `INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
			VALUES (?, ?, ?, ?, '0 0 2 * * *', 0, ?, ?, ?)`, source, kind, owner, name, ref, ownerUID, schedUID)
	}
	job("j-gone", "left-behind", "cronomicon", "fin-prod", nil)
	entry("cronomicon", "job", "left-behind", "j-gone", "nightly", "nightly", "s-pruned") // the condition
	job("j-free", "unscoped", "cronomicon", "", nil)
	entry("cronomicon", "job", "unscoped", "j-free", "nightly", "nightly", "s-pruned") // the condition, Global's
	mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at) VALUES ('w-1', 'flow', 'cronomicon', '[]', 't')`)
	entry("cronomicon", "workflow", "flow", "w-1", "nightly", "nightly", "s-pruned") // the condition, a workflow

	job("j-name", "has-namesake", "cronomicon", "fin-prod", nil)
	entry("cronomicon", "job", "has-namesake", "j-name", "weekly", "weekly", "s-pruned") // a schedule holds the name: the other notice
	job("j-ok", "tied", "cronomicon", "fin-prod", nil)
	entry("cronomicon", "job", "tied", "j-ok", "weekly", "weekly", "s-live")           // its schedule is there
	entry("cronomicon", "job", "tied", "j-ok", "inline", nil, nil)                     // names no schedule
	entry("cronomicon", "job", "tied", "j-ok", "never", "never-recorded", nil)         // from before a schedule was recorded
	entry("cronomicon", "job", "tied", "j-ok", "binned-one", "binned-one", "s-binned") // its schedule is in the recycle bin
	job("j-git", "from-git", "git", "fin-prod", nil)
	entry("git", "job", "from-git", "j-git", "nightly", "nightly", "s-pruned") // a Git definition: its sync's to settle
	job("j-bin", "binned", "cronomicon", "fin-prod", "2026-01-01T00:00:00Z")
	entry("cronomicon", "job", "binned", "j-bin", "nightly", "nightly", "s-pruned") // a binned job fires nothing

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindScheduleGone)
	}
	got := check()
	if len(got) != 3 {
		t.Fatalf("open notices = %d (%v), want the three entries whose schedule has gone", len(got), got)
	}
	n, ok := got["job:j-gone:nightly"]
	if !ok || n.AgencyID != "ag-fin" {
		t.Fatalf("the scoped job's notice: found %v, agency %q; want it under its scope's agency", ok, n.AgencyID)
	}
	for _, want := range []string{"The job left-behind", "nightly", "removed from its Git repository", "0 0 2 * * *", "timing of its own"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	if u, ok := got["job:j-free:nightly"]; !ok || u.AgencyID != "global" {
		t.Errorf("the unscoped job's notice: found %v, agency %q; want it under Global", ok, u.AgencyID)
	}
	if w, ok := got["workflow:w-1:nightly"]; !ok || w.AgencyID != "global" || !strings.Contains(w.Detail, "The workflow flow") {
		t.Errorf("the workflow's notice: found %v, %+v", ok, w)
	}
	// The other notice has the entry whose schedule's name is held.
	if amb := openOf(t, pool, notices.KindScheduleBindingAmbiguous); len(amb) != 1 {
		t.Errorf("entries reported as tied to no schedule although one holds the name = %d, want the one", len(amb))
	}

	// Bound to another schedule; given a timing of its own; a schedule of the name appears.
	mustExec(t, pool, `UPDATE definition_schedules SET source_ref = 'weekly', schedule_uid = 's-live' WHERE owner_uid = 'j-gone'`)
	mustExec(t, pool, `UPDATE definition_schedules SET source_ref = NULL, schedule_uid = NULL WHERE owner_uid = 'j-free'`)
	got = check()
	if len(got) != 1 {
		t.Errorf("open notices after two were settled = %d (%v), want the workflow's", len(got), got)
	}
	mustExec(t, pool, `INSERT INTO schedules (uid, name, source, cron, content_hash) VALUES ('s-new', 'nightly', 'cronomicon', '0 0 5 * * *', 'h')`)
	if got = check(); len(got) != 0 {
		t.Errorf("open notices once a schedule holds the name again = %v; that is the other notice's now", got)
	}
}

package notices_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// 2.5.3 CA4 (CA-17): what the built-in agency holds from before 2.5.3 is
// listed to global administrators, one finding per kind with its names and
// the way out, and each finding clears when Central holds none of its kind.
func TestCA4WhatCentralHoldsIsListedAndClearsWhenItIsGone(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	leftover := func(ctx context.Context, q interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	}) (bool, error) {
		var left bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM git_repos WHERE id = 'global' AND COALESCE(url, '') <> '')`).Scan(&left)
		return left, err
	}
	notices.SetCentralLeftoverRepoCheck(leftover)
	t.Cleanup(func() { notices.SetCentralLeftoverRepoCheck(nil) })

	// An upgraded database: one of each kind left as Global's (born with no
	// owner, the state 1220 and 1400 left them in).
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `UPDATE git_repos SET url = 'https://git.example.com/old.git' WHERE id = 'global'`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, synced_at) VALUES ('j1', 'left-job', 'cronomicon', 'bash', 't')`)
	mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at, owner_agency) VALUES ('w1', 'left-flow', 'cronomicon', '[]', 't', 'global')`)
	mustExec(t, pool, `INSERT INTO schedules (uid, name, source, cron, content_hash, owner_agency) VALUES ('s1', 'left-sched', 'cronomicon', '0 2 * * *', 'h', 'global')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc1', 'left-hosts', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('sec1', 'LEFT_SECRET', '', 'stored', 't')`)
	mustExec(t, pool, `INSERT INTO env_vars (id, key, value, created_at) VALUES ('ev1', 'LEFT_VAR', 'v', 't')`)
	mustExec(t, pool, `INSERT INTO ssh_credentials (id, label, source, created_at, last_modified_at) VALUES ('k1', 'left_key', 'stored', 't', 't')`)
	mustExec(t, pool, `INSERT INTO ssh_hosts (id, hostname, created_at) VALUES ('h1', 'left-host', 't')`)
	mustExec(t, pool, `INSERT INTO bastions (id, name, hostname, created_at) VALUES ('b1', 'left-jump', 'jump.example', 't')`)
	mustExec(t, pool, `INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at, owner_agency)
	                   VALUES ('rn1', 'left-agent', 'agent', 'offline', 'Linux', '["bash"]', 0, 1, '1', 't', 't', 'global')`)
	// Not held (the review of CA4): a host record imported for an agency's
	// scope keeps the owner column's default, and is the scope's (1230); a job
	// in the recycle bin is deleted.
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	mustExec(t, pool, `INSERT INTO ssh_hosts (id, scope_id, hostname, created_at) VALUES ('h-imp', 'sc-fin', 'imported-01', 't')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, synced_at, deleted_at) VALUES ('j-bin', 'binned-job', 'cronomicon', 'bash', 't', 't')`)

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(ctx, pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindCentralHoldsWork)
	}
	got := check()
	for subject, says := range map[string][]string{
		"central:repository":     {"remove the leftover", "connect the same", "a job with no scope"},
		"central:job":            {"left-job", "Delete forever", "then make it again"},
		"central:workflow":       {"left-flow", "Delete forever", "then make it again"},
		"central:schedule":       {"left-sched"},
		"central:scope":          {"left-hosts", "a repair"},
		"central:secret":         {"LEFT_SECRET", "Repair: move to another agency"},
		"central:env-var":        {"LEFT_VAR"},
		"central:ssh-credential": {"left_key"},
		"central:host":           {"left-host"},
		"central:bastion":        {"left-jump"},
		"central:runner":         {"left-agent", "registration token minted in its agency"},
	} {
		n, ok := got[subject]
		if !ok {
			t.Errorf("no finding %s; open: %v", subject, got)
			continue
		}
		if n.AgencyID != "global" {
			t.Errorf("%s is filed under %q, want the built-in agency", subject, n.AgencyID)
		}
		for _, w := range says {
			if !strings.Contains(n.Detail, w) {
				t.Errorf("%s does not say %q: %s", subject, w, n.Detail)
			}
		}
	}
	if strings.Contains(got["central:host"].Detail, "imported-01") || strings.Contains(got["central:job"].Detail, "binned-job") {
		t.Errorf("an imported host record or a deleted job is listed: %s / %s", got["central:host"].Detail, got["central:job"].Detail)
	}

	// A check run with no reader of the leftover leaves its finding as it was.
	notices.SetCentralLeftoverRepoCheck(nil)
	if _, ok := check()["central:repository"]; !ok {
		t.Errorf("the repository finding was resolved by a check that could not ask")
	}
	notices.SetCentralLeftoverRepoCheck(leftover)

	// Moved out, deleted (to the recycle bin, as the app deletes), or
	// re-enrolled: each finding clears with its kind.
	mustExec(t, pool, `UPDATE git_repos SET url = '' WHERE id = 'global'`)
	mustExec(t, pool, `UPDATE jobs SET deleted_at = 't' WHERE uid = 'j1'`)
	mustExec(t, pool, `UPDATE workflows SET deleted_at = 't' WHERE uid = 'w1'`)
	mustExec(t, pool, `UPDATE schedules SET deleted_at = 't' WHERE uid = 's1'`)
	mustExec(t, pool, `DELETE FROM scope_agencies WHERE scope_id = 'sc1'`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc1', 'ag-fin')`)
	for _, q := range []string{
		`UPDATE secrets SET owner_agency = 'ag-fin' WHERE id = 'sec1'`,
		`UPDATE env_vars SET owner_agency = 'ag-fin' WHERE id = 'ev1'`,
		`UPDATE ssh_credentials SET owner_agency = 'ag-fin' WHERE id = 'k1'`,
		`UPDATE ssh_hosts SET owner_agency = 'ag-fin' WHERE id = 'h1'`,
		`UPDATE bastions SET owner_agency = 'ag-fin' WHERE id = 'b1'`,
		`DELETE FROM runners WHERE id = 'rn1'`,
	} {
		mustExec(t, pool, q)
	}
	if got := check(); len(got) != 0 {
		t.Errorf("after everything left Central: %v, want none open", got)
	}
}

// The notices CA-18 retires clear at the next check, and shared_ownership
// keeps its findings to the agencies a Central row lists (the inventory has
// the row itself).
func TestCA4RetiredNoticesClear(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	for _, kind := range []string{notices.KindGlobalJobOnAgencyScope, notices.KindGlobalCalendarScope} {
		if err := notices.Reconcile(ctx, pool, kind, []notices.Finding{{AgencyID: "global", Subject: "x", Detail: "left from before"}}); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('sec1', 'SHARED', '', 'stored', 't')`)
	mustExec(t, pool, `INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('sec1', 'ag-fin')`)
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{notices.KindGlobalJobOnAgencyScope, notices.KindGlobalCalendarScope} {
		if got := openOf(t, pool, kind); len(got) != 0 {
			t.Errorf("%s still open after the check: %v", kind, got)
		}
	}
	shared := openOf(t, pool, notices.KindSharedOwnership)
	if _, ok := shared["secret:sec1"]; ok {
		t.Errorf("shared_ownership still files the Central row under Central (central_holds_work lists it): %v", shared)
	}
	if n, ok := shared["secret:sec1@ag-fin"]; !ok || n.AgencyID != "ag-fin" {
		t.Errorf("shared_ownership no longer tells Finance its former secret is held: %v", shared)
	}
}

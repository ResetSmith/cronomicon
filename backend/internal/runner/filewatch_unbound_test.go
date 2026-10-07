package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// RA-24, the file-arrival half: a watched job with no scope fires as Global's
// run, which resolves Global's secrets and no department's. An arrival for a
// job that declares a department's secret is refused with the reason on the
// sighting — "the file landed and nothing happened" must stay answerable.
//
// The guard was "no scope AND no agencies". An unbound run carries ["Global"]
// since migration 1220, so the second half was never true again.

func seedUnboundWatch(t *testing.T, svc *Service, secretKey, owner string) sightingIn {
	t.Helper()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)
	insertAgencyRow(t, svc, "ag-tax", "Tax")
	exec(`INSERT INTO secrets (id, key, source, owner_agency, created_at) VALUES ('s1', ?, 'stored', ?, 't')`, secretKey, owner)
	if owner != "" {
		exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s1', ?)`, owner)
	}
	if err := runref.ReplaceBindings(context.Background(), svc.db,
		runref.Owner{Kind: "job", Source: "git", Name: "ingest", UID: "uid-ingest"},
		[]runref.Binding{{Kind: runref.KindSecret, Name: secretKey}}, "t"); err != nil {
		t.Fatalf("bind secret: %v", err)
	}
	return sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv",
		SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}
}

func TestArrivalForAnUnboundJobThatNeedsADepartmentsSecretIsRefused(t *testing.T) {
	svc := newTestService(t)
	sg := seedUnboundWatch(t, svc, "DEPT_PASSWORD", "ag-tax")

	ok, err := svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg))
	if ok || err != nil {
		t.Fatalf("an arrival whose run could not resolve its secret fired (%v, %v)", ok, err)
	}
	if got := sightingReason(t, svc, sg.Path); got != runref.QueuedReasonUnboundReferences {
		t.Errorf("refused_reason = %q, want %q", got, runref.QueuedReasonUnboundReferences)
	}
	if n := runCountFor(t, svc, "ingest"); n != 0 {
		t.Errorf("%d runs were enqueued for a refused arrival", n)
	}
}

// The control: bound to a secret that is Global's, the same arrival fires, and
// the run is Global's.
func TestArrivalForAnUnboundJobWithAGlobalSecretFires(t *testing.T) {
	svc := newTestService(t)
	sg := seedUnboundWatch(t, svc, "SHARED_TOKEN", "")

	ok, err := svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg))
	if !ok || err != nil {
		t.Fatalf("did not fire: %v %v (reason %q)", ok, err, sightingReason(t, svc, sg.Path))
	}
	var agencies string
	if err := svc.db.QueryRow(`SELECT agencies_json FROM runs WHERE job_name = 'ingest'`).Scan(&agencies); err != nil || agencies != `["Global"]` {
		t.Errorf("the arrival's run carries %q (%v), want [\"Global\"]", agencies, err)
	}
}

package runref

import (
	"context"
	"testing"
)

// 2.5.1 SG2.2: the dispatch audit records the ROWS a run used, with how the
// run came to carry each, the run's job and runner; a variable is not
// recorded; and the injection's change_log row is written with them.
func TestAuditInjectionRecordsUses(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, runner_name)
		VALUES('run-u','deploy','job-uid-1','ansible','running','ops@x','manual','2026-01-01T00:00:00Z','fin-runner')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	resolved := &Resolved{Refs: []ResolvedRef{
		{Kind: KindSecret, Name: "DB_PASS", Source: "stored", RowID: "sec-1", Owner: "ag-fin"},
		{Kind: KindSecret, Name: "BECOME", Source: "stored", RowID: "sec-2", Owner: "ag-fin", Origin: OriginBecome},
		{Kind: KindSecret, Name: "EXTRA", Source: "vault", RowID: "sec-3", Owner: "ag-fin", Origin: OriginOverride},
		{Kind: KindKey, Name: "deploy-key", Source: "stored", RowID: "key-1", Owner: "", Origin: OriginConnectAs},
		{Kind: KindVar, Name: "REGION", Source: "stored"},
	}}
	if err := AuditInjectionOnce(ctx, pool, nilLog(), "run-u", "ops@x", "fin-prod", resolved); err != nil {
		t.Fatalf("audit: %v", err)
	}
	rows, err := pool.Query(`SELECT kind, row_id, owner_agency, via, ref_name, job_uid, job_name, COALESCE(runner_id, ''), runner_name
		FROM secret_uses WHERE trace_id='run-u' ORDER BY row_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type use struct{ kind, row, owner, via, name, juid, jname, rid, rname string }
	var got []use
	for rows.Next() {
		var u use
		if err := rows.Scan(&u.kind, &u.row, &u.owner, &u.via, &u.name, &u.juid, &u.jname, &u.rid, &u.rname); err != nil {
			t.Fatal(err)
		}
		got = append(got, u)
	}
	want := []use{
		{"key", "key-1", "global", "connect_as", "deploy-key", "job-uid-1", "deploy", "", "fin-runner"},
		{"secret", "sec-1", "ag-fin", "binding", "DB_PASS", "job-uid-1", "deploy", "", "fin-runner"},
		{"secret", "sec-2", "ag-fin", "become", "BECOME", "job-uid-1", "deploy", "", "fin-runner"},
		{"secret", "sec-3", "ag-fin", "override", "EXTRA", "job-uid-1", "deploy", "", "fin-runner"},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d uses, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("use %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The repair path writes the uses again: no duplicate.
	if _, err := pool.Exec(`DELETE FROM change_log WHERE target='run-u'`); err != nil {
		t.Fatal(err)
	}
	if err := AuditInjectionOnce(ctx, pool, nilLog(), "run-u", "ops@x", "fin-prod", resolved); err != nil {
		t.Fatalf("repair: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_uses WHERE trace_id='run-u'`).Scan(&n)
	if n != 4 {
		t.Errorf("after the repair %d uses, want 4", n)
	}
}

// A failure to record the uses fails the audit closed: the slot is released
// and no change_log row claims an injection the uses do not show.
func TestAuditInjectionFailsClosedWhenUsesCannotBeRecorded(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
		VALUES('run-f','j','bash','running','ops@x','manual','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`DROP TABLE secret_uses`); err != nil {
		t.Fatal(err)
	}
	resolved := &Resolved{Refs: []ResolvedRef{{Kind: KindSecret, Name: "DB_PASS", Source: "stored", RowID: "sec-1", Owner: "ag-fin"}}}
	if err := AuditInjectionOnce(ctx, pool, nilLog(), "run-f", "ops@x", "", resolved); err == nil {
		t.Fatal("the audit succeeded with no secret_uses table")
	}
	var audited bool
	_ = pool.QueryRow(`SELECT injection_audited FROM runs WHERE id='run-f'`).Scan(&audited)
	if audited {
		t.Error("the slot stayed claimed")
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE target='run-f'`).Scan(&n)
	if n != 0 {
		t.Errorf("%d change_log row(s) written without the uses", n)
	}
}

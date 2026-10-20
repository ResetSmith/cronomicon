package api_test

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
)

// TestC12_AScheduleEditRewritesOnlyItsOwnDefinitions (present defect 99,
// question 119): a reusable schedule's edit, delete or restore resets the
// legacy `schedule` column of the definitions bound to it. It read and wrote
// by (source, name), so FIN editing its `nightly` rewrote TAX's `backup` row
// too, from whichever agency's entry came first. It is by the owner's uid.
func TestC12_AScheduleEditRewritesOnlyItsOwnDefinitions(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, schedule, created_at) VALUES
	        ('j-mf', 'backup', 'cronomicon', 'bash', 'fin-hosts', 'ag:FIN', 'true', 1, '0 3 * * *', '2026-01-01T00:00:00Z'),
	        ('j-mt', 'backup', 'cronomicon', 'bash', 'tax-hosts', 'ag:TAX', 'true', 1, '0 5 * * *', '2026-01-01T00:00:00Z')`)

	if c := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/schedule-defs", gFinAdmin,
		`{"name":"c12-nightly","cron":"0 3 * * *"}`, agencyctx.Header, "ag:FIN").Code; c != http.StatusCreated {
		t.Fatalf("FIN's administrator creating FIN's schedule = %d", c)
	}
	var schedUID string
	if err := pool.QueryRow(`SELECT uid FROM schedules WHERE name = 'c12-nightly' AND owner_agency = 'ag:FIN'`).Scan(&schedUID); err != nil {
		t.Fatal(err)
	}
	// FIN's `backup` is bound to FIN's schedule; TAX's has an entry of its own.
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid) VALUES
	        ('cronomicon', 'job', 'backup', 'c12-nightly', '0 3 * * *', 0, 'c12-nightly', 'j-mf', ?),
	        ('cronomicon', 'job', 'backup', 'early', '0 5 * * *', 0, NULL, 'j-mt', NULL)`, schedUID)

	if rec := gateReqWithHeader(t, h, http.MethodPut, "/api/v1/schedule-defs/c12-nightly", gFinAdmin,
		`{"cron":"0 4 * * *"}`, agencyctx.Header, "ag:FIN"); rec.Code != http.StatusOK {
		t.Fatalf("FIN's administrator editing FIN's schedule = %d %s", rec.Code, rec.Body)
	}
	mirror := func(uid string) string {
		var s sql.NullString
		if err := pool.QueryRow(`SELECT schedule FROM jobs WHERE uid = ?`, uid).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	if got := mirror("j-mf"); got != "0 4 * * *" {
		t.Errorf("FIN's `backup` after FIN's edit: schedule %q, want its new cron", got)
	}
	if got := mirror("j-mt"); got != "0 5 * * *" {
		t.Errorf("TAX's `backup` after FIN's edit: schedule %q, want its own, untouched", got)
	}
}

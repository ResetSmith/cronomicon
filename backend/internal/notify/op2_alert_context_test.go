package notify

import (
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/settings"
)

// 2.5.2 OP2 — an alert says whose the run is and links to it (OP-5, OP-6), and
// falls back to the agency's contact when the job names none (OP-5a). Driven
// through the real dispatch and captured at the transport, like AN-4's tests.

// setAlertLink stores the address alerts link to, as Settings → Notifications
// does ("" is cleared: no links).
func setAlertLink(t *testing.T, e annEnv, address string) {
	t.Helper()
	if _, err := settings.SetAlertLinkAddress(t.Context(), e.pool, address, "root@example.gov"); err != nil {
		t.Fatalf("set the alert link address: %v", err)
	}
}

func newOP2Env(t *testing.T) annEnv {
	t.Helper()
	e := newAnnEnv(t)
	setAlertLink(t, e, "https://cron.example.gov")
	e.exec(`INSERT INTO agencies (id, name, created_at, contact) VALUES ('ag-pay', 'Payroll', 'now', 'Payroll desk, ext. 4410')`)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, owner_agency, synced_at) VALUES('export','git','uid-export','bash','ag-pay','t')`)
	e.exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at, owner_agency)
	        VALUES('t-pay','export','git','uid-export','bash','failure','t','manual','t','ag-pay')`)
	e.exec(`INSERT INTO alert_config (id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
	        VALUES ('r-secret','all',NULL,'secret-changed','["email"]','ops@example.com',1,'now')`)
	return e
}

func TestOP2AFailureNamesItsAgencyAndLinksToTheRun(t *testing.T) {
	e := newOP2Env(t)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "t-pay", JobName: "export", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if *e.subject != "[Cronomicon] export FAILED" {
		t.Errorf("subject %q: mail filters key on the old shape (AN-Q4)", *e.subject)
	}
	for _, want := range []string{
		"Job: export\nAgency: Payroll\nStatus: failure\n",
		"Run:    t-pay\nOpen:   https://cron.example.gov/runs?agency=ag-pay&trace=t-pay\n",
	} {
		if !strings.Contains(*e.body, want) {
			t.Errorf("body missing %q:\n%s", want, *e.body)
		}
	}
}

func TestOP2AWorkflowRunLinksToTheWorkflowRunsTab(t *testing.T) {
	e := newOP2Env(t)
	e.exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at, agency_id)
	        VALUES ('wr-pay', 'month-end', 'git', 'failure', 't', 'manual', 't', 'ag-pay')`)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "wr-pay", JobName: "month-end", OwnerKind: "workflow", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*e.body, "Agency: Payroll\n") || !strings.Contains(*e.body, "Open:   https://cron.example.gov/runs?agency=ag-pay&tab=workflow-runs&trace=wr-pay\n") {
		t.Errorf("workflow failure body:\n%s", *e.body)
	}
}

func TestOP2NoLinkWithoutTheAddressOrARun(t *testing.T) {
	e := newOP2Env(t)
	setAlertLink(t, e, "")
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "t-pay", JobName: "export", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*e.body, "Open:") || !strings.Contains(*e.body, "Agency: Payroll") {
		t.Errorf("without an alert link address: want the agency and no link:\n%s", *e.body)
	}

	// A suppression did not run: the agency, and nothing to open.
	setAlertLink(t, e, "https://cron.example.gov")
	e.exec(`INSERT INTO alert_config (id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
	        VALUES ('r-skip','all',NULL,'skipped','["email"]','ops@example.com',1,'now')`)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "t-pay", JobName: "export", Status: "skipped", Reason: "Skipped: the job is paused"})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*e.body, "Open:") || !strings.Contains(*e.body, "Agency: Payroll") {
		t.Errorf("a suppression: want the agency and no link:\n%s", *e.body)
	}

	// A missed run has no trace: the agency (by the job), no link.
	if err := e.d.dispatch(t.Context(), alertDispatchEvent(AlertEvent{Trigger: TriggerMissedRun, JobName: "export", JobUID: "uid-export"})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*e.body, "Open:") || !strings.Contains(*e.body, "Agency: Payroll") {
		t.Errorf("a missed run: want the agency and no link:\n%s", *e.body)
	}
}

// OP2.1: with no address stored at all (nobody has opened the app yet), an
// alert carries no link, and nothing else is missing.
func TestOP2NoLinkBeforeAnyAddressIsKnown(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, created_at)
	        VALUES('t-1','export','git','bash','failure','t','manual','t')`)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "t-1", JobName: "export", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*e.body, "Open:") || !strings.Contains(*e.body, "Agency: Global") {
		t.Errorf("no address stored: want the agency and no link:\n%s", *e.body)
	}
}

func TestOP2ARenamedAgencyIsNamedAsItIsNow(t *testing.T) {
	e := newOP2Env(t)
	e.exec(`UPDATE agencies SET name = 'Payroll and Benefits' WHERE id = 'ag-pay'`)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "t-pay", JobName: "export", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*e.body, "Agency: Payroll and Benefits\n") {
		t.Errorf("body:\n%s", *e.body)
	}
}

func TestOP2TheAgencysContactIsTheFallback(t *testing.T) {
	e := newOP2Env(t)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "t-pay", JobName: "export", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*e.body, "Contact:  Payroll desk, ext. 4410 (agency)\n") {
		t.Errorf("an unannotated job: want the agency's contact, marked:\n%s", *e.body)
	}

	// A missed run (no trace) is about the job too.
	if err := e.d.dispatch(t.Context(), alertDispatchEvent(AlertEvent{Trigger: TriggerMissedRun, JobName: "export", JobUID: "uid-export"})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*e.body, "(agency)") {
		t.Errorf("a missed run: want the agency's contact:\n%s", *e.body)
	}

	// The job's own contact comes first, and the agency's is not added.
	annotate(t, e, "job", "uid-export", "export-owner@example.gov", false)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{TraceID: "t-pay", JobName: "export", Status: "failure"})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*e.body, "Contact:  export-owner@example.gov\n") || strings.Contains(*e.body, "(agency)") {
		t.Errorf("an annotated job: want its own contact alone:\n%s", *e.body)
	}

	// An alert that names no job (a secret's) is about the agency itself: no
	// contact line, but its agency named.
	if err := e.d.dispatch(t.Context(), alertDispatchEvent(AlertEvent{Trigger: TriggerSecretChanged, OwnerKind: "secret", Agency: "ag-pay", Subject: "s", Detail: "Secret X of Payroll was changed."})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*e.body, "Contact:") || !strings.Contains(*e.body, "Agency: Payroll\n") {
		t.Errorf("a secret's alert: want the agency named and no contact:\n%s", *e.body)
	}
}

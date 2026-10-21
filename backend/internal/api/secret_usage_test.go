package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 2.5.1 SG2.6 (SG-16): a secret Global holds that lists FIN (shared before
// 2.3.0) is FIN's to see, and its Usage; not TAX's, which it does not list.
func TestHeldByGlobal(t *testing.T) {
	h, pool := gateServer(t)
	for _, q := range []string{
		`INSERT INTO secrets (id, key, scope, source, created_by, created_at, last_modified_by, last_modified_at) VALUES ('s-held', 'OLD_SHARED', 'fin-hosts', 'stored', 'old@x', '2026-01-01T00:00:00Z', 'old@x', '2026-01-01T00:00:00Z')`,
		`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s-held', 'ag:FIN')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	var owner string
	if err := pool.QueryRow(`SELECT owner_agency FROM secrets WHERE id='s-held'`).Scan(&owner); err != nil || owner != "global" {
		t.Fatalf("precondition: the row is Global's (owner %q, %v)", owner, err)
	}

	rec := gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/held-by-global", gFinAdmin, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"OLD_SHARED"`) {
		t.Fatalf("FIN's held-by-global = %d %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:TAX/held-by-global", gTaxAdmin, ""); strings.Contains(rec.Body.String(), "OLD_SHARED") {
		t.Errorf("TAX's list names FIN's held row: %s", rec.Body)
	}
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/env-secrets/s-held/usage", gFinViewer, ""); rec.Code != http.StatusOK {
		t.Errorf("FIN's viewer reading the held row's usage = %d %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/env-secrets/s-held/usage", gTaxAdmin, ""); rec.Code != http.StatusNotFound {
		t.Errorf("TAX reading it = %d, want 404", rec.Code)
	}
	// The exception is the usage and the list only: the secret itself is
	// still not on FIN's secrets list (it is Global's).
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/env-secrets", gFinAdmin, ""); strings.Contains(rec.Body.String(), "OLD_SHARED") {
		t.Errorf("the held row is on FIN's secrets list: %s", rec.Body)
	}
}

// 2.5.1 SG2.5: an agency's secrets report — its own rows only, the uses and
// reveals counted, a global administrator's reveal counted as an outside
// change; CSV with a header row; another agency's administrator gets 404.
func TestSecretsReport(t *testing.T) {
	h, pool := gateServer(t)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"f"}`)
	var c struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	_ = gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gTaxAdmin, `{"key":"TAX_TOKEN","source":"stored","scope":"tax-hosts","value":"t"}`)
	if r := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets/"+c.ID+"/reveal", gRoot, ""); r.Code != http.StatusOK {
		t.Fatalf("reveal = %d", r.Code)
	}
	if _, err := pool.Exec(`INSERT INTO secret_uses (at, trace_id, kind, row_id, owner_agency, via, ref_name) VALUES (?, 'run-1', 'secret', ?, 'ag:FIN', 'binding', 'FIN_TOKEN')`,
		time.Now().UTC().Format(time.RFC3339), c.ID); err != nil {
		t.Fatal(err)
	}

	rec = gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/secrets-report", gFinAdmin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("FIN's report = %d %s", rec.Code, rec.Body)
	}
	var rep struct {
		Items []struct {
			Kind, Name, LastOutsideActor              string
			Uses, Reveals, OutsideChanges, References int
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if len(rep.Items) != 1 || rep.Items[0].Name != "FIN_TOKEN" {
		t.Fatalf("FIN's report holds %+v", rep.Items)
	}
	it := rep.Items[0]
	if it.Uses != 1 || it.Reveals != 1 || it.OutsideChanges != 1 || it.LastOutsideActor == "" {
		t.Errorf("FIN_TOKEN's row %+v", it)
	}
	rec = gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/secrets-report?format=csv", gFinAdmin, "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "kind,id,name,scope,source,vault_path") || !strings.Contains(rec.Body.String(), "FIN_TOKEN") {
		t.Errorf("CSV = %d %q", rec.Code, rec.Body.String())
	}
	if r := gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/secrets-report", gTaxAdmin, ""); r.Code != http.StatusNotFound {
		t.Errorf("TAX reading FIN's report = %d, want 404", r.Code)
	}
	if r := gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/secrets-report?format=xml", gFinAdmin, ""); r.Code != http.StatusBadRequest {
		t.Errorf("format=xml = %d, want 400", r.Code)
	}
}

// 2.5.1 SG2.4: a secret's Usage lists only what would resolve to THIS row —
// FIN's job that binds FIN_TOKEN, not TAX's job binding its own FIN_TOKEN —
// with the runs that used it and its changes; another agency's administrator
// gets 404.
func TestSecretUsage(t *testing.T) {
	h, pool := gateServer(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	create := func(group, body string) string {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", group, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body)
		}
		var c struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &c)
		return c.ID
	}
	fin := create(gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"f"}`)
	tax := create(gTaxAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"tax-hosts","value":"t"}`)
	const now = "2026-10-21T00:00:00Z"
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, created_at) VALUES
		('job-fin','fin-deploy','cronomicon','bash','fin-hosts','ag:FIN','true',?),
		('job-tax','tax-deploy','cronomicon','bash','tax-hosts','ag:TAX','true',?),
		('job-fin-become','fin-play','cronomicon','ansible','fin-hosts','ag:FIN','true',?)`, now, now, now)
	exec(`UPDATE jobs SET become_password_secret='FIN_TOKEN' WHERE uid='job-fin-become'`)
	exec(`INSERT INTO reference_bindings (owner_kind, owner_name, owner_source, owner_uid, ref_kind, ref_name, created_at) VALUES
		('job','fin-deploy','cronomicon','job-fin','secret','FIN_TOKEN',?),
		('job','tax-deploy','cronomicon','job-tax','secret','FIN_TOKEN',?)`, now, now)
	exec(`INSERT INTO secret_uses (at, trace_id, kind, row_id, owner_agency, via, ref_name, job_uid, job_name, runner_name) VALUES
		(?, 'run-1', 'secret', ?, 'ag:FIN', 'binding', 'FIN_TOKEN', 'job-fin', 'fin-deploy', 'fin-runner')`, time.Now().UTC().Format(time.RFC3339), fin)

	// A use recorded while another agency owned the row (before a move) is
	// that agency's history, not FIN's (2.5.1 review).
	exec(`INSERT INTO secret_uses (at, trace_id, kind, row_id, owner_agency, via, ref_name, job_name) VALUES
		(?, 'run-tax', 'secret', ?, 'ag:TAX', 'binding', 'FIN_TOKEN', 'tax-secret-job')`, time.Now().UTC().Format(time.RFC3339), fin)

	rec := gateReq(t, h, http.MethodGet, "/api/v1/env-secrets/"+fin+"/usage", gFinAdmin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("FIN reading its secret's usage = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "tax-secret-job") || strings.Contains(rec.Body.String(), "run-tax") {
		t.Errorf("FIN sees a use recorded under TAX: %s", rec.Body)
	}
	var u struct {
		Owner      string
		References []struct{ Via, OwnerKind, OwnerID string }
		Uses       struct{ Days7 int }
		Recent     []struct{ TraceID, JobName, RunnerName string }
		Changes    []struct{ Action, ActorRelation string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, r := range u.References {
		refs[r.OwnerID] = r.Via
	}
	if len(refs) != 2 || refs["job-fin"] != "binding" || refs["job-fin-become"] != "become" {
		t.Errorf("references %+v, want FIN's binding and become password only", u.References)
	}
	if u.Uses.Days7 != 1 || len(u.Recent) != 1 || u.Recent[0].RunnerName != "fin-runner" {
		t.Errorf("uses %+v recent %+v", u.Uses, u.Recent)
	}
	if len(u.Changes) == 0 || u.Changes[0].Action != "created" || u.Changes[0].ActorRelation != "member" {
		t.Errorf("changes %+v", u.Changes)
	}

	// TAX's own row of the same name has TAX's job only.
	rec = gateReq(t, h, http.MethodGet, "/api/v1/env-secrets/"+tax+"/usage", gTaxAdmin, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &u)
	if rec.Code != http.StatusOK || len(u.References) != 1 || u.References[0].OwnerID != "job-tax" {
		t.Errorf("TAX's usage = %d %+v", rec.Code, u.References)
	}
	// Another agency's administrator, and a viewer of another agency: 404.
	for _, g := range []string{gTaxAdmin, gFinViewer} {
		want := http.StatusNotFound
		if g == gFinViewer {
			want = http.StatusOK // FIN's own viewer reads FIN's secret's usage
		}
		if rec := gateReq(t, h, http.MethodGet, "/api/v1/env-secrets/"+fin+"/usage", g, ""); rec.Code != want {
			t.Errorf("%s reading FIN's secret's usage = %d, want %d", g, rec.Code, want)
		}
	}
}

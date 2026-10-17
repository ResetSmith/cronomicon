package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 2.5.0 C2 (CF-1, question 77): the lists switcherLists does not pin answer by
// the same rule: an agency's rows to its own people and those who reach every
// agency, Global's only to the latter. Each leaked another agency's rows before
// C2 (C0's route table): a workflow's schedule entries and a no-scope job's,
// with their plaintext env; a workflow's reactions; the agency matrix's and the
// membership lists' every scope, key and runner.
func TestC2_TheListsOutsideTheSwitcherPinAnswerByAgency(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, owner_uid, name, cron, env, position) VALUES
	        ('cronomicon', 'workflow', 's0-fin-flow',    'w-fin',  'n', '0 1 * * *', '{"S0_FIN_ENV":"1"}',  0),
	        ('cronomicon', 'workflow', 's0-tax-flow',    'w-tax',  'n', '0 2 * * *', '{"S0_TAX_ENV":"1"}',  0),
	        ('cronomicon', 'workflow', 's0-glob-flow',   'w-glob', 'n', '0 3 * * *', '{"S0_GLOB_ENV":"1"}', 0),
	        ('cronomicon', 'job',      's0-fin-noscope', 'j-fin0', 'n', '0 4 * * *', '{"S0_FIN0_ENV":"1"}', 0)`)
	exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
	                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, position, owner_uid, on_uid) VALUES
	        ('cronomicon', 'workflow', 's0-fin-flow',  's0-fin-react',  'cronomicon', 'job', 's0-fin-job', 'success', 0, 0, 0, 1, 0, 'w-fin',  'j-fin'),
	        ('cronomicon', 'workflow', 's0-tax-flow',  's0-tax-react',  'cronomicon', 'job', 's0-tax-job', 'success', 0, 0, 0, 1, 0, 'w-tax',  'j-tax'),
	        ('cronomicon', 'workflow', 's0-glob-flow', 's0-glob-react', 'cronomicon', 'job', 's0-glob-job', 'success', 0, 0, 0, 1, 0, 'w-glob', 'j-glob')`)

	sees := func(path, groups string, markers [3]string) string {
		t.Helper()
		r := gateReq(t, h, http.MethodGet, path, groups, "")
		if r.Code != http.StatusOK {
			return http.StatusText(r.Code)
		}
		out := ""
		for _, m := range markers {
			if strings.Contains(r.Body.String(), m) {
				out += "Y"
			} else {
				out += "."
			}
		}
		return out
	}
	lists := []struct {
		path    string
		markers [3]string // FIN's, TAX's, Global's
	}{
		{"/api/v1/schedules", [3]string{"S0_FIN_ENV", "S0_TAX_ENV", "S0_GLOB_ENV"}},
		{"/api/v1/schedules", [3]string{"S0_FIN0_ENV", "S0_TAX_ENV", "S0_GLOB_ENV"}}, // FIN's no-scope job's
		{"/api/v1/reactions", [3]string{"s0-fin-react", "s0-tax-react", "s0-glob-react"}},
		{"/api/v1/agency-matrix", [3]string{"s0-fin-runner", "s0-tax-runner", "s0-glob-runner"}},
		{"/api/v1/scope-agencies", [3]string{`"sc:fin"`, `"sc:tax"`, `"none"`}},
	}
	for _, l := range lists {
		for _, c := range []struct{ who, want string }{
			{gRoot, "YYY"}, {gFinAdmin, "Y.."}, {gTaxAdmin, ".Y."},
		} {
			want := c.want
			if l.path == "/api/v1/scope-agencies" {
				want = want[:2] + "." // no Global scope in the fixture
			}
			if got := sees(l.path, c.who, l.markers); got != want {
				t.Errorf("%s (%s) as %s: FIN/TAX/Global %s, want %s", l.path, l.markers[0], c.who, got, want)
			}
		}
	}

	// One agency's contents: its own people's, and as if absent to another's.
	if code := gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN", gFinAdmin, "").Code; code != http.StatusOK {
		t.Errorf("FIN's administrator reading FIN = %d, want 200", code)
	}
	if code := gateReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN", gTaxAdmin, "").Code; code != http.StatusNotFound {
		t.Errorf("TAX's administrator reading FIN = %d, want 404", code)
	}
}

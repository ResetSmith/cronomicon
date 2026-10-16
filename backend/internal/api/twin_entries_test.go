package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The schedule entries a definition's detail shows are that definition's
// (2.4.0 Phase R6).
//
// Two agencies may each hold a job, or a workflow, of one name. The detail
// read its entries by source, kind and name: FIN's `load` showed the entries
// of TAX's `load` beside its own, with the environment each carries, and the
// edit form, which sends back what the detail gave it, saved them as FIN's.
//
// An entry that records no owner is shown where the scheduler fires it: on
// the definition that is the one holder of its name, and on neither of two
// (the notice `schedule_entry_no_owner` says it is there).
func TestTheScheduleEntriesOfADetailAreThatDefinitionsOwn(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, enabled, created_at) VALUES
	      ('j-fin-load', 'load', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('j-tax-load', 'load', 'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('j-fin-solo', 'solo', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency) VALUES
	      ('w-fin', 'nightly', 'cronomicon', '[]', 1, '2026-01-01T00:00:00Z', 'ag:FIN'),
	      ('w-tax', 'nightly', 'cronomicon', '[]', 1, '2026-01-01T00:00:00Z', 'ag:TAX')`)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, env, position, owner_uid) VALUES
	      ('cronomicon', 'job',      'load',    'fin-two',   '0 2 * * *', '{"WHO":"fin"}',          0, 'j-fin-load'),
	      ('cronomicon', 'job',      'load',    'tax-three', '0 3 * * *', '{"WHO":"taxs-own-word"}', 0, 'j-tax-load'),
	      ('cronomicon', 'job',      'load',    'ghost',     '0 4 * * *', '{"WHO":"nobodys"}',      1, NULL),
	      ('cronomicon', 'job',      'solo',    'kept',      '0 5 * * *', NULL,                     0, NULL),
	      ('cronomicon', 'workflow', 'nightly', 'fin-two',   '0 2 * * *', '{"WHO":"fin"}',          0, 'w-fin'),
	      ('cronomicon', 'workflow', 'nightly', 'tax-three', '0 3 * * *', '{"WHO":"taxs-own-word"}', 0, 'w-tax'),
	      ('cronomicon', 'workflow', 'nightly', 'ghost',     '0 4 * * *', '{"WHO":"nobodys"}',      1, NULL)`)

	entries := func(who, path string) ([]string, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, path, who, "")
		var d struct {
			Schedules []struct{ Name string }
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &d) != nil {
			t.Fatalf("GET %s as %s = %d (%s)", path, who, rec.Code, rec.Body)
		}
		var names []string
		for _, e := range d.Schedules {
			names = append(names, e.Name)
		}
		slices.Sort(names)
		return names, rec.Body.String()
	}
	for _, c := range []struct {
		what, who, path string
		want            []string
	}{
		{"FIN's job", gFinAdmin, "/api/v1/jobs/" + rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = 'j-fin-load'`), []string{"fin-two"}},
		{"TAX's job", gTaxAdmin, "/api/v1/jobs/" + rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = 'j-tax-load'`), []string{"tax-three"}},
		{"FIN's workflow", gFinAdmin, "/api/v1/workflows/" + rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = 'w-fin'`), []string{"fin-two"}},
		{"TAX's workflow", gTaxAdmin, "/api/v1/workflows/" + rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = 'w-tax'`), []string{"tax-three"}},
		// A global administrator, who may read both, is shown each one's own too.
		{"FIN's job, to a global administrator", gRoot, "/api/v1/jobs/" + rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = 'j-fin-load'`), []string{"fin-two"}},
		// The one holder of a name has the entry that records no owner.
		{"the one job of its name", gFinAdmin, "/api/v1/jobs/" + rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = 'j-fin-solo'`), []string{"kept"}},
	} {
		got, body := entries(c.who, c.path)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s shows the entries %v, want %v", c.what, got, c.want)
		}
		if strings.HasPrefix(c.what, "FIN's") && strings.Contains(body, "taxs-own-word") {
			t.Errorf("%s shows the environment of TAX's entry: %s", c.what, body)
		}
	}
}

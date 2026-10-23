package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// SO-2 (2.5.0 C3): the recycle bin and a definition's revisions are compose on
// the definition's agency. They were a global administrator's (an agency's
// administrator could not restore their own job), and the bin listed every
// agency's binned rows. A name is resolved among the definitions the caller
// composes on: another agency's of the name is neither found, counted nor
// touched (question 72).
func TestC3_AnAgencyRestoresItsOwnBinnedJobAndSeesNoOther(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	const at = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO jobs (uid, name, source, run_type, owner_agency, command, created_at, deleted_at, deleted_by) VALUES
	        ('jb-fin',  'c3-fin',  'cronomicon', 'bash', 'ag:FIN', 'true', ?, ?, 'x'),
	        ('jb-tax',  'c3-tax',  'cronomicon', 'bash', 'ag:TAX', 'true', ?, ?, 'x'),
	        ('jb-both-fin', 'c3-both', 'cronomicon', 'bash', 'ag:FIN', 'true', ?, ?, 'x'),
	        ('jb-both-tax', 'c3-both', 'cronomicon', 'bash', 'ag:TAX', 'true', ?, ?, 'x')`,
		at, at, at, at, at, at, at, at)
	exec(`INSERT INTO definition_revisions (id, kind, source, name, revision_no, action, actor, created_at, snapshot_json, snapshot_digest, uid) VALUES
	        ('rv-fin', 'job', 'cronomicon', 'c3-both', 1, 'created', 'x', ?, '{"c3":"fin-snapshot"}', 'd1', 'jb-both-fin'),
	        ('rv-tax', 'job', 'cronomicon', 'c3-both', 1, 'created', 'x', ?, '{"c3":"tax-snapshot"}', 'd2', 'jb-both-tax')`, at, at)

	bin := func(who string) string {
		t.Helper()
		r := gateReq(t, h, http.MethodGet, "/api/v1/recycle-bin", who, "")
		if r.Code != http.StatusOK {
			t.Fatalf("%s reading the recycle bin = %d %s", who, r.Code, r.Body)
		}
		return r.Body.String()
	}
	fin := bin(gFinAdmin)
	if !strings.Contains(fin, `"c3-fin"`) || strings.Contains(fin, `"c3-tax"`) || strings.Count(fin, `"c3-both"`) != 1 {
		t.Errorf("FIN's administrator's bin: %s; want FIN's two only", fin)
	}
	if root := bin(gRoot); !strings.Contains(root, `"c3-tax"`) || strings.Count(root, `"c3-both"`) != 2 {
		t.Errorf("a global administrator's bin: %s; want every agency's", root)
	}

	// Revisions: FIN's of the name, not TAX's (both are revision 1).
	if b := gateReq(t, h, http.MethodGet, "/api/v1/definitions/job/c3-both/revisions", gFinAdmin, "").Body.String(); !strings.Contains(b, "fin-snapshot") || strings.Contains(b, "tax-snapshot") {
		t.Errorf("FIN's administrator's revisions of c3-both: %s; want FIN's only", b)
	}
	if code := gateReq(t, h, http.MethodGet, "/api/v1/definitions/job/c3-tax/revisions", gFinAdmin, "").Code; code != http.StatusNotFound {
		t.Errorf("FIN's administrator reading TAX's job's revisions = %d, want 404", code)
	}

	// Another agency's binned job: as absent.
	if code := gateReq(t, h, http.MethodPost, "/api/v1/recycle-bin/job/c3-tax/restore", gFinAdmin, "").Code; code != http.StatusNotFound {
		t.Errorf("FIN's administrator restoring TAX's job = %d, want 404", code)
	}
	if code := gateReq(t, h, http.MethodDelete, "/api/v1/recycle-bin/job/c3-tax", gFinAdmin, "").Code; code != http.StatusNotFound {
		t.Errorf("FIN's administrator purging TAX's job = %d, want 404", code)
	}
	// Its own, by a name TAX holds too: FIN's, and TAX's stays binned.
	if r := gateReq(t, h, http.MethodPost, "/api/v1/recycle-bin/job/c3-both/restore", gFinAdmin, ""); r.Code != http.StatusOK {
		t.Fatalf("FIN's administrator restoring its c3-both = %d %s", r.Code, r.Body)
	}
	if r := gateReq(t, h, http.MethodDelete, "/api/v1/recycle-bin/job/c3-fin", gFinAdmin, ""); r.Code != http.StatusNoContent {
		t.Fatalf("FIN's administrator purging its c3-fin = %d %s", r.Code, r.Body)
	}
	state := func(uid string) string {
		t.Helper()
		var deleted *string
		if err := pool.QueryRow(`SELECT deleted_at FROM jobs WHERE uid = ?`, uid).Scan(&deleted); err != nil {
			return "gone"
		}
		if deleted == nil {
			return "live"
		}
		return "binned"
	}
	for uid, want := range map[string]string{"jb-both-fin": "live", "jb-both-tax": "binned", "jb-fin": "gone", "jb-tax": "binned"} {
		if got := state(uid); got != want {
			t.Errorf("%s is %s, want %s", uid, got, want)
		}
	}
	// A viewer composes nothing: the bin is refused (compose).
	if code := gateReq(t, h, http.MethodGet, "/api/v1/recycle-bin", gFinViewer, "").Code; code != http.StatusForbidden {
		t.Errorf("a viewer reading the bin = %d, want 403", code)
	}
}

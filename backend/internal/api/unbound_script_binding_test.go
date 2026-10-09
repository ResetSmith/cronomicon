package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The manual trigger reads the bindings of the job's SCRIPT as well as the
// job's own, and since migration 1290 it finds the script by the uid on the job
// (jobs.script_uid), not by its name. Both halves are checked: a script that
// binds a department's secret refuses the unbound run, and another script of
// the SAME NAME (another repository's) does not, because it is not this job's
// script. A trigger that dropped the uid would let the first run through with
// no secret; one that fell back to the name would refuse the second.
func TestUnboundRunFollowsTheJobsOwnScript(t *testing.T) {
	h, pool := unboundRefServer(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib','global','lib.sh','bash','x','h','t')`)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('uid-lib-other','repo-b','lib.sh','bash','x','h','t')`)
	exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,owner_uid,ref_kind,ref_name,created_at)
	      VALUES('script','','lib.sh','uid-lib','secret','DEPT_PASSWORD','t')`)
	var jobID int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='unscoped-job'`).Scan(&jobID)
	run := func() (int, string) {
		t.Helper()
		rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs/"+itoa(jobID)+"/run", "sec-admins", `{}`)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		msg, _ := body["message"].(string)
		return rec.Code, msg
	}

	// The job uses the script that binds the department's secret.
	exec(`UPDATE jobs SET script_ref='lib.sh', script_uid='uid-lib' WHERE name='unscoped-job'`)
	code, msg := run()
	if code != http.StatusUnprocessableEntity || !strings.Contains(msg, "DEPT_PASSWORD") {
		t.Fatalf("an unbound run whose script binds an owned secret = %d %q, want 422 naming DEPT_PASSWORD", code, msg)
	}
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Errorf("the refused trigger created %d runs", runs)
	}

	// The same name, another script: not this job's bindings.
	exec(`UPDATE jobs SET script_uid='uid-lib-other' WHERE name='unscoped-job'`)
	if code, msg := run(); code == http.StatusUnprocessableEntity {
		t.Errorf("an unbound run was refused (%q) for a binding of ANOTHER script that shares its script's name", msg)
	}

	// The name with no script behind it: nothing to read, nothing refused.
	exec(`UPDATE jobs SET script_uid=NULL WHERE name='unscoped-job'`)
	if code, msg := run(); code == http.StatusUnprocessableEntity {
		t.Errorf("an unbound run of a job with a script name and no script was refused (%q)", msg)
	}
}

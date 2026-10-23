package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// What an agency's repository supplies is read by that agency's people and by
// those who reach every agency (2.4.0 Phase R6; GR-26). An agency's repository
// may be private: its scripts and their bodies, its Git schedules, its Git
// workflows and its past publishes are not another agency's to see, in a list
// or one at a time. Since 2.5.0 C2 a definition built in the app is judged by
// its owner too (it was read by everyone), and Global's are read only by
// those who reach every agency (the owner's answer to question 77; they were
// every session's).
func TestWhatAnAgencysRepositorySuppliesIsReadByItsAgency(t *testing.T) {
	h, pool := repoGateServer(t)
	exec := mustExec(t, pool)
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/git-repo", gFinAdmin, `{"repoUrl":"http://127.0.0.1:1/fin.git"}`)
	var made struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ID == "" {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	fin := made.ID
	syncsSettled(t, pool)

	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES
	        ('s-g',  'global', 'shared.sh',   'bash', 'echo GLOBAL-BODY',     'h', 't'),
	        ('s-f',  ?,        'fin-only.sh', 'bash', 'echo FIN-SECRET-BODY', 'h', 't'),
	        ('s-f2', ?,        'shared.sh',   'bash', 'echo FIN-TWIN-BODY',   'h', 't')`, fin, fin)
	exec(`INSERT INTO schedules (uid, name, source, owner_agency, repo_id, cron, content_hash, synced_at) VALUES
	        ('sch-g', 'global-nightly', 'git',        'global', 'global', '0 2 * * *', 'h', 't'),
	        ('sch-f', 'fin-nightly',    'git',        'ag:FIN', ?,        '0 3 * * *', 'h', 't'),
	        ('sch-a', 'app-sched',      'cronomicon', 'ag:FIN', NULL,     '0 4 * * *', 'h', 't')`, fin)
	exec(`INSERT INTO workflows (uid, name, source, repo_id, owner_agency, steps, synced_at) VALUES
	        ('w-g', 'g-flow',   'git',        'global', 'global', '[]', 't'),
	        ('w-f', 'fin-flow', 'git',        ?,        'ag:FIN', '[]', 't'),
	        ('w-a', 'app-flow', 'cronomicon', NULL,     'ag:FIN', '[]', 't')`, fin)
	exec(`DELETE FROM schedule_pushes`)
	exec(`INSERT INTO schedule_pushes (repo_id, at, actor, schedule_file, status, created_at) VALUES
	        ('global', '2026-01-01T00:00:00Z', 'a@example.com', 'jobs/global-file.yaml', 'success', 't'),
	        (?,        '2026-01-02T00:00:00Z', 'b@example.com', 'jobs/fin-secret-file.yaml', 'success', 't')`, fin)
	var finFlow, gFlow int64
	if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE uid = 'w-f'`).Scan(&finFlow); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE uid = 'w-g'`).Scan(&gFlow); err != nil {
		t.Fatal(err)
	}
	get := func(who, path string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, path, who, "")
		return rec.Code, rec.Body.String()
	}
	has := func(what, body string, want bool, part string) {
		t.Helper()
		if strings.Contains(body, part) != want {
			t.Errorf("%s: contains %q = %v, want %v\n%s", what, part, !want, want, body)
		}
	}

	// ── Who is of FIN, or reaches every agency: FIN's. ───────────────────────
	everywhere := map[string]bool{gAllViewer: true, gRoot: true}
	for _, who := range []string{gFinViewer, gFinOperator, gFinAdmin, gAllViewer, gRoot} {
		_, scripts := get(who, "/api/v1/scripts")
		has(who+" listing scripts", scripts, true, "fin-only.sh")
		has(who+" listing scripts", scripts, true, `"repoId":"`+fin+`"`)
		// Global's: only theirs who reach every agency (question 77).
		has(who+" listing scripts", scripts, everywhere[who], `"repoId":"global"`)
		if everywhere[who] {
			has(who+" listing scripts", scripts, true, `"totalItems":3`)
		} else {
			has(who+" listing scripts", scripts, true, `"totalItems":2`)
		}
		if code, body := get(who, "/api/v1/scripts/fin-only.sh?repo="+fin); code != http.StatusOK || !strings.Contains(body, "FIN-SECRET-BODY") {
			t.Errorf("%s reading FIN's script = %d %s, want it with its body", who, code, body)
		}
		if code, body := get(who, "/api/v1/script-content/fin-only.sh?repo="+fin); code != http.StatusOK || !strings.Contains(body, "FIN-SECRET-BODY") {
			t.Errorf("%s reading FIN's script's content = %d %s, want the body", who, code, body)
		}
		// A name two repositories hold: each by its repository, Global's by none.
		if _, body := get(who, "/api/v1/scripts/shared.sh?repo="+fin); !strings.Contains(body, "FIN-TWIN-BODY") {
			t.Errorf("%s reading FIN's shared.sh: %s", who, body)
		}
		if code, body := get(who, "/api/v1/scripts/shared.sh"); everywhere[who] != (code == http.StatusOK) ||
			strings.Contains(body, "FIN-TWIN") || (everywhere[who] && !strings.Contains(body, "GLOBAL-BODY")) {
			t.Errorf("%s reading shared.sh with no repository named, which is Global's = %d %s", who, code, body)
		}
		_, scheds := get(who, "/api/v1/schedule-defs")
		has(who+" listing schedules", scheds, true, "fin-nightly")
		has(who+" listing schedules", scheds, true, "app-sched") // FIN's, built in the app
		has(who+" listing schedules", scheds, everywhere[who], "global-nightly")
		_, flows := get(who, "/api/v1/workflows")
		has(who+" listing workflows", flows, true, "fin-flow")
		has(who+" listing workflows", flows, true, "app-flow")
		has(who+" listing workflows", flows, everywhere[who], "g-flow")
		if code, body := get(who, fmt.Sprintf("/api/v1/workflows/%d", finFlow)); code != http.StatusOK ||
			!strings.Contains(body, `"repoId":"`+fin+`"`) || !strings.Contains(body, `"ownerAgency":"ag:FIN"`) {
			t.Errorf("%s reading FIN's workflow = %d %s, want it with its repository and its agency", who, code, body)
		}
		_, pushes := get(who, "/api/v1/schedule-pushes")
		has(who+" listing past publishes", pushes, true, "fin-secret-file")
		has(who+" listing past publishes", pushes, everywhere[who], "global-file")
	}

	// ── Another agency's people: nothing of FIN's, nor of Global's. ──────────
	for _, who := range []string{gTaxAdmin} {
		_, scripts := get(who, "/api/v1/scripts")
		has(who+" listing scripts", scripts, false, "fin-only.sh")
		has(who+" listing scripts", scripts, false, fin)
		has(who+" listing scripts", scripts, false, "shared.sh")
		has(who+" listing scripts", scripts, true, `"totalItems":0`)
		_, named := get(who, "/api/v1/scripts?repo="+fin)
		has(who+" listing FIN's scripts by naming the repository", named, true, `"totalItems":0`)
		for _, path := range []string{
			"/api/v1/scripts/fin-only.sh?repo=" + fin, "/api/v1/script-content/fin-only.sh?repo=" + fin,
			"/api/v1/scripts/shared.sh?repo=" + fin, "/api/v1/script-content/shared.sh?repo=" + fin,
		} {
			code, body := get(who, path)
			if code != http.StatusNotFound || strings.Contains(body, "BODY") {
				t.Errorf("%s reading %s = %d %s, want 404", who, path, code, body)
			}
			// As for a repository that is not there.
			if gone, _ := get(who, strings.Replace(path, fin, "repo-nope", 1)); gone != code {
				t.Errorf("%s: %s answers %d and the same of a repository that is not there %d", who, path, code, gone)
			}
		}
		if code, body := get(who, "/api/v1/scripts/shared.sh"); code != http.StatusNotFound || strings.Contains(body, "BODY") {
			t.Errorf("%s reading Global's shared.sh = %d %s, want 404 (question 77)", who, code, body)
		}
		if code, body := get(who, "/api/v1/script-content/shared.sh"); code != http.StatusNotFound || strings.Contains(body, "BODY") {
			t.Errorf("%s reading Global's shared.sh's content = %d %s, want 404 (question 77)", who, code, body)
		}
		// FIN's script's name, with no repository named, is Global's question,
		// and Global's has none of that name.
		if code, _ := get(who, "/api/v1/scripts/fin-only.sh"); code != http.StatusNotFound {
			t.Errorf("%s reading fin-only.sh with no repository named = %d, want 404", who, code)
		}

		_, scheds := get(who, "/api/v1/schedule-defs")
		has(who+" listing schedules", scheds, false, "fin-nightly")
		has(who+" listing schedules", scheds, false, "global-nightly")
		has(who+" listing schedules", scheds, false, "app-sched") // FIN's, built in the app (C2)
		has(who+" listing schedules", scheds, true, `"totalItems":0`)

		_, flows := get(who, "/api/v1/workflows")
		has(who+" listing workflows", flows, false, "fin-flow")
		has(who+" listing workflows", flows, false, "g-flow")
		has(who+" listing workflows", flows, false, "app-flow") // FIN's, built in the app (C2)
		has(who+" listing workflows", flows, true, `"totalItems":0`)
		if code, body := get(who, fmt.Sprintf("/api/v1/workflows/%d", finFlow)); code != http.StatusNotFound {
			t.Errorf("%s reading FIN's workflow = %d %s, want 404", who, code, body)
		}
		if code, _ := get(who, fmt.Sprintf("/api/v1/workflows/%d", gFlow)); code != http.StatusNotFound {
			t.Errorf("%s reading Global's workflow = %d, want 404 (question 77)", who, code)
		}

		_, pushes := get(who, "/api/v1/schedule-pushes")
		has(who+" listing past publishes", pushes, false, "fin-secret-file")
		has(who+" listing past publishes", pushes, false, "global-file")
		_, named = get(who, "/api/v1/schedule-pushes?repo="+fin)
		has(who+" listing FIN's publishes by naming the repository", named, false, "fin-secret-file")
	}
}

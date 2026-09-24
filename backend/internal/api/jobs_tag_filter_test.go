package api_test

import (
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"testing"
	"time"
)

// TestJobsTagFilter is the FU-2 regression: the jobs-list ?tag= filter must match
// whole tags (exact JSON element), NOT a substring — the old `LIKE '%prod%'`
// wrongly matched a "production" tag. It also covers multi-tag AND/OR
// (?tag=a&tag=b + ?tagMatch=all|any).
func TestJobsTagFilter(t *testing.T) {
	ts, db := newTestServer(t)
	seedTaggedJobs(t, db)
	client := devLoginClient(t, ts)

	type jobItem struct {
		Name string `json:"name"`
	}
	type pageEnvelope struct {
		TotalItems int       `json:"totalItems"`
		Items      []jobItem `json:"items"`
	}
	names := func(q string) []string {
		var env pageEnvelope
		getJSON(t, client, ts.URL+"/api/v1/jobs?pageSize=200&"+q, &env)
		out := make([]string, 0, len(env.Items))
		for _, it := range env.Items {
			out = append(out, it.Name)
		}
		sort.Strings(out)
		return out
	}
	eq := func(name string, got, want []string) {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}

	// Exact match, not substring: ?tag=prod matches every job carrying the whole
	// "prod" tag (j-prod and j-prodweb) but NOT the "production"-tagged job — the
	// old `LIKE '%prod%'` wrongly included j-production.
	eq("tag=prod (exact)", names("tag=prod"), []string{"j-prod", "j-prodweb"})
	eq("tag=production (exact)", names("tag=production"), []string{"j-production"})

	// Multi-tag default (any = OR): union.
	eq("tag=prod&tag=web (any)", names("tag=prod&tag=web"), []string{"j-prod", "j-prodweb"})

	// Multi-tag all = AND: only the row carrying both.
	eq("tag=prod&tag=web (all)", names("tag=prod&tag=web&tagMatch=all"), []string{"j-prodweb"})

	// ?tags=csv form is unioned with ?tag=.
	eq("tags=prod,web (any)", names("tags="+url.QueryEscape("prod,web")), []string{"j-prod", "j-prodweb"})
}

func seedTaggedJobs(t *testing.T, db *sql.DB) {
	t.Helper()
	stamp := time.Now().UTC().Format(time.RFC3339)
	jobs := map[string]string{
		"j-prod":       `["prod"]`,
		"j-production": `["production"]`,
		"j-prodweb":    `["prod","web"]`,
		"j-untagged":   `[]`,
	}
	for name, tags := range jobs {
		if _, err := db.Exec(`
			INSERT INTO jobs(name, source, run_type, command, enabled, content_hash, synced_at, tags)
			VALUES (?, 'git', 'bash', 'echo hi', 1, 'h', ?, ?)`, name, stamp, tags); err != nil {
			t.Fatalf("seed job %s: %v", name, err)
		}
	}
}

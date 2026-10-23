package gitlab

import (
	"context"
	"strings"
	"testing"
)

// Present defect 64 (found by the review of 2.4.x): Global's repository URL
// is exempt from the allowed hosts, so it may carry a credential, and a clone
// that failed put the URL in its error, which the sync history, the activity
// log and the sync's answer carried to every session. The credential is in
// none of them.
func TestRV_ACloneThatFailsDoesNotRecordTheURLsCredential(t *testing.T) {
	svc, _, _ := newSyncFixture(t)
	const secret = "glpat-SECRETTOKEN"
	svc.repoURL = "http://oauth2:" + secret + "@127.0.0.1:9/infra/defs.git"
	res := svc.SyncBlocking(context.Background(), "manual")
	if res.Status != "failed" {
		t.Fatalf("a sync of an unreachable remote: %s, want failed", res.Status)
	}
	if strings.Contains(res.ErrorMessage, secret) || strings.Contains(res.ErrorMessage, "oauth2") {
		t.Errorf("the sync's answer carries the credential: %q", res.ErrorMessage)
	}
	for _, q := range []string{
		`SELECT COALESCE(GROUP_CONCAT(error_message, ' | '), '') FROM git_sync_events`,
		`SELECT COALESCE(GROUP_CONCAT(summary, ' | '), '') FROM activity`,
		`SELECT COALESCE(GROUP_CONCAT(message, ' | '), '') FROM git_sync_problems`,
	} {
		var got string
		if err := svc.db.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if strings.Contains(got, secret) || strings.Contains(got, "oauth2") {
			t.Errorf("%s holds the credential: %q", q, got)
		}
	}
}

package gitlab

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidatePublishPath(t *testing.T) {
	ok := []string{
		"jobs/foo.yaml",
		"schedules/bar.yml",
		"workflows/wf.yaml",
		"jobs/nested/deep.yaml",
	}
	for _, p := range ok {
		if err := validatePublishPath(p); err != nil {
			t.Errorf("validatePublishPath(%q) = %v, want nil", p, err)
		}
	}

	bad := []string{
		"",                             // empty
		"/etc/passwd",                  // absolute
		"../x.yaml",                    // parent escape
		"jobs/../../etc/cron.d/x.yaml", // traversal that escapes the clone
		"../../.ssh/authorized_keys",   // classic traversal
		"scripts/x.yaml",               // not a publishable dir
		"inventory/hosts.yaml",         // not a publishable dir
		"jobs/x.txt",                   // wrong suffix
		"jobs",                         // a dir, not a file
		"x.yaml",                       // no top-level publishable dir
	}
	for _, p := range bad {
		if err := validatePublishPath(p); err == nil {
			t.Errorf("validatePublishPath(%q) = nil, want error", p)
		}
	}
}

// TestPublishScheduleRejectsTraversal verifies the handler returns 422 for a
// path-traversal filePath and never reaches Publish (which would need the clone),
// so no out-of-tree file is written (PP-B3).
func TestPublishScheduleRejectsTraversal(t *testing.T) {
	// cloneDir intentionally does not contain a git repo: if Publish were reached
	// it would fail to open the clone (503), so a 422 proves validation short-
	// circuited before any filesystem access.
	svc := &Service{cloneDir: t.TempDir()}
	h := NewHandlers(svc)

	body := `{"filePath":"../../../tmp/cronomicon-evil.yaml","content":"apiVersion: cronomicon.io/v1\nkind: Job\n"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules/publish", strings.NewReader(body))
	req.Header.Set("If-Match", "deadbeefdeadbeef")
	rec := httptest.NewRecorder()

	h.PublishSchedule("attacker@example.com", rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("traversal publish = %d, want 422 (rejected before Publish)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "filePath") {
		t.Errorf("422 body should name the offending field; got %s", rec.Body.String())
	}
}

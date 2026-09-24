package gitlab

import (
	"path/filepath"
	"testing"
)

func jobManifest(name string) string {
	return "apiVersion: amadeus.io/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n  run_type: bash\n  command: echo hi\n"
}

// TestParseJobs_RecursiveFolders proves jobs in sub-folders are discovered (folder
// support, FB5) and that SourcePath carries the real nested path the UI groups by.
// parseJobs/parseSchedules/parseWorkflows share walkManifests, so jobs is the
// representative case.
func TestParseJobs_RecursiveFolders(t *testing.T) {
	clone := t.TempDir()
	svc := &Service{cloneDir: clone}

	mustWrite(t, filepath.Join(clone, "jobs", "top.yaml"), jobManifest("top"))
	mustWrite(t, filepath.Join(clone, "jobs", "db", "backup.yaml"), jobManifest("db-backup"))
	mustWrite(t, filepath.Join(clone, "jobs", "db", "restore.yml"), jobManifest("db-restore"))
	mustWrite(t, filepath.Join(clone, "jobs", "net", "edge", "scan.yaml"), jobManifest("net-edge-scan"))

	jobs, errs := svc.parseJobs()
	if len(errs) != 0 {
		t.Fatalf("unexpected parseJobs errors: %v", errs)
	}
	if len(jobs) != 4 {
		t.Fatalf("expected 4 jobs across folders, got %d", len(jobs))
	}
	want := map[string]string{
		"top":           "jobs/top.yaml",
		"db-backup":     "jobs/db/backup.yaml",
		"db-restore":    "jobs/db/restore.yml",
		"net-edge-scan": "jobs/net/edge/scan.yaml",
	}
	for _, j := range jobs {
		exp, ok := want[j.Metadata.Name]
		if !ok {
			t.Errorf("unexpected job discovered: %q", j.Metadata.Name)
			continue
		}
		if filepath.ToSlash(j.SourcePath) != exp {
			t.Errorf("job %q: SourcePath = %q, want %q", j.Metadata.Name, j.SourcePath, exp)
		}
	}
}

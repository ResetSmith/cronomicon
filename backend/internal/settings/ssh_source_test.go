package settings

import (
	"context"
	"strings"
	"testing"
)

// TestSshHostGitReadOnly (M4 / §9.5): a git-source (imported) host is read-only —
// Update/Delete are rejected; CreateSshHost writes cronomicon rows; ListSshHosts
// surfaces the source so the UI can render git rows read-only.
func TestSshHostGitReadOnly(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id,source,hostname,created_at,created_by,last_modified_by,last_modified_at) VALUES('g1','git','web1','t','gitlab','gitlab','t')`); err != nil {
		t.Fatal(err)
	}
	am, err := CreateSshHost(ctx, pool, SshHostInput{Hostname: "web2"}, "op")
	if err != nil || am == nil {
		t.Fatalf("create cronomicon host: %v", err)
	}
	if am.Source != "cronomicon" {
		t.Errorf("CreateSshHost source = %q, want cronomicon", am.Source)
	}

	hosts, err := ListSshHosts(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	srcByHost := map[string]string{}
	for _, h := range hosts {
		srcByHost[h.Hostname] = h.Source
	}
	if srcByHost["web1"] != "git" || srcByHost["web2"] != "cronomicon" {
		t.Errorf("ListSshHosts sources = %v, want web1=git web2=cronomicon", srcByHost)
	}

	// Git row: Update and Delete are rejected.
	if _, err := UpdateSshHost(ctx, pool, "g1", SshHostInput{Hostname: "x"}, "op"); err == nil || !strings.Contains(err.Error(), "cronomicon") {
		t.Errorf("UpdateSshHost on git row err = %v, want a not-editable error", err)
	}
	if ok, err := DeleteSshHost(ctx, pool, "g1", "op"); ok || err == nil || !strings.Contains(err.Error(), "cronomicon") {
		t.Errorf("DeleteSshHost on git row = (%v, %v), want (false, not-deletable)", ok, err)
	}

	// Cronomicon row: editable.
	if _, err := UpdateSshHost(ctx, pool, am.ID, SshHostInput{Hostname: "web2b"}, "op"); err != nil {
		t.Errorf("UpdateSshHost on cronomicon row should succeed, got %v", err)
	}
}

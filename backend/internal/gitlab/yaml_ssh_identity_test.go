package gitlab

import (
	"strings"
	"testing"
)

// CA-8 — spec.ssh_user / spec.ssh_credential parse and validate: the username
// charset is a hard error (it becomes an SSH auth string), while credential
// existence is deliberately NOT checked here (sync-time warning only — the repo
// may sync before the credential exists).
func TestValidateYAMLBytes_SSHIdentity(t *testing.T) {
	valid := "apiVersion: amadeus.io/v1\nkind: Job\nmetadata:\n  name: id-job\nspec:\n  run_type: bash\n  command: echo hi\n  ssh_user: deploy\n  ssh_credential: prod-key\n"
	errs, err := validateYAMLBytes("test.yaml", []byte(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("valid identity: expected no errors, got %v", errs)
	}

	bad := "apiVersion: amadeus.io/v1\nkind: Job\nmetadata:\n  name: id-job\nspec:\n  run_type: bash\n  command: echo hi\n  ssh_user: \"bad user;rm\"\n"
	errs, err = validateYAMLBytes("test.yaml", []byte(bad))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, e := range errs {
		if e.Field == "spec.ssh_user" && strings.Contains(e.Message, "invalid ssh_user") {
			found = true
		}
	}
	if !found {
		t.Errorf("bad ssh_user: expected a spec.ssh_user validation error, got %v", errs)
	}
}

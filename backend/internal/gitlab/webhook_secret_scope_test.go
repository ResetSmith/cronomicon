package gitlab

import (
	"context"
	"testing"
)

// The environment's webhook secret and the one read at start-up open Global's
// webhook and no other repository's (GR-21): they are scalar, and a second
// repository that honoured them would be opened by whoever holds Global's
// secret. A repository other than Global's accepts its own row's secret only.
func TestWebhookSecretsOfTheEnvironmentAreGlobalsAlone(t *testing.T) {
	db := mustOpenDB(t)
	if _, err := db.Exec(`INSERT INTO git_repos(id, agency_id) VALUES('repo-b', 'ag-b')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	global := &Service{db: db, webhookSecret: "boot-secret"}
	other := &Service{db: db, repoID: "repo-b", agencyID: "ag-b", webhookSecret: "boot-secret"}
	// A repository that has no row at all.
	gone := &Service{db: db, repoID: "no-such-repository", webhookSecret: "boot-secret"}

	if !global.ValidateWebhookToken(ctx, "boot-secret") {
		t.Errorf("Global's webhook refused the secret read at start-up")
	}
	if other.ValidateWebhookToken(ctx, "boot-secret") {
		t.Errorf("another repository's webhook accepted the secret read at start-up, which is Global's")
	}
	if gone.ValidateWebhookToken(ctx, "boot-secret") {
		t.Errorf("a repository with no row accepted the secret read at start-up")
	}

	t.Setenv("CRONOMICON_GITLAB_WEBHOOK_SECRET", "env-secret")
	if !global.ValidateWebhookToken(ctx, "env-secret") {
		t.Errorf("Global's webhook refused the environment's secret")
	}
	if global.ValidateWebhookToken(ctx, "boot-secret") {
		t.Errorf("with the environment's secret set, Global's webhook accepted another")
	}
	if other.ValidateWebhookToken(ctx, "env-secret") {
		t.Errorf("another repository's webhook accepted the environment's secret, which is Global's")
	}
	if gone.ValidateWebhookToken(ctx, "env-secret") {
		t.Errorf("a repository with no row accepted the environment's secret")
	}
}

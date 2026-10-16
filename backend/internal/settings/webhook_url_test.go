package settings

import "testing"

// A rotation of the webhook secret updates the hook on the Git server's side,
// which it finds by the hook's URL: this repository's webhook route (GR-20).
// Global's is also the route with no id, which every hook installed before
// 2.4.0 has. A hook of ANOTHER repository of this installation is not this
// repository's, although its URL begins the same way.
func TestIsWebhookURLOf(t *testing.T) {
	const base = "https://cron.example/api/v1/webhooks/gitlab"
	for _, c := range []struct {
		url, repo string
		want      bool
	}{
		{base, "global", true},
		{base + "/", "global", true},
		{base + "/global", "global", true},
		{base + "/repo-b", "global", false},
		{base + "/repo-b", "repo-b", true},
		{base + "/repo-b/", "repo-b", true},
		{base, "repo-b", false},
		{base + "/global", "repo-b", false},
		{base + "/repo-bb", "repo-b", false},
		{"https://elsewhere.example/hooks/ci", "global", false},
		{"https://elsewhere.example/hooks/ci", "repo-b", false},
	} {
		if got := isWebhookURLOf(c.url, c.repo); got != c.want {
			t.Errorf("isWebhookURLOf(%q, %q) = %v, want %v", c.url, c.repo, got, c.want)
		}
	}
}

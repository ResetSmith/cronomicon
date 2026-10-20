package gitlab

import "testing"

// 2.5.0 C11, defect 74 (question 111): the stamp an assignment commits to the
// target agency's repository names the origin's repository without a user, a
// password, a query or a fragment. Global's stored URL may carry `user:token@`
// (settings.CheckRepoURL holds only an agency's to the no-user rule), and the
// target's history would keep it.
func TestC11_AnAssignmentStampsNoCredentials(t *testing.T) {
	for raw, want := range map[string]string{
		"https://oauth2:glpat-SECRET@gitlab.example/ops/defs.git":       "https://gitlab.example/ops/defs.git",
		"https://deploy@gitlab.example/ops/defs.git":                    "https://gitlab.example/ops/defs.git",
		"https://gitlab.example/ops/defs.git?private_token=SECRET#frag": "https://gitlab.example/ops/defs.git",
		"http://git:8080/global.git":                                    "http://git:8080/global.git",
		"/srv/git/global.git":                                           "/srv/git/global.git",
		"git@gitlab.example:ops/defs.git":                               "", // not a URL we can clean: none
		"":                                                              "",
	} {
		if got := originRepository(raw); got != want {
			t.Errorf("originRepository(%q) = %q, want %q", raw, got, want)
		}
	}
}

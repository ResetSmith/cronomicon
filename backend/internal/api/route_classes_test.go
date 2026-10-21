package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// CF-2 (2.5.0 C2): every GET route is classified by who may read what it
// serves, and this test fails on a route that is not (and on a class kept for
// a route that is no longer mounted). The leaks C0 found were routes nobody had
// classified.
//
// The classes:
//
//	one-agency      an agency's rows to its own people and those who reach every
//	                agency; Global's only to the latter (CF-1; questions 76, 77)
//	all-agencies    the Operations overview: those who reach every agency only
//	installation    a global administrator's
//	any-session     not an agency's content: the caller's own, or definitions
//	                and names that are not confidential
//	public          no session (health, sign-in, agent downloads)
//	runner          an agent's bearer token
//
// target, where set, is the class a later phase gives the route, and that
// phase: the route is today's class until then.
type routeClass struct {
	class  string
	target string
}

var routeClasses = map[string]routeClass{
	// ── one-agency ──
	"/api/v1/access-grants":                        {"one-agency", ""},
	"/api/v1/activity":                             {"one-agency", ""},
	"/api/v1/activity/actors":                      {"one-agency", ""},
	"/api/v1/agencies/{agencyId}":                  {"one-agency", ""},
	"/api/v1/agencies/{agencyId}/git-repo":         {"one-agency", ""},
	"/api/v1/agencies/{agencyId}/vault-prefixes":   {"one-agency", ""},
	"/api/v1/agencies/{agencyId}/groups":           {"one-agency", ""},
	"/api/v1/agencies/{agencyId}/secrets-report":   {"one-agency", ""},
	"/api/v1/agencies/{agencyId}/held-by-global":   {"one-agency", ""},
	"/api/v1/agencies/{agencyId}/vault":            {"one-agency", ""},
	"/api/v1/agency-matrix":                        {"one-agency", ""},
	"/api/v1/analytics/runs":                       {"one-agency", ""},
	"/api/v1/audit/export":                         {"one-agency", ""}, // CF-5 (C1); a global administrator's holds all
	"/api/v1/calendars":                            {"one-agency", ""},
	"/api/v1/calendars/{name}":                     {"one-agency", ""},
	"/api/v1/change-log":                           {"one-agency", ""},
	"/api/v1/env-secrets":                          {"one-agency", ""},
	"/api/v1/env-var-agencies":                     {"one-agency", ""},
	"/api/v1/env-var-shadows":                      {"one-agency", ""},
	"/api/v1/env-vars":                             {"one-agency", ""},
	"/api/v1/git-repo-names":                       {"one-agency", ""},
	"/api/v1/git-repos":                            {"one-agency", ""},
	"/api/v1/git-repos/{repoId}/problems":          {"one-agency", ""},
	"/api/v1/git/history":                          {"one-agency", ""},
	"/api/v1/git/sync":                             {"one-agency", ""},
	"/api/v1/host-key-batches/{batchId}":           {"one-agency", ""},
	"/api/v1/job-reference-bindings/{jobId}":       {"one-agency", ""},
	"/api/v1/jobs":                                 {"one-agency", ""},
	"/api/v1/jobs/{jobId}":                         {"one-agency", ""},
	"/api/v1/jobs/{jobId}/file-sightings":          {"one-agency", ""},
	"/api/v1/me/agency-badges":                     {"one-agency", ""},
	"/api/v1/notices":                              {"one-agency", ""},
	"/api/v1/reactions":                            {"one-agency", ""},
	"/api/v1/reactions/{ownerKind}/{ownerName}":    {"one-agency", ""},
	"/api/v1/reference-usage":                      {"one-agency", ""},
	"/api/v1/runner-agencies":                      {"one-agency", ""},
	"/api/v1/runners":                              {"one-agency", ""},
	"/api/v1/runners/host-keys/pending":            {"one-agency", ""},
	"/api/v1/runners/registration-tokens":          {"one-agency", ""},
	"/api/v1/runners/{id}/host-keys":               {"one-agency", ""},
	"/api/v1/runners/{id}/host-keys/pending":       {"one-agency", ""},
	"/api/v1/runs":                                 {"one-agency", ""},
	"/api/v1/runs/{traceId}":                       {"one-agency", ""},
	"/api/v1/runs/{traceId}/log":                   {"one-agency", ""},
	"/api/v1/runs/{traceId}/references":            {"one-agency", ""},
	"/api/v1/schedule-defs":                        {"one-agency", ""},
	"/api/v1/schedule-defs/{name}":                 {"one-agency", ""},
	"/api/v1/schedule-pushes":                      {"one-agency", ""},
	"/api/v1/schedules":                            {"one-agency", ""},
	"/api/v1/schedules/upcoming":                   {"one-agency", ""},
	"/api/v1/scope-agencies":                       {"one-agency", ""},
	"/api/v1/scope-binding-notices":                {"one-agency", ""},
	"/api/v1/scopes":                               {"one-agency", ""},
	"/api/v1/scopes/{scopeId}":                     {"one-agency", ""},
	"/api/v1/scopes/{scopeId}/host-key-coverage":   {"one-agency", ""},
	"/api/v1/scopes/{scopeId}/inventory":           {"one-agency", ""},
	"/api/v1/script-content/{name...}":             {"one-agency", ""},
	"/api/v1/script-reference-bindings/{name...}":  {"one-agency", ""},
	"/api/v1/script-reference-scan/{name...}":      {"one-agency", ""},
	"/api/v1/scripts":                              {"one-agency", ""},
	"/api/v1/scripts/{name...}":                    {"one-agency", ""},
	"/api/v1/secret-agencies":                      {"one-agency", ""},
	"/api/v1/service-accounts":                     {"one-agency", ""},
	"/api/v1/ssh-credential-agencies":              {"one-agency", ""},
	"/api/v1/ssh/bastions":                         {"one-agency", ""},
	"/api/v1/ssh/credentials":                      {"one-agency", ""},
	"/api/v1/ssh/credentials/{credentialId}":       {"one-agency", ""},
	"/api/v1/ssh/credentials/{credentialId}/usage": {"one-agency", ""},
	"/api/v1/env-secrets/{secretId}/usage":         {"one-agency", ""},
	"/api/v1/ssh/hosts":                            {"one-agency", ""},
	"/api/v1/workflow-runs":                        {"one-agency", ""},
	"/api/v1/workflow-runs/{traceId}":              {"one-agency", ""},
	"/api/v1/workflows":                            {"one-agency", ""},
	"/api/v1/workflows/{workflowId}":               {"one-agency", ""},
	"/api/v1/alert-targets":                        {"one-agency", ""},
	"/api/v1/alerts":                               {"one-agency", ""},
	"/api/v1/recent-logins":                        {"one-agency", ""},
	"/api/v1/access/recent-logins":                 {"one-agency", ""},
	"/api/v1/definitions/{kind}/{name}/revisions":  {"one-agency", ""}, // SO-2 (C3): compose on the definition's agency
	"/api/v1/recycle-bin":                          {"one-agency", ""}, // SO-2 (C3)

	// ── all-agencies (the Operations overview) ──
	"/api/v1/overview":        {"all-agencies", ""},
	"/api/v1/overview/search": {"all-agencies", ""},

	// ── installation ──
	"/api/v1/settings/vault":         {"installation", ""},
	"/api/v1/rbac-preflight":         {"installation", ""},
	"/api/v1/settings/log-storage":   {"installation", ""},
	"/api/v1/settings/observability": {"installation", ""},
	"/api/v1/settings/backup":        {"installation", ""},

	// ── any-session ──
	"/api/v1/me":                        {"any-session", ""},
	"/api/v1/me/access":                 {"any-session", ""},
	"/api/v1/me/context":                {"any-session", ""},
	"/api/v1/capabilities":              {"any-session", ""},
	"/api/v1/agencies":                  {"any-session", ""},
	"/api/v1/roles":                     {"any-session", ""},
	"/api/v1/settings/general":          {"any-session", ""},
	"/api/v1/settings/freeze":           {"any-session", ""},
	"/api/v1/settings/audit-compliance": {"any-session", ""},
	"/api/v1/local-runner":              {"any-session", ""},
	"/api/v1/settings/notifications":    {"any-session", ""},

	// ── public ──
	"/healthz":               {"public", ""},
	"/readyz":                {"public", ""},
	"/version":               {"public", ""},
	"/agents/{filename}":     {"public", ""},
	"/install/{token}":       {"public", ""},
	"/api/v1/auth/providers": {"public", ""},
	"/api/v1/auth/login":     {"public", ""},
	"/api/v1/auth/callback":  {"public", ""},
	"/api/v1/auth/dev-login": {"public", ""},

	// ── runner ──
	"/api/v1/runners/{id}/poll":       {"runner", ""},
	"/api/v1/runs/{traceId}/manifest": {"runner", ""},
}

// routeRegistration is a GET registration on the mux: its pattern is the
// string literal after "GET ".
var routeRegistration = regexp.MustCompile(`mux\.Handle(?:Func)?\(\s*"GET (/[^"]*)"`)

// mountedGetRoutes reads this package's source for every GET route it mounts.
// The membership routes are mounted in a loop ("GET /api/v1/" + kind +
// "-agencies"), so that registration stands for its four kinds; the metrics
// route's path is read from the database and is not a literal (it serves
// aggregate counters only, behind its own guard).
func mountedGetRoutes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) //nolint:gosec // our own package's source
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeRegistration.FindAllStringSubmatch(string(src), -1) {
			p := m[1]
			if p == "/api/v1/" { // the membership loop
				for _, k := range []string{"scope", "secret", "env-var", "ssh-credential"} {
					seen["/api/v1/"+k+"-agencies"] = true
				}
				continue
			}
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestEveryGetRouteIsClassified(t *testing.T) {
	mounted := mountedGetRoutes(t)
	if len(mounted) < 90 {
		t.Fatalf("found %d GET routes; the source scan is broken", len(mounted))
	}
	valid := map[string]bool{"one-agency": true, "all-agencies": true, "installation": true, "any-session": true, "public": true, "runner": true}
	on := map[string]bool{}
	for _, p := range mounted {
		on[p] = true
		c, ok := routeClasses[p]
		if !ok {
			t.Errorf("GET %s is not classified: add it to routeClasses with who may read what it serves (CF-2)", p)
			continue
		}
		if !valid[c.class] {
			t.Errorf("GET %s: unknown class %q", p, c.class)
		}
	}
	for p := range routeClasses {
		if !on[p] {
			t.Errorf("routeClasses classifies GET %s, which is not mounted: remove it", p)
		}
	}
}

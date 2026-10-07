package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every write route, and the gate that guards it (GC-18, v2.2.2).
//
// The cross-agency holes closed in v2.2.2 had one cause: a route was registered
// behind "holds this permission somewhere" and nobody recorded whether that was
// enough for what the route changes. This table records it for every route that
// is not a GET. TestEveryWriteRouteIsClassified fails when a route is
// registered that the table does not name — so a new route cannot ship until
// someone has decided, in writing, who may call it — and the two probe tests
// then hold the table to its word.
//
// The classes are the only vocabulary. There is deliberately no catch-all:
// clsAnywhere is "the permission held anywhere is enough", and every use of it
// must say why that is acceptable and what replaces it.
type gateClass string

const (
	// clsGlobal — a global administrator: an unrestricted grant that itself
	// carries the permission (requireGlobal, requireFleetWide, or the same
	// predicate in the handler).
	clsGlobal gateClass = "global"
	// clsComposeAdmin — compose AND configureApp on one unrestricted grant
	// (requireComposeAdmin): the shared authoring surfaces with no owner.
	clsComposeAdmin gateClass = "composeAdmin"
	// clsObject — a permission checked against the object's own agency or
	// scope. The note names the check.
	clsObject gateClass = "object"
	// clsVerb — a run verb (triggerJobs / killJobs) checked in the handler
	// against every scope the run touches.
	clsVerb gateClass = "verb"
	// clsCompose — the compose permission checked against the definition's
	// scope(s), on both sides of an edit.
	clsCompose gateClass = "compose"
	// clsVisible — any signed-in user, but only on a row that user can read.
	clsVisible gateClass = "visible"
	// clsOpen — any signed-in user, by design. The note says why.
	clsOpen gateClass = "open"
	// clsSelf — acts only on the caller's own session.
	clsSelf gateClass = "self"
	// clsAgent / clsToken / clsWebhook — not a session at all: a runner's
	// bearer token, a service-account token, a shared webhook secret.
	clsAgent   gateClass = "agent"
	clsToken   gateClass = "token"
	clsWebhook gateClass = "webhook"
	// clsAnywhere — the permission held on ANY agency. A known gap, never a
	// default: the note must say what closes it.
	clsAnywhere gateClass = "anywhere"
)

type routeGate struct {
	class gateClass
	note  string
}

var writeRouteGates = map[string]routeGate{
	"POST /api/v1/access-grants":                                    {clsObject, "requireGrantWritable — own agency, never all-scopes, no amplification (AF-3)"},
	"PUT /api/v1/access-grants/{grantId}":                           {clsObject, "requireGrantWritable on the existing row and the incoming one"},
	"DELETE /api/v1/access-grants/{grantId}":                        {clsObject, "requireGrantWritable on the existing row"},
	"POST /api/v1/roles":                                            {clsGlobal, "requireRoleTemplateAdmin — role templates are shared by every agency"},
	"PUT /api/v1/roles/{roleName}":                                  {clsGlobal, "requireRoleTemplateAdmin"},
	"DELETE /api/v1/roles/{roleName}":                               {clsGlobal, "requireRoleTemplateAdmin"},
	"POST /api/v1/agencies":                                         {clsGlobal, "the agency catalog is the installation's"},
	"PUT /api/v1/agencies/{agencyId}":                               {clsGlobal, "the agency catalog is the installation's"},
	"DELETE /api/v1/agencies/{agencyId}":                            {clsGlobal, "the agency catalog is the installation's"},
	"PUT /api/v1/scopes/{scopeId}/agency":                           {clsGlobal, "moving a scope decides who can reach it (GC-6)"},
	"PUT /api/v1/agencies/{agencyId}/members":                       {clsObject, "per member: requireEntityAgency on both sides; a scope member needs requireScopeMove"},
	"PUT /api/v1/runner-agencies":                                   {clsObject, "per runner: the runner-agency gate on both sides"},
	"PUT /api/v1/":                                                  {clsObject, "the four {kind}-agencies setters: requireEntityAgency on both sides; the scope kind needs requireScopeMove"},
	"POST /api/v1/auth/logout":                                      {clsSelf, "ends the caller's own session"},
	"PUT /api/v1/job-reference-bindings/{jobId}":                    {clsObject, "the job's scope must be writable by the caller"},
	"PUT /api/v1/script-reference-bindings/{name...}":               {clsGlobal, "a script is shared across scopes: an unrestricted manageEnvVars grant"},
	"POST /api/v1/references/validate":                              {clsOpen, "validates reference names for the caller; writes nothing"},
	"POST /api/v1/calendars":                                        {clsComposeAdmin, "a calendar has no owner; a global one freezes every agency"},
	"PUT /api/v1/calendars/{name}":                                  {clsComposeAdmin, "a calendar has no owner"},
	"DELETE /api/v1/calendars/{name}":                               {clsComposeAdmin, "a calendar has no owner"},
	"PUT /api/v1/calendars/{name}/days":                             {clsComposeAdmin, "a calendar has no owner"},
	"POST /api/v1/jobs/{jobId}/run":                                 {clsVerb, "requireCan(triggerJobs) on the effective scope; unbound is unrestricted-only"},
	"POST /api/v1/jobs/{jobId}/pause":                               {clsVerb, "requireCan on the job's scope"},
	"POST /api/v1/jobs/{jobId}/resume":                              {clsVerb, "requireCan on the job's scope"},
	"POST /api/v1/jobs/{jobId}/kill":                                {clsVerb, "requireCan(killJobs) on the run's scope"},
	"PUT /api/v1/job-tags/{jobId}":                                  {clsVisible, "any signed-in user, on a job they can read (GC-12)"},
	"PUT /api/v1/job-annotation/{jobId}":                            {clsVisible, "any signed-in user, on a job they can read (GC-12)"},
	"PATCH /api/v1/workflows/{workflowId}":                          {clsVerb, "workflowScopesPermit over every job, sub-workflows included"},
	"POST /api/v1/workflows/{workflowId}/trigger":                   {clsVerb, "workflowScopesPermit over every job, sub-workflows included"},
	"PUT /api/v1/workflow-tags/{workflowId}":                        {clsVisible, "any signed-in user, on a workflow they can read (GC-12)"},
	"PUT /api/v1/workflow-annotation/{workflowId}":                  {clsVisible, "any signed-in user, on a workflow they can read (GC-12)"},
	"POST /api/v1/workflows/runs/{traceId}/cancel":                  {clsVerb, "killJobs on every scope the run tree touches (GC-10)"},
	"POST /api/v1/webhooks/gitlab":                                  {clsWebhook, "X-Gitlab-Token shared secret; triggers a sync, takes no input"},
	"POST /api/v1/schedules/publish":                                {clsObject, "authorizePublish — the scope on both sides of the write (GC-9)"},
	"POST /api/v1/git/sync":                                         {clsGlobal, "one repository for the installation"},
	"POST /api/v1/scopes/resync":                                    {clsGlobal, "one repository for the installation"},
	"POST /api/v1/jobs":                                             {clsCompose, "requireComposeScope on the job's scope"},
	"PUT /api/v1/jobs/{jobId}":                                      {clsCompose, "requireComposeScope on the existing scope and the new one"},
	"DELETE /api/v1/jobs/{jobId}":                                   {clsCompose, "requireComposeScope on the job's scope"},
	"DELETE /api/v1/pending-runs/{id}":                              {clsVerb, "a run verb on every scope the pending run would touch (GC-11)"},
	"PUT /api/v1/reactions/{ownerKind}/{ownerName}":                 {clsComposeAdmin, "a reaction can trigger any agency's definition"},
	"DELETE /api/v1/reactions/{ownerKind}/{ownerName}/{name}":       {clsComposeAdmin, "a reaction can trigger any agency's definition"},
	"POST /api/v1/definitions/{kind}/{name}/revisions/{no}/restore": {clsComposeAdmin, "revisions span every agency's definitions"},
	"POST /api/v1/recycle-bin/{kind}/{name}/restore":                {clsComposeAdmin, "the bin spans every agency's definitions"},
	"DELETE /api/v1/recycle-bin/{kind}/{name}":                      {clsComposeAdmin, "a purge is irreversible and spans every agency"},
	"PUT /api/v1/runner-tags/{runnerId}":                            {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/register":                                 {clsAgent, "a single-use registration token"},
	"POST /api/v1/runners/registration-tokens":                      {clsGlobal, "requireFleetWide — a new runner joins the general pool"},
	"DELETE /api/v1/runners/registration-tokens/{id}":               {clsGlobal, "requireFleetWide"},
	"POST /api/v1/runners/{id}/drain":                               {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/resync":                              {clsObject, "requireRunnerAgency"},
	"PATCH /api/v1/runners/{id}/settings":                           {clsObject, "requireRunnerAgency"},
	"PUT /api/v1/runners/{id}/secret-injection":                     {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/keyscan":                             {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/host-keys/{keyId}/resolve":                {clsObject, "requireHostKeyRunnerAgency"},
	"POST /api/v1/runners/{id}/host-keys/resolve-batch":             {clsObject, "requireHostKeyBatchRunnerAgency"},
	"POST /api/v1/runners/{id}/host-keys/provide":                   {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/host-keys/{ledgerId}/remove":         {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/host-keys/{ledgerId}/resend":         {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/known-hosts/refresh":                 {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/host-keys/carry":                     {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/file-sightings":                      {clsAgent, "the runner's own bearer token"},
	"POST /api/v1/runners/{id}/hostkeys":                            {clsAgent, "the runner's own bearer token"},
	"POST /api/v1/runners/{id}/known-hosts":                         {clsAgent, "the runner's own bearer token"},
	"POST /api/v1/runners/{id}/redeclare":                           {clsAgent, "the runner's own bearer token"},
	"POST /api/v1/runners/{id}/placement":                           {clsObject, "configureApp on every agency the placement restores"},
	"POST /api/v1/runners/{id}/placement/dismiss":                   {clsObject, "the same gate as accepting the placement"},
	"DELETE /api/v1/runners/{id}":                                   {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runners/{id}/test":                                {clsObject, "requireRunnerAgency"},
	"POST /api/v1/runs/{traceId}/log":                               {clsAgent, "the runner's own bearer token"},
	"POST /api/v1/schedule-defs":                                    {clsComposeAdmin, "a reusable schedule has no owner; an edit retimes every referrer"},
	"PUT /api/v1/schedule-defs/{name}":                              {clsComposeAdmin, "a reusable schedule has no owner"},
	"DELETE /api/v1/schedule-defs/{name}":                           {clsComposeAdmin, "a reusable schedule has no owner"},
	"PUT /api/v1/schedule-tags/{name}":                              {clsOpen, "any signed-in user; a schedule has no scope to read-check until it has an owner"},
	"PUT /api/v1/scopes/{scopeId}/runners":                          {clsObject, "requireScopeAgency, plus the runner gate for each runner added"},
	"POST /api/v1/scopes/{scopeId}/runners/preview":                 {clsObject, "requireScopeAgency"},
	"POST /api/v1/scope-runners/replace":                            {clsObject, "the replacement runner, and every scope the old one is bound to (GC-6)"},
	"POST /api/v1/scope-binding-notices/dismiss":                    {clsObject, "configureApp on each notice's scope; a notice with no scope is a global administrator's"},
	"PUT /api/v1/script-tags/{name...}":                             {clsOpen, "any signed-in user; a script has no scope to read-check until it has an owner"},
	"POST /api/v1/service-accounts":                                 {clsObject, "requireGrantWritable — minting is granting (GC-5)"},
	"DELETE /api/v1/service-accounts/{id}":                          {clsObject, "requireGrantWritable on the existing row (GC-5)"},
	"POST /api/v1/trigger/jobs/{name}":                              {clsToken, "a service-account token; delegates to runJob's own gates"},
	"POST /api/v1/trigger/workflows/{name}":                         {clsToken, "a service-account token; delegates to triggerWorkflow's own gates"},
	"POST /api/v1/env-vars":                                         {clsObject, "ScopeWritable and requireCreationAgencies"},
	"PUT /api/v1/env-vars/{envVarId}":                               {clsObject, "ScopeWritable and requireEntityAgency"},
	"DELETE /api/v1/env-vars/{envVarId}":                            {clsObject, "ScopeWritable and requireEntityAgency"},
	"PUT /api/v1/env-var-tags/{envVarId}":                           {clsObject, "requireEntityAgency"},
	"POST /api/v1/env-secrets":                                      {clsObject, "ScopeWritable and requireCreationAgencies; a Vault path is global (GC-8)"},
	"PUT /api/v1/env-secrets/{secretId}":                            {clsObject, "ScopeWritable and requireEntityAgency; a Vault path is global (GC-8)"},
	"DELETE /api/v1/env-secrets/{secretId}":                         {clsObject, "ScopeWritable and requireEntityAgency"},
	"POST /api/v1/env-secrets/{secretId}/reveal":                    {clsObject, "requireEntityAgency"},
	"POST /api/v1/env-secrets/{secretId}/migrate-to-vault":          {clsObject, "requireEntityAgency, then a global administrator (GC-8)"},
	"PUT /api/v1/env-secret-tags/{secretId}":                        {clsObject, "requireEntityAgency"},
	"POST /api/v1/scopes":                                           {clsObject, "requireCreationAgencies — born in the creator's agency (GC-6)"},
	"PUT /api/v1/scopes/{scopeId}/inventory":                        {clsObject, "requireScopeAgency"},
	"POST /api/v1/scopes/{scopeId}/inventory/import-hosts":          {clsObject, "requireScopeAgency"},
	"PATCH /api/v1/scopes/{scopeId}":                                {clsObject, "requireScopeAgency"},
	"DELETE /api/v1/scopes/{scopeId}":                               {clsObject, "requireScopeAgency"},
	"PUT /api/v1/scope-tags/{scopeId}":                              {clsObject, "requireScopeAgency"},
	"POST /api/v1/alerts":                                           {clsGlobal, "alert rules have no owner and can match every agency's runs"},
	"PUT /api/v1/alerts/{alertId}":                                  {clsGlobal, "alert rules have no owner"},
	"DELETE /api/v1/alerts/{alertId}":                               {clsGlobal, "alert rules have no owner"},
	"POST /api/v1/ssh/hosts":                                        {clsGlobal, "a manual host record applies to every scope (GC-7)"},
	"PUT /api/v1/ssh/hosts/{hostId}":                                {clsObject, "requireHostOwner"},
	"DELETE /api/v1/ssh/hosts/{hostId}":                             {clsObject, "requireHostOwner"},
	"POST /api/v1/ssh/hosts/{hostId}/test":                          {clsObject, "requireHostOwner"},
	"DELETE /api/v1/ssh/hosts/{hostId}/host-key":                    {clsObject, "requireHostOwner"},
	"POST /api/v1/ssh/bastions":                                     {clsGlobal, "a bastion has no owner (GC-7)"},
	"PUT /api/v1/ssh/bastions/{bastionId}":                          {clsGlobal, "a bastion has no owner"},
	"DELETE /api/v1/ssh/bastions/{bastionId}":                       {clsGlobal, "a bastion has no owner"},
	"POST /api/v1/ssh/bastions/{bastionId}/test":                    {clsGlobal, "a bastion has no owner"},
	"DELETE /api/v1/ssh/bastions/{bastionId}/host-key":              {clsGlobal, "a bastion has no owner"},
	"POST /api/v1/ssh/credentials":                                  {clsObject, "requireCreationAgencies"},
	"PUT /api/v1/ssh/credentials/{credentialId}":                    {clsObject, "requireEntityAgency"},
	"DELETE /api/v1/ssh/credentials/{credentialId}":                 {clsObject, "requireEntityAgency"},
	"PUT /api/v1/ssh-credential-tags/{credentialId}":                {clsObject, "requireEntityAgency"},
	"PUT /api/v1/settings/general":                                  {clsGlobal, "install-wide setting"},
	"PUT /api/v1/settings/notifications":                            {clsGlobal, "install-wide setting"},
	"POST /api/v1/settings/notifications/test":                      {clsGlobal, "sends over the installation's transports"},
	"PUT /api/v1/settings/audit-compliance":                         {clsGlobal, "install-wide setting"},
	"PUT /api/v1/settings/gitlab":                                   {clsGlobal, "install-wide setting"},
	"POST /api/v1/settings/gitlab/webhook-secret/rotate":            {clsGlobal, "install-wide setting"},
	"PUT /api/v1/settings/vault":                                    {clsGlobal, "install-wide setting"},
	"PUT /api/v1/settings/log-storage":                              {clsGlobal, "install-wide setting"},
	"POST /api/v1/settings/log-storage/sync":                        {clsGlobal, "install-wide setting"},
	"PUT /api/v1/settings/observability":                            {clsGlobal, "install-wide setting"},
	"POST /api/v1/workflows":                                        {clsCompose, "requireComposeStepScopes over every job, sub-workflows included (GC-10)"},
	"POST /api/v1/workflows/validate":                               {clsCompose, "validates a graph for the caller; writes nothing"},
	"PUT /api/v1/workflows/{workflowId}":                            {clsCompose, "requireComposeStepScopes on the existing graph and the new one"},
	"DELETE /api/v1/workflows/{workflowId}":                         {clsCompose, "requireComposeStepScopes on the existing graph"},
}

var writeRouteRe = regexp.MustCompile(`"((?:POST|PUT|PATCH|DELETE) /[^"]*)"`)

// registeredWriteRoutes reads the route patterns out of this package's source.
// http.ServeMux does not expose what was registered, and a source scan also
// catches a route mounted behind a condition the test server does not meet.
func registeredWriteRoutes(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range writeRouteRe.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = f
		}
	}
	if len(out) < 100 {
		t.Fatalf("found only %d write routes — the scan is broken, not the router", len(out))
	}
	return out
}

func TestEveryWriteRouteIsClassified(t *testing.T) {
	registered := registeredWriteRoutes(t)
	var missing, stale []string
	for route, file := range registered {
		if _, ok := writeRouteGates[route]; !ok {
			missing = append(missing, route+"  ("+file+")")
		}
	}
	for route, g := range writeRouteGates {
		if _, ok := registered[route]; !ok {
			stale = append(stale, route)
		}
		if strings.TrimSpace(g.note) == "" {
			t.Errorf("%s has no note — say what the gate checks", route)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, r := range missing {
		t.Errorf("write route is not classified in writeRouteGates: %s\n"+
			"\tdecide who may call it (see the gateClass constants) and add it — do not reach for clsAnywhere", r)
	}
	for _, r := range stale {
		t.Errorf("writeRouteGates names a route that is no longer registered: %s", r)
	}
}

var pathParamRe = regexp.MustCompile(`\{[^}]+\}`)

func probe(route string) (method, path string) {
	method, path, _ = strings.Cut(route, " ")
	return method, pathParamRe.ReplaceAllString(path, "x")
}

// An administrator of ONE agency is refused by every route the table calls
// global or composeAdmin — and so is gMixed, who additionally reaches every
// scope as a viewer. This is the test that fails if a requireGlobal is ever
// weakened back to requirePerm.
func TestInstallWideRoutesRefuseAnAdminOfOneAgency(t *testing.T) {
	h, _ := gateServer(t)
	for route, g := range writeRouteGates {
		if g.class != clsGlobal && g.class != clsComposeAdmin {
			continue
		}
		method, path := probe(route)
		for _, who := range []string{gFinAdmin, gMixed} {
			if rec := gateReq(t, h, method, path, who, `{}`); rec.Code != http.StatusForbidden {
				t.Errorf("%s as %s = %d, want 403 — the table says %s (%s)", route, who, rec.Code, g.class, strings.TrimSpace(rec.Body.String()))
			}
		}
	}
}

// A viewer of EVERY scope holds no verb, so no gated write may succeed for
// them however far they can read. This is the permission-blind mistake in its
// general form: reach is not authority.
func TestAViewerOfEveryScopeCanChangeNothing(t *testing.T) {
	h, _ := gateServer(t)
	for route, g := range writeRouteGates {
		switch g.class {
		case clsGlobal, clsComposeAdmin, clsObject, clsVerb, clsCompose, clsAnywhere:
		default:
			continue // not a permission gate: self, open, visible, or not a session
		}
		if route == "PUT /api/v1/" {
			continue // the dynamic {kind}-agencies prefix; its four routes share one handler
		}
		method, path := probe(route)
		rec := gateReq(t, h, method, path, gAllViewer, `{}`)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("%s as a viewer of every scope = %d, want 403 or 404 (%s)", route, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

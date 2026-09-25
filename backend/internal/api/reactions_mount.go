package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/reaction"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountReactions owns the reaction authoring surface
// (the reactions-update plan, RX-12 — Phase C).
//
// A reaction is a named entry ON a definition — a job or a workflow — saying
// "when the named upstream reaches a terminal state matching on_outcome, run
// me". Phase B built the engine; this is what makes it authorable by something
// other than SQL, alongside the Git/YAML path (RX-13).
//
// Reactions are sub-resources of the definition that OWNS them, mirroring
// schedule-defs' shape, because that is what they are: the owner tuple is
// definition_schedules' tuple exactly, cascade triggers included.
//
// # Why writes are admin-only
//
// requireCompose, the same gate as schedule-defs and calendars, for a sharper
// version of the same reason: a reaction is a standing instruction to run
// something, and unlike a schedule it has no visible clock. Someone who can
// author one can make any definition run in response to any other — which is
// also why the upstream must be READABLE by the author (§2.9): without that
// check a low-privilege author could bind to a high-privilege job and use fire
// timing as a covert channel for "did the quarterly close succeed?".
//
//	GET    /api/v1/reactions                              every reaction (the edge list)
//	GET    /api/v1/reactions/{ownerKind}/{ownerName}      one definition's reactions
//	PUT    /api/v1/reactions/{ownerKind}/{ownerName}      replace them wholesale
//	DELETE /api/v1/reactions/{ownerKind}/{ownerName}/{name}  remove one
func (s *Server) mountReactions(mux *http.ServeMux) {
	requireSession := s.auth.RequireSession
	mux.Handle("GET /api/v1/reactions", requireSession(http.HandlerFunc(s.listReactions)))
	mux.Handle("GET /api/v1/reactions/{ownerKind}/{ownerName}", requireSession(http.HandlerFunc(s.getDefinitionReactions)))
	mux.Handle("PUT /api/v1/reactions/{ownerKind}/{ownerName}", s.requireComposeAdmin(http.HandlerFunc(s.replaceDefinitionReactions)))
	mux.Handle("DELETE /api/v1/reactions/{ownerKind}/{ownerName}/{name}", s.requireComposeAdmin(http.HandlerFunc(s.deleteReaction)))
}

// ErrCodeReactionsWatching is the error code both definition-delete routes
// return when reactions watch the definition and ?force=true was not passed
// (RX-24). It is distinct from the generic "conflict" the git-source refusal
// carries so a client can tell the two apart — one is clearable with ?force=true
// and the other never is.
const ErrCodeReactionsWatching = "reactions_watching"

// reactionInput is one authored reaction in a PUT body.
type reactionInput struct {
	Name                    string `json:"name"`
	OnKind                  string `json:"onKind"`
	OnName                  string `json:"onName"`
	OnSource                string `json:"onSource"`
	OnOutcome               string `json:"onOutcome"`
	DelaySeconds            int    `json:"delaySeconds"`
	MinIntervalSeconds      int    `json:"minIntervalSeconds"`
	IncludeWorkflowChildren bool   `json:"includeWorkflowChildren"`
	Enabled                 *bool  `json:"enabled"`
}

// reactionOut is one reaction as served.
type reactionOut struct {
	OwnerKind               string `json:"ownerKind"`
	OwnerName               string `json:"ownerName"`
	OwnerSource             string `json:"ownerSource"`
	Name                    string `json:"name"`
	OnKind                  string `json:"onKind"`
	OnName                  string `json:"onName"`
	OnSource                string `json:"onSource"`
	OnOutcome               string `json:"onOutcome"`
	DelaySeconds            int    `json:"delaySeconds"`
	MinIntervalSeconds      int    `json:"minIntervalSeconds"`
	IncludeWorkflowChildren bool   `json:"includeWorkflowChildren"`
	Enabled                 bool   `json:"enabled"`
	Position                int    `json:"position"`
	// Missing reports that the WATCHED definition no longer exists. A dangling
	// reaction never fires; it is rendered as missing rather than quietly inert,
	// which is the CAL 1C force-deleted-binding treatment. Dangling is a
	// SUPPORTED state (the Git prune cannot 409), so this is a normal field and
	// not an error condition.
	Missing bool `json:"missing"`
}

func (s *Server) listReactions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.loadReactions(r, "", "", "")
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rows)
}

func (s *Server) getDefinitionReactions(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("ownerKind"), r.PathValue("ownerName")
	if kind != "job" && kind != "workflow" {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "ownerKind must be job or workflow")
		return
	}
	// Resolve the source so a name present in BOTH namespaces does not merge two
	// definitions' reactions into one list. A missing definition simply yields
	// no rows rather than a 404: a caller listing reactions for something that
	// does not exist wants an empty list, not an error.
	source, _ := s.definitionSource(r, kind, name)
	rows, err := s.loadReactions(r, kind, source, name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rows)
}

// replaceDefinitionReactions replaces one definition's whole reaction list.
//
// Wholesale replacement, like the schedule-entry compose paths, rather than
// per-entry POST/PUT: a reaction's identity is (owner, name) and the set is
// small, so a replace is both simpler to reason about and free of the
// partial-update races a per-entry API invites.
func (s *Server) replaceDefinitionReactions(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	kind, name := r.PathValue("ownerKind"), r.PathValue("ownerName")
	if kind != "job" && kind != "workflow" {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "ownerKind must be job or workflow")
		return
	}
	source, ok := s.definitionSource(r, kind, name)
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found", kind+" not found")
		return
	}
	// FX-A4: no authoring a standing instruction onto a definition in the recycle
	// bin — it would go live, unannounced, the moment the definition was restored.
	// Matches updateJob/updateWorkflow/updateScheduleDef, which all refuse to edit
	// a binned definition. Listing and deleting stay open (see definitionIsBinned).
	if s.definitionIsBinned(r, kind, source, name) {
		httpx.Fail(w, http.StatusConflict, "conflict",
			kind+" is in the recycle bin — restore it before editing its reactions")
		return
	}
	if !s.requireCronomiconOwner(w, kind, source, name) {
		return
	}

	var body struct {
		Reactions []reactionInput `json:"reactions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}

	owner := reactionRef{Source: source, Kind: kind, Name: name}
	if err := s.validateReactions(r, owner, body.Reactions); err != nil {
		if ve, ok := errors.AsType[*reactionValidationError](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", ve.Error())
			return
		}
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM reactions WHERE owner_source=? AND owner_kind=? AND owner_name=?`,
		source, kind, name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	for i, in := range body.Reactions {
		enabled := 1
		if in.Enabled != nil && !*in.Enabled {
			enabled = 0
		}
		onSource := in.OnSource
		if onSource == "" {
			onSource = "git"
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO reactions (owner_source, owner_kind, owner_name, name,
			                       on_source, on_kind, on_name, on_outcome,
			                       delay_seconds, min_interval_seconds,
			                       include_workflow_children, enabled, position,
			                       owner_uid, on_uid)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,
				CASE ?
				  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
				  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
				END,
				CASE ?
				  WHEN 'job'      THEN (SELECT uid FROM jobs      WHERE name = ? AND source = ?)
				  WHEN 'workflow' THEN (SELECT uid FROM workflows WHERE name = ? AND source = ?)
				END)`,
			source, kind, name, in.Name,
			onSource, in.OnKind, in.OnName, in.OnOutcome,
			in.DelaySeconds, in.MinIntervalSeconds,
			boolToInt(in.IncludeWorkflowChildren), enabled, i,
			kind, name, source, name, source,
			in.OnKind, in.OnName, onSource, in.OnName, onSource); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	s.auditReaction(r, id.Email, "Replaced", kind+"/"+name,
		"Reactions replaced ("+strconv.Itoa(len(body.Reactions))+" entries)")
	rows, err := s.loadReactions(r, kind, source, name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rows)
}

func (s *Server) deleteReaction(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	kind, name, entry := r.PathValue("ownerKind"), r.PathValue("ownerName"), r.PathValue("name")
	if kind != "job" && kind != "workflow" {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "ownerKind must be job or workflow")
		return
	}
	source, ok := s.definitionSource(r, kind, name)
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found", kind+" not found")
		return
	}
	if !s.requireCronomiconOwner(w, kind, source, name) {
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		`DELETE FROM reactions WHERE owner_source=? AND owner_kind=? AND owner_name=? AND name=?`,
		source, kind, name, entry)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "reaction not found")
		return
	}
	s.auditReaction(r, id.Email, "Deleted", kind+"/"+name, "Reaction "+entry+" deleted")
	w.WriteHeader(http.StatusNoContent)
}

// ── validation ─────────────────────────────────────────────────────────────

type reactionValidationError struct{ msg string }

func (e *reactionValidationError) Error() string { return e.msg }

func badReaction(format string) error { return &reactionValidationError{msg: format} }

// reactionNameRe is the YAML path's slug rule, shared so the two authoring
// surfaces cannot drift.
var reactionNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// reactionIntervalCeiling is the longest min_interval that can still be
// anchored, in seconds: the runs retention window, which reaction_deliveries
// shares.
func (s *Server) reactionIntervalCeiling() int {
	days := 90
	if s.cfg != nil && s.cfg.RetentionRunsDays > 0 {
		days = s.cfg.RetentionRunsDays
	}
	return days * 24 * 60 * 60
}

// reactionRef identifies one side of a reaction edge.
type reactionRef struct{ Source, Kind, Name string }

func (r reactionRef) String() string { return r.Kind + ":" + r.Source + "/" + r.Name }

// validateReactions applies every authoring-time rule (§2.8, §2.9).
func (s *Server) validateReactions(r *http.Request, owner reactionRef, in []reactionInput) error {
	id, _ := auth.IdentityFrom(r.Context())
	seen := map[string]bool{}
	for _, x := range in {
		if strings.TrimSpace(x.Name) == "" {
			return badReaction("every reaction needs a name")
		}
		// The SAME slug rule the YAML path enforces. Without this the two
		// authoring surfaces disagree: a name accepted here is rejected the
		// moment the same reaction is expressed in a repo, which turns "move
		// this into Git" into a debugging session.
		if !reactionNameRe.MatchString(x.Name) {
			return badReaction(x.Name + ": name must be a slug — lowercase letters, digits, '-' or '_'")
		}
		if seen[x.Name] {
			return badReaction("duplicate reaction name " + x.Name)
		}
		seen[x.Name] = true

		if x.OnKind != "job" && x.OnKind != "workflow" {
			return badReaction(x.Name + ": onKind must be job or workflow")
		}
		if strings.TrimSpace(x.OnName) == "" {
			return badReaction(x.Name + ": onName is required")
		}
		if !reaction.ValidOnOutcome(x.OnOutcome) {
			return badReaction(x.Name + ": onOutcome must be one of success, failure, stopped, any")
		}
		if x.DelaySeconds < 0 || x.MinIntervalSeconds < 0 {
			return badReaction(x.Name + ": delaySeconds and minIntervalSeconds cannot be negative")
		}
		// RX-21 — the rate brake anchors on the last FIRED delivery, and that
		// table is pruned with runs. An interval longer than the retention window
		// silently disarms itself once the anchor row is swept, which is worse
		// than an error naming the limit.
		if max := s.reactionIntervalCeiling(); x.MinIntervalSeconds > max {
			return badReaction(fmt.Sprintf(
				"%s: minIntervalSeconds %d exceeds the %d-second run-retention window, so the "+
					"brake would silently disarm once its anchor is pruned",
				x.Name, x.MinIntervalSeconds, max))
		}

		onSource := x.OnSource
		if onSource == "" {
			onSource = "git"
		}
		upstream := reactionRef{Source: onSource, Kind: x.OnKind, Name: x.OnName}

		// Self-reference is rejected outright: a definition that reacts to its
		// own completion is an unconditional infinite loop, and the depth ceiling
		// should be a backstop for cycles nobody can see rather than the thing
		// that catches the obvious one.
		if upstream == owner {
			return badReaction(x.Name + ": a definition cannot react to itself")
		}

		exists, err := s.definitionExists(r, upstream)
		if err != nil {
			return err
		}
		if !exists {
			return badReaction(x.Name + ": no such " + x.OnKind + " " + x.OnName +
				" (source " + onSource + ")")
		}

		// §2.9 — the author must be able to READ the upstream. Authority for a
		// reaction comes from its having been authored, so this is where the
		// check belongs. Without it a low-privilege author binds to a
		// high-privilege definition and reads fire timing as a covert channel.
		//
		// NOTE this holds on the API path only. Git sync has no author identity
		// to check, and that is a stated trust boundary rather than a gap: repo
		// write access already confers the ability to define jobs that run as
		// the platform, which strictly dominates the covert channel.
		readable, err := s.definitionReadable(r, id, upstream)
		if err != nil {
			return err
		}
		if !readable {
			return badReaction(x.Name + ": you do not have access to " + upstream.String())
		}
	}

	// Cycle detection runs once, over the whole proposed set plus every OTHER
	// definition's stored edges.
	return s.detectReactionCycle(r, owner, in)
}

// detectReactionCycle rejects an edge set that closes a loop (§2.8).
//
// The edge set is CROSS-PLANE by necessity: reactions are authorable in both
// YAML and the API, so this must union the proposed edges with every stored
// edge regardless of source. That means an API write can be rejected because of
// a Git-authored reaction (and vice versa at sync time), so the error names the
// full path — an operator who cannot see the other plane's edge would otherwise
// have no way to understand the refusal.
//
// Two authors landing simultaneously on different planes can still create a
// cycle neither validation saw. That TOCTOU is what the runtime depth ceiling
// exists for; it is a backstop, not a duplicate of this check.
func (s *Server) detectReactionCycle(r *http.Request, owner reactionRef, proposed []reactionInput) error {
	// edges[downstream] = upstreams it waits on. A reaction says "when UPSTREAM
	// finishes, run OWNER", so the graph edge runs upstream → owner.
	edges := map[string][]string{}

	// DISABLED edges are included, deliberately. A disabled reaction is a latent
	// edge, not an absent one: filtering them out would let an operator author a
	// cycle in two steps — disable one edge, add the edge that closes the loop
	// (now accepted), re-enable — and the re-enable is an innocuous-looking
	// toggle with no validation of its own. The depth ceiling would catch the
	// runaway at runtime, but a cycle you can construct through the front door
	// should be refused at the front door.
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT owner_source, owner_kind, owner_name, on_source, on_kind, on_name
		  FROM reactions`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var os, ok2, on, ns, nk, nn string
		if err := rows.Scan(&os, &ok2, &on, &ns, &nk, &nn); err != nil {
			continue
		}
		down := reactionRef{Source: os, Kind: ok2, Name: on}
		if down == owner {
			continue // replaced wholesale by `proposed` below
		}
		edges[reactionRef{Source: ns, Kind: nk, Name: nn}.String()] =
			append(edges[reactionRef{Source: ns, Kind: nk, Name: nn}.String()], down.String())
	}
	rows.Close()

	for _, x := range proposed {
		onSource := x.OnSource
		if onSource == "" {
			onSource = "git"
		}
		up := reactionRef{Source: onSource, Kind: x.OnKind, Name: x.OnName}
		edges[up.String()] = append(edges[up.String()], owner.String())
	}

	// Depth-first search from the owner: if following edges forward returns to
	// it, the proposed set closes a cycle.
	var path []string
	state := map[string]int{} // 0 unvisited, 1 on-stack, 2 done
	var walk func(node string) []string
	walk = func(node string) []string {
		state[node] = 1
		path = append(path, node)
		for _, next := range edges[node] {
			switch state[next] {
			case 1:
				return append(append([]string{}, path...), next)
			case 0:
				if cyc := walk(next); cyc != nil {
					return cyc
				}
			}
		}
		path = path[:len(path)-1]
		state[node] = 2
		return nil
	}
	if cyc := walk(owner.String()); cyc != nil {
		return badReaction("these reactions would create a cycle: " + strings.Join(cyc, " → "))
	}
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────────

// definitionSource resolves which source's definition a bare name refers to.
//
// The two sources are disjoint namespaces, so the SAME name can exist in both.
// 'cronomicon' sorts before 'git' and therefore wins, which is the right
// preference for a write path: the in-app definition is the one this API can
// own. A git-only name still resolves to 'git', and writeGuard below is what
// stops that from becoming a silent data loss.
func (s *Server) definitionSource(r *http.Request, kind, name string) (string, bool) {
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	var source string
	//nolint:gosec // table is a package-local constant chosen by the kind switch
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT source FROM "+table+" WHERE name = ? ORDER BY source LIMIT 1", name).Scan(&source); err != nil {
		return "", false
	}
	return source, true
}

// definitionIsBinned reports whether a resolved definition is in the recycle bin.
//
// FX-A4 gates reaction AUTHORING on this, and deliberately nothing else. Making
// definitionSource itself filter binned rows looked equivalent and was not: it
// also blanked the LIST and 404'd the DELETE, so reactions written before the bin
// became invisible and unremovable while still going live on restore — sealing in
// exactly the hazard the authoring block exists to prevent. Reading and removing a
// binned definition's reactions must keep working; only writing new ones is
// refused.
func (s *Server) definitionIsBinned(r *http.Request, kind, source, name string) bool {
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	var binned int
	//nolint:gosec // table is a package-local constant chosen by the kind switch
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT deleted_at IS NOT NULL FROM "+table+" WHERE source = ? AND name = ?",
		source, name).Scan(&binned); err != nil {
		return false
	}
	return binned == 1
}

// requireCronomiconOwner refuses to author reactions onto a GIT-source definition.
//
// Without this the write appears to succeed and is then silently destroyed: a
// reaction stored with owner_source='git' sits in exactly the rows
// writeDefinitionReactions clears on every sync (a source-scoped replace, so
// that a sync can never wipe an operator's in-app rows). The operator would see
// their reaction land, work, and then vanish at the next sync with nothing
// anywhere saying why.
//
// Refusing matches how every other in-app authoring path treats git rows —
// compose 409s with "only cronomicon-source jobs are deletable in-app" — and it
// states the real rule: a Git-defined definition's configuration belongs in
// Git, which is what RX-13's YAML surface is for.
func (s *Server) requireCronomiconOwner(w http.ResponseWriter, kind, source, name string) bool {
	if source == "cronomicon" {
		return true
	}
	httpx.Fail(w, http.StatusConflict, "conflict",
		"reactions on the git-source "+kind+" "+name+" are authored in Git (spec.reactions), "+
			"not in-app: an in-app write here would be replaced by the next sync")
	return false
}

func (s *Server) definitionExists(r *http.Request, ref reactionRef) (bool, error) {
	table := "jobs"
	if ref.Kind == "workflow" {
		table = "workflows"
	}
	var one int
	//nolint:gosec // table is a package-local constant chosen by the kind switch
	// RH: a binned definition counts as absent here. A reaction pointing at one
	// will never fire (the reactor's own gates filter deleted_at), so reporting it
	// as present would render a dangling edge as healthy — which is the exact
	// misreading the `missing` flag exists to prevent.
	err := s.db.QueryRowContext(r.Context(),
		"SELECT 1 FROM "+table+" WHERE source = ? AND name = ? AND deleted_at IS NULL", ref.Source, ref.Name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// definitionReadable answers §2.9's can-the-author-see-it question against the
// upstream's own scope, using the same scope predicate every read path uses.
func (s *Server) definitionReadable(r *http.Request, id auth.Identity, ref reactionRef) (bool, error) {
	return s.definitionReadableByScope(r, id, true, ref)
}

// definitionReadableByScope is the shared SU-2 predicate for both the authoring
// check and the list filter. An unrestricted actor sees everything; everyone
// else is gated by ScopeReadable, which still admits global (NULL) scopes —
// the scheduleOwnerReadable shape.
func (s *Server) definitionReadableByScope(r *http.Request, id auth.Identity, hasID bool, ref reactionRef) (bool, error) {
	if hasID && id.Unrestricted() {
		return true, nil
	}
	if ref.Kind == "workflow" {
		// A workflow definition carries no scope of its own (scope is a property
		// of the run), so there is nothing narrower than session access to check.
		return true, nil
	}
	var scope sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT scope FROM jobs WHERE source = ? AND name = ?`, ref.Source, ref.Name).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !scope.Valid || scope.String == "" {
		// An unscoped job is visible to anyone with a session, matching the job
		// list's own read filter (scope IS NULL rows are global).
		return true, nil
	}
	return auth.ScopeReadable(id, scope.String), nil
}

// loadReactions reads reactions (all, or one definition's) and marks dangling
// upstreams. kind=="" means every reaction; source=="" means every source.
//
// The per-definition path passes the RESOLVED source, not just the name. The two
// sources are disjoint namespaces, so a name present in both would otherwise
// merge two definitions' reactions into one list — and since the write path
// resolves to a single source, a read-modify-write would then copy the git
// definition's entries in as in-app rows. The next sync restores the git ones,
// leaving both, and one upstream completion produces two runs.
func (s *Server) loadReactions(r *http.Request, kind, source, name string) ([]reactionOut, error) {
	q := `SELECT owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name,
	             on_outcome, delay_seconds, min_interval_seconds,
	             include_workflow_children, enabled, position
	        FROM reactions`
	var args []any
	if kind != "" {
		q += ` WHERE owner_kind = ? AND owner_name = ?`
		args = append(args, kind, name)
		if source != "" {
			q += ` AND owner_source = ?`
			args = append(args, source)
		}
	}
	rows, err := s.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		return nil, err
	}
	out := []reactionOut{}
	for rows.Next() {
		var x reactionOut
		var includeChildren, enabled int
		if err := rows.Scan(&x.OwnerSource, &x.OwnerKind, &x.OwnerName, &x.Name,
			&x.OnSource, &x.OnKind, &x.OnName, &x.OnOutcome,
			&x.DelaySeconds, &x.MinIntervalSeconds, &includeChildren, &enabled, &x.Position); err != nil {
			continue
		}
		x.IncludeWorkflowChildren = includeChildren == 1
		x.Enabled = enabled == 1
		out = append(out, x)
	}
	rows.Close() // drain BEFORE the per-row existence probes (SQLite pool rule)

	// SU-2 — drop edges the caller cannot read, matching what the schedule edge
	// list and the pending-run list already do.
	//
	// BOTH ends are checked, not just the owner. An edge names two definitions,
	// so filtering on the owner alone still discloses every scoped job that
	// appears as an UPSTREAM — the reaction's whole purpose is to name one. That
	// is a strictly larger disclosure than the fire-timing channel §2.9's
	// authoring check exists to close, and it would have leaked exactly the
	// names an authoring check refuses to let you bind to.
	id, hasID := auth.IdentityFrom(r.Context())
	readable := out[:0]
	for _, x := range out {
		ownerOK, err := s.definitionReadableByScope(r, id, hasID, reactionRef{
			Source: x.OwnerSource, Kind: x.OwnerKind, Name: x.OwnerName})
		if err != nil {
			return nil, err
		}
		if !ownerOK {
			continue
		}
		upstreamOK, err := s.definitionReadableByScope(r, id, hasID, reactionRef{
			Source: x.OnSource, Kind: x.OnKind, Name: x.OnName})
		if err != nil {
			return nil, err
		}
		// A DANGLING upstream is readable by definition — there is no row whose
		// scope could deny it, and hiding the edge would make the one state the
		// tab exists to flag invisible to exactly the operator who owns it.
		if !upstreamOK {
			exists, eerr := s.definitionExists(r, reactionRef{
				Source: x.OnSource, Kind: x.OnKind, Name: x.OnName})
			if eerr != nil {
				return nil, eerr
			}
			if exists {
				continue
			}
		}
		readable = append(readable, x)
	}
	out = readable

	for i := range out {
		exists, err := s.definitionExists(r, reactionRef{
			Source: out[i].OnSource, Kind: out[i].OnKind, Name: out[i].OnName})
		if err != nil {
			return nil, err
		}
		out[i].Missing = !exists
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].OwnerName != out[b].OwnerName {
			return out[a].OwnerName < out[b].OwnerName
		}
		return out[a].Position < out[b].Position
	})
	return out, nil
}

// reactionsWatching lists the reactions pointing AT a definition — the RX-24
// delete guard's question.
func (s *Server) reactionsWatching(r *http.Request, kind, source, name string) ([]string, error) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT owner_kind, owner_name, name FROM reactions
		 WHERE on_kind = ? AND on_source = ? AND on_name = ?`, kind, source, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ok2, on, nm string
		if err := rows.Scan(&ok2, &on, &nm); err != nil {
			continue
		}
		out = append(out, ok2+":"+on+"/"+nm)
	}
	return out, nil
}

// reactionDeleteGuard implements RX-24. Returns a non-empty refusal message
// when reactions watch this definition and ?force=true was not passed.
//
// This is the CAL-22 shape, down to the "leave those dangling" wording, and it
// exists because the two delete paths CANNOT behave the same way. Deleting a
// watched definition must never cascade its reactions away — a reaction that
// vanishes when somebody removes an unrelated upstream is a silent capability
// loss. But the Git sync prune has no request to fail and no operator to ask,
// so it always succeeds and leaves the reactions dangling.
//
// Dangling is therefore a SUPPORTED state regardless of what this guard does.
// The 409 does not make it impossible; it makes it rare and deliberate on the
// one path where a human is present to be asked.
//
// The refusal carries its OWN error code (ErrCodeReactionsWatching) rather than
// the generic "conflict". Both delete routes can 409 for two unrelated reasons —
// git-source, and this — and a client that cannot tell them apart has to guess:
// the console guessed wrong for a whole release, telling operators an
// cronomicon-source job was Git-authored. A distinguishable code is also what lets
// the UI offer "delete anyway" for exactly the refusal that ?force=true clears,
// and not for the one it cannot.
func (s *Server) reactionDeleteGuard(r *http.Request, kind, source, name string) (string, error) {
	watching, err := s.reactionsWatching(r, kind, source, name)
	if err != nil {
		return "", err
	}
	if len(watching) == 0 || r.URL.Query().Get("force") == "true" {
		return "", nil
	}
	return kind + " is watched by " + strconv.Itoa(len(watching)) + " reaction(s): " +
		strings.Join(watching, ", ") +
		" — pass ?force=true to delete and leave those reactions dangling", nil
}

func (s *Server) auditReaction(r *http.Request, actor, action, target, summary string) {
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Reactions", action, target, summary)
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Reactions", Action: action, Target: target, Summary: summary,
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

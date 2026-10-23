package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/reaction"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
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
	// Compose on the agency of the definition the reactions are of (2.4.2, the
	// owner's answer to question 61), as calendars and schedules since §0a: a
	// reaction's upstream is its own agency's (upstreamExistsFor), so it can no
	// longer trigger another agency's definition, which is what kept these
	// routes an administrator's of every agency.
	mux.Handle("PUT /api/v1/reactions/{ownerKind}/{ownerName}", s.requireCompose(http.HandlerFunc(s.replaceDefinitionReactions)))
	mux.Handle("DELETE /api/v1/reactions/{ownerKind}/{ownerName}/{name}", s.requireCompose(http.HandlerFunc(s.deleteReaction)))
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
	OwnerKind   string `json:"ownerKind"`
	OwnerName   string `json:"ownerName"`
	OwnerSource string `json:"ownerSource"`
	// OwnerUID says which definition of the name the row is, for the agency
	// the upstream is looked for in (I2); not shown.
	OwnerUID                string `json:"-"`
	OnUID                   string `json:"-"`
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
	//
	// With ?uid= it is ONE definition's reactions (present defect 36): a name
	// can be held by a definition in each agency, and by the name alone their
	// lists are one list.
	if uid := r.URL.Query().Get("uid"); uid != "" {
		owner, status := s.reactionOwner(r, kind, name)
		if status != 0 {
			httpx.JSON(w, http.StatusOK, []reactionOut{})
			return
		}
		cond, args := owner.rowsSQL(kind, name)
		rows, err := s.loadReactionsWhere(r, cond, args)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		httpx.JSON(w, http.StatusOK, rows)
		return
	}
	source, _ := s.definitionSource(r, kind, name)
	rows, err := s.loadReactions(r, kind, source, name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rows)
}

// reactionOwner is the ONE definition a per-definition reactions route
// addresses (present defect 36).
//
// The routes name a definition by its kind and name, and a name can be held by
// a definition in each agency. The write found "the" definition by the name,
// replaced every reaction row of that source, kind and NAME, and stamped the
// new rows' owner only when one definition held the name: saving one twin's
// reactions deleted the other twin's, and the rows it wrote belonged to
// neither by identity.
//
// ?uid= says which definition. Without it the name must be enough: where more
// than one definition of the source holds it, a write is refused (409) and
// told to say which.
type reactionOwner struct {
	source string
	uid    string // "" only for a definition that records none
	// alone: no other definition of the source holds the name, so a reaction
	// row of the name that records no owner is this definition's.
	alone bool
}

// reactionOwner resolves it. status is 0, or the HTTP status of the refusal:
// 404 (no such definition), 409 (the name does not say which).
func (s *Server) reactionOwner(r *http.Request, kind, name string) (reactionOwner, int) {
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	uid := r.URL.Query().Get("uid")
	var o reactionOwner
	if uid != "" {
		//nolint:gosec // table is a package-local constant chosen by the kind switch
		if err := s.db.QueryRowContext(r.Context(),
			"SELECT source FROM "+table+" WHERE uid = ? AND name = ?", uid, name).Scan(&o.source); err != nil {
			return o, http.StatusNotFound
		}
		o.uid = uid
	} else {
		src, ok := s.definitionSource(r, kind, name)
		if !ok {
			return o, http.StatusNotFound
		}
		o.source = src
	}
	//nolint:gosec // as above
	rows, err := s.db.QueryContext(r.Context(),
		"SELECT COALESCE(uid, '') FROM "+table+" WHERE source = ? AND name = ?", o.source, name)
	if err != nil {
		return o, http.StatusNotFound
	}
	defer rows.Close()
	var uids []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			uids = append(uids, u)
		}
	}
	o.alone = len(uids) == 1
	if uid == "" {
		if !o.alone {
			return o, http.StatusConflict
		}
		o.uid = uids[0]
	}
	return o, 0
}

// rowsSQL is the condition over `reactions` for the rows that are this
// definition's, with its arguments: the rows that record it as their owner,
// and, where no other definition holds the name, the rows of the name that
// record no owner (written before a reaction recorded one).
func (o reactionOwner) rowsSQL(kind, name string) (string, []any) {
	switch {
	case o.uid == "":
		return "owner_source = ? AND owner_kind = ? AND owner_name = ?", []any{o.source, kind, name}
	case o.alone:
		return "owner_kind = ? AND (owner_uid = ? OR (owner_uid IS NULL AND owner_source = ? AND owner_name = ?))",
			[]any{kind, o.uid, o.source, name}
	default:
		return "owner_kind = ? AND owner_uid = ?", []any{kind, o.uid}
	}
}

// refuseReactionOwner writes the answer for a definition reactionOwner could
// not settle on.
func refuseReactionOwner(w http.ResponseWriter, status int, kind, name string) {
	if status == http.StatusConflict {
		httpx.Fail(w, http.StatusConflict, "ambiguous_definition",
			"more than one "+kind+" is called "+name+": say which by its uid (?uid=)")
		return
	}
	httpx.Fail(w, http.StatusNotFound, "not_found", kind+" not found")
}

// binned reports whether THIS definition is in the recycle bin (see
// definitionIsBinned, which asks by the name).
func (s *Server) reactionOwnerBinned(r *http.Request, kind, name string, o reactionOwner) bool {
	if o.uid == "" {
		return s.definitionIsBinned(r, kind, o.source, name)
	}
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	var binned int
	//nolint:gosec // table is a package-local constant chosen by the kind switch
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT deleted_at IS NOT NULL FROM "+table+" WHERE uid = ?", o.uid).Scan(&binned); err != nil {
		return false
	}
	return binned == 1
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
	owner, status := s.reactionOwner(r, kind, name)
	if status != 0 {
		refuseReactionOwner(w, status, kind, name)
		return
	}
	source := owner.source
	// FX-A4: no authoring a standing instruction onto a definition in the recycle
	// bin — it would go live, unannounced, the moment the definition was restored.
	// Matches updateJob/updateWorkflow/updateScheduleDef, which all refuse to edit
	// a binned definition. Listing and deleting stay open (see definitionIsBinned).
	if s.reactionOwnerBinned(r, kind, name, owner) {
		httpx.Fail(w, http.StatusConflict, "conflict",
			kind+" is in the recycle bin — restore it before editing its reactions")
		return
	}
	if !s.requireCronomiconOwner(w, kind, source, name) {
		return
	}
	if !s.requireReactionOwnerCompose(w, r, reactionRef{Source: source, Kind: kind, Name: name, UID: owner.uid}) {
		return
	}

	var body struct {
		Reactions []reactionInput `json:"reactions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}

	ownerRef := reactionRef{Source: source, Kind: kind, Name: name, UID: owner.uid}
	if err := s.validateReactions(r, ownerRef, body.Reactions); err != nil {
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

	// THIS definition's rows, and no other definition's of the name.
	mine, mineArgs := owner.rowsSQL(kind, name)
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM reactions WHERE `+mine, mineArgs...); err != nil {
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
				-- The owner is the definition the route addressed, by its
				-- identity. (It was "the one definition of the name", and NULL
				-- when two held it.)
				NULLIF(?, ''),
				CASE ?
				  WHEN 'job'      THEN (SELECT CASE WHEN COUNT(*) = 1 THEN MAX(uid) END FROM jobs      WHERE name = ? AND source = ?)
				  WHEN 'workflow' THEN (SELECT CASE WHEN COUNT(*) = 1 THEN MAX(uid) END FROM workflows WHERE name = ? AND source = ?)
				END)`,
			source, kind, name, in.Name,
			onSource, in.OnKind, in.OnName, in.OnOutcome,
			in.DelaySeconds, in.MinIntervalSeconds,
			boolToInt(in.IncludeWorkflowChildren), enabled, i,
			owner.uid,
			in.OnKind, in.OnName, onSource, in.OnName, onSource); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	}
	// GR-16 — a GIT upstream's identity is not "the one definition of that name,
	// wherever it is", which with a repository per agency can be another
	// agency's. It is the one the engine would find by the name: in the owner's
	// agency's repository, then in Global's. The same expression the engine
	// uses, so the two cannot disagree; a name neither holds is left with no
	// identity and fires on nothing.
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE reactions SET on_uid = `+scheduler.ReactionUpstreamByNameSQL+`
		 WHERE `+mine+` AND on_source = 'git'`, mineArgs...); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	s.auditReaction(r, s.definitionAgency(r.Context(), kind, owner.uid), id.Email, "Replaced", kind+"/"+name,
		"Reactions replaced ("+strconv.Itoa(len(body.Reactions))+" entries)")
	rows, err := s.loadReactionsWhere(r, mine, mineArgs)
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
	owner, status := s.reactionOwner(r, kind, name)
	if status != 0 {
		refuseReactionOwner(w, status, kind, name)
		return
	}
	if !s.requireCronomiconOwner(w, kind, owner.source, name) {
		return
	}
	if !s.requireReactionOwnerCompose(w, r, reactionRef{Source: owner.source, Kind: kind, Name: name, UID: owner.uid}) {
		return
	}
	mine, mineArgs := owner.rowsSQL(kind, name)
	res, err := s.db.ExecContext(r.Context(),
		`DELETE FROM reactions WHERE `+mine+` AND name = ?`, append(mineArgs, entry)...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "reaction not found")
		return
	}
	s.auditReaction(r, s.definitionAgency(r.Context(), kind, owner.uid), id.Email, "Deleted", kind+"/"+name, "Reaction "+entry+" deleted")
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
// reactionRef names a definition. UID, when set, says which definition of
// the name (I2: a name may be held in each agency); it is not part of the
// String key.
type reactionRef struct{ Source, Kind, Name, UID string }

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
		if upstream.Source == owner.Source && upstream.Kind == owner.Kind && upstream.Name == owner.Name {
			return badReaction(x.Name + ": a definition cannot react to itself")
		}

		// For the OWNER: a Git name means the definition the engine would find
		// for it (GR-16), not any repository's. Saved against a name that only
		// another agency's repository holds, the reaction would be stamped with
		// no upstream, never fire, and be shown as healthy.
		exists, err := s.upstreamExistsFor(r, owner, upstream)
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

// upstreamExistsFor is definitionExists as the reaction's OWNER would find the
// upstream: a definition of the owner's agency, and no other's (§0a, IN-8). A
// Git one is of the owner agency's repository (from GR-16 until §0a, Global's
// next); one built in the app is the owner agency's (until §0a, any of the
// name). This is the question the engine asks when the upstream finishes, so
// "missing" here is "will not fire".
func (s *Server) upstreamExistsFor(r *http.Request, owner, upstream reactionRef) (bool, error) {
	agency := s.reactionOwnerAgency(r.Context(), owner)
	var one int
	var err error
	switch {
	case upstream.Source == "git":
		table := "jobs"
		if upstream.Kind == "workflow" {
			table = "workflows"
		}
		//nolint:gosec // the table is one of two literals
		err = s.db.QueryRowContext(r.Context(),
			"SELECT 1 FROM "+table+" WHERE source = 'git' AND name = ? AND deleted_at IS NULL AND COALESCE(repo_id, ?) = ? LIMIT 1",
			upstream.Name, repoid.Global, s.agencyRepo(r.Context(), agency)).Scan(&one)
	case upstream.Kind == "workflow":
		err = s.db.QueryRowContext(r.Context(),
			`SELECT 1 FROM workflows WHERE source = ? AND name = ? AND deleted_at IS NULL AND owner_agency = ? LIMIT 1`,
			upstream.Source, upstream.Name, agency).Scan(&one)
	default:
		err = s.db.QueryRowContext(r.Context(),
			`SELECT 1 FROM jobs WHERE source = ? AND name = ? AND deleted_at IS NULL AND `+inAppJobAgencySQL+` = ? LIMIT 1`,
			upstream.Source, upstream.Name, agency).Scan(&one)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// inAppJobAgencySQL is the agency of the row `jobs` (agencyid.JobSQL; an
// in-app job's owner since 1400).
var inAppJobAgencySQL = `COALESCE(` + agencyid.JobSQL("jobs") + `, '` + agencyid.Global + `')`

// reactionOwnerAgency is the agency of a reaction's owner: a Git owner's
// repository's, a definition built in the app's owner. By the owner's uid
// when the ref carries one (I2: a name may be held in each agency); by the
// name otherwise, for a row that records no owner.
// requireReactionOwnerCompose answers 403 unless the caller holds compose on
// the agency of the definition whose reactions are written (question 61).
func (s *Server) requireReactionOwnerCompose(w http.ResponseWriter, r *http.Request, owner reactionRef) bool {
	id, _ := auth.IdentityFrom(r.Context())
	agency := s.reactionOwnerAgency(r.Context(), owner)
	if id.CanAgency(auth.PermCompose, agency) {
		return true
	}
	s.denyEntityAgency(w, r, id, auth.PermCompose, agency,
		"a reaction belongs to its definition's agency, and writing it takes compose on that agency")
	return false
}

func (s *Server) reactionOwnerAgency(ctx context.Context, owner reactionRef) string {
	agency := agencyid.Global
	if owner.UID != "" {
		var a sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionOf("?2", "?1"), owner.Kind, owner.UID).Scan(&a)
		if a.Valid && a.String != "" {
			return a.String
		}
		return agency
	}
	switch {
	case owner.Source == "git":
		table := "jobs"
		if owner.Kind == "workflow" {
			table = "workflows"
		}
		var a sql.NullString
		//nolint:gosec // the table is one of two literals
		_ = s.db.QueryRowContext(ctx,
			"SELECT g.agency_id FROM "+table+" d JOIN git_repos g ON g.id = COALESCE(d.repo_id, ?) WHERE d.source = 'git' AND d.name = ? ORDER BY d.rowid LIMIT 1",
			repoid.Global, owner.Name).Scan(&a)
		if a.Valid {
			agency = a.String
		}
	case owner.Kind == "workflow":
		var a sql.NullString
		_ = s.db.QueryRowContext(ctx,
			`SELECT owner_agency FROM workflows WHERE source = 'cronomicon' AND name = ? ORDER BY rowid LIMIT 1`, owner.Name).Scan(&a)
		if a.Valid {
			agency = a.String
		}
	default:
		var a sql.NullString
		_ = s.db.QueryRowContext(ctx,
			`SELECT `+inAppJobAgencySQL+` FROM jobs WHERE source = 'cronomicon' AND name = ? ORDER BY rowid LIMIT 1`, owner.Name).Scan(&a)
		if a.Valid {
			agency = a.String
		}
	}
	return agency
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
		// A workflow carries no scope of its own (scope is a property of the
		// run): it is its agency's (2.5.0 C2, CF-1; it had been everyone's).
		q := `SELECT ` + agencyid.WorkflowOwnerSQL + ` FROM workflows w WHERE w.source = ? AND w.name = ? ORDER BY w.rowid LIMIT 1`
		args := []any{ref.Source, ref.Name}
		if ref.UID != "" {
			q, args = `SELECT `+agencyid.WorkflowOwnerSQL+` FROM workflows w WHERE w.uid = ?`, []any{ref.UID}
		}
		var owner string
		if err := s.db.QueryRowContext(r.Context(), q, args...).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return hasID && id.ReadsAgency(owner), nil
	}
	// The definition the ref names, by its uid when the ref carries one (I2: a
	// name may be held in each agency); by the name for a row that records none.
	var scope sql.NullString
	var owner string
	q := `SELECT scope, COALESCE(` + agencyid.JobSQL("jobs") + `, '` + agencyid.Global + `') FROM jobs WHERE source = ? AND name = ?`
	args := []any{ref.Source, ref.Name}
	if ref.UID != "" {
		q, args = `SELECT scope, COALESCE(`+agencyid.JobSQL("jobs")+`, '`+agencyid.Global+`') FROM jobs WHERE uid = ?`, []any{ref.UID}
	}
	err := s.db.QueryRowContext(r.Context(), q, args...).Scan(&scope, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The job's agency first (2.5.0 C2, CF-1: an unscoped job had been
	// everyone's), then its scope, as the job list reads it.
	if !hasID || !id.ReadsAgency(owner) {
		return false, nil
	}
	if !scope.Valid || scope.String == "" {
		return true, nil
	}
	return auth.ScopeReadableIn(id, owner, scope.String), nil
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
	cond, args := "", []any(nil)
	if kind != "" {
		cond = `owner_kind = ? AND owner_name = ?`
		args = append(args, kind, name)
		if source != "" {
			cond += ` AND owner_source = ?`
			args = append(args, source)
		}
	}
	return s.loadReactionsWhere(r, cond, args)
}

// loadReactionsWhere is loadReactions over the rows a condition picks (none:
// every row), with the same filter on what the caller may read.
func (s *Server) loadReactionsWhere(r *http.Request, cond string, args []any) ([]reactionOut, error) {
	q := `SELECT owner_source, owner_kind, owner_name, COALESCE(owner_uid, ''), COALESCE(on_uid, ''), name, on_source, on_kind, on_name,
	             on_outcome, delay_seconds, min_interval_seconds,
	             include_workflow_children, enabled, position
	        FROM reactions`
	if cond != "" {
		q += ` WHERE ` + cond
	}
	rows, err := s.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		return nil, err
	}
	out := []reactionOut{}
	for rows.Next() {
		var x reactionOut
		var includeChildren, enabled int
		if err := rows.Scan(&x.OwnerSource, &x.OwnerKind, &x.OwnerName, &x.OwnerUID, &x.OnUID, &x.Name,
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
			Source: x.OwnerSource, Kind: x.OwnerKind, Name: x.OwnerName, UID: x.OwnerUID})
		if err != nil {
			return nil, err
		}
		if !ownerOK {
			continue
		}
		upstreamOK, err := s.definitionReadableByScope(r, id, hasID, reactionRef{
			Source: x.OnSource, Kind: x.OnKind, Name: x.OnName, UID: x.OnUID})
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
		exists, err := s.upstreamExistsFor(r,
			reactionRef{Source: out[i].OwnerSource, Kind: out[i].OwnerKind, Name: out[i].OwnerName, UID: out[i].OwnerUID},
			reactionRef{Source: out[i].OnSource, Kind: out[i].OnKind, Name: out[i].OnName})
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

func (s *Server) auditReaction(r *http.Request, agency, actor, action, target, summary string) {
	_ = workflow.InsertChangeLog(r.Context(), s.db, agency, actor, "Reactions", action, target, summary)
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		AgencyID: agency, Kind: "config", Actor: actor, Category: "Reactions", Action: action, Target: target, Summary: summary,
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// A workflow built in the app is at home in its agency (2.5.0 C11, defect 79):
// the engine looks its name-only steps up there, a Git name in that agency's
// repository, an in-app name among its jobs (workflow.Engine.Home,
// AgencyHome). The composer asked about every agency's job of the name: a FIN
// workflow whose step names `dup`, which TAX also holds, was refused its save,
// its delete and its re-pin with a 403 naming TAX's scope, while the engine
// would run FIN's (found by the re-review of C11).
//
// The compose routes say whose the workflow is on the request's context, and
// the step lookups below read it: the stored owner on an edit or a delete, and
// on a create or a dry run the agency the request works in (else the agency it
// names). With none, the lookup is across agencies, as it was.

type composeAgencyKey struct{}

// withComposeAgency is r with the agency the composed workflow is at home in.
func withComposeAgency(r *http.Request, agency string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), composeAgencyKey{}, strings.TrimSpace(agency)))
}

// composeAgencyOf is the agency withComposeAgency set, or "".
func composeAgencyOf(ctx context.Context) string {
	a, _ := ctx.Value(composeAgencyKey{}).(string)
	return a
}

// creationComposeAgency is a new workflow's agency: the one the request works
// in, else the one it names, else the caller's one agency to compose in (as
// workflowSaveOwner decides a new workflow's owner); "" for anyone else (a
// global administrator naming none: across agencies, as before).
func creationComposeAgency(r *http.Request, named string) string {
	if a, ok := agencyctx.From(r); ok {
		return a
	}
	if named = strings.TrimSpace(named); named != "" {
		return named
	}
	if id, ok := auth.IdentityFrom(r.Context()); ok && !id.GlobalAdmin(auth.PermCompose) {
		if mine := id.AgenciesFor(auth.PermCompose); len(mine) == 1 {
			return mine[0]
		}
	}
	return ""
}

// stepJobRow resolves ONE step's job for a workflow at home in
// composeAgencyOf(ctx): its scope and its agency (agencyid.JobSQL; a job whose
// owner cannot be read is Global's). A name-only step is that agency's job of
// the name when it has one, as the engine runs it; else, as before, any
// agency's: on a save the incoming graph's jobs say whose the workflow
// becomes (workflowSaveOwner; a global administrator's repair names another
// agency's jobs), and a step on a job of another agency is refused by the
// scope it is on, not answered "unknown".
func (s *Server) stepJobRow(ctx context.Context, ref workflow.StepRef) (scope sql.NullString, agency string, ok bool) {
	read := func(where string, args ...any) bool {
		var a sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT j.scope, `+agencyid.JobSQL("j")+` FROM jobs j WHERE `+where+` AND j.deleted_at IS NULL`, args...).Scan(&scope, &a)
		if err != nil {
			return false
		}
		agency = agencyid.Global
		if a.Valid {
			agency = a.String
		}
		return true
	}
	// R2F-2 — a pinned step resolves by identity alone, as the engine does.
	if ref.UID != "" {
		ok = read(`j.uid = ?`, ref.UID)
		return scope, agency, ok
	}
	sources := workflow.StepSourceOrder(ref.Source, "cronomicon")
	if home := composeAgencyOf(ctx); home != "" {
		for _, src := range sources {
			if src == "git" {
				ok = read(`j.name = ? AND j.source = 'git' AND COALESCE(j.repo_id, '`+repoid.Global+`') IN (SELECT g.id FROM git_repos g WHERE g.agency_id = ?)`, ref.Name, home)
			} else {
				ok = read(`j.name = ? AND j.source = 'cronomicon' AND `+agencyid.JobOwnerSQL("j")+` = ?`, ref.Name, home)
			}
			if ok {
				return scope, agency, true
			}
		}
	}
	for _, src := range sources {
		if ok = read(`j.name = ? AND j.source = ?`, ref.Name, src); ok {
			return scope, agency, true
		}
	}
	return sql.NullString{}, "", false
}

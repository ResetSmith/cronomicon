package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// The agency an audit row is of is its OBJECT's, never the agency the request
// was viewing (2.5.0 C1; the owner's answer to question 74). These read it for
// the objects the handlers here change; each is Global's when the object is
// not there (any longer).

// agencyReadFilter is who reads a row of an agency (2.5.0 C1 and C2, CF-1), as a clause over
// agencyExpr, the row's agency by the no-owner rule: an agency's rows are its
// own people's and theirs whose grant reaches every agency; Global's rows, and
// the installation's (which are Global's), are only the latter's: global
// administrators and a viewer of every agency, the operations team (the
// owner's answer to question 76). The one place the rule is written.
func agencyReadFilter(r *http.Request, agencyExpr string) (string, []any) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		return " AND 1 = 0", nil
	}
	everywhere, agencies := id.AgenciesRead()
	if everywhere {
		return "", nil
	}
	if len(agencies) == 0 {
		return " AND 1 = 0", nil
	}
	args := make([]any, len(agencies))
	for i, a := range agencies {
		args[i] = a
	}
	return " AND " + agencyExpr + " IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(agencies)), ", ") + ")", args
}

// readsAgency is agencyReadFilter for a row already read, by its owner's id
// (none is Global's): whether the caller may read a row of that agency.
func readsAgency(r *http.Request, owner string) bool {
	id, ok := auth.IdentityFrom(r.Context())
	return ok && id.ReadsAgency(agencyOrGlobal(owner))
}

// readsAnyAgency is readsAgency for a row that names several agencies (a
// membership): readable when the caller reads one of them; a row of none is
// Global's.
func readsAnyAgency(r *http.Request, agencies []string) bool {
	if len(agencies) == 0 {
		return readsAgency(r, agencyid.Global)
	}
	for _, a := range agencies {
		if readsAgency(r, a) {
			return true
		}
	}
	return false
}

// keepReadable keeps the rows of a list already read that the caller may read
// (readsAgency, by owner), then narrows them to the agency the request works
// in (agencyctx.Keep): the reading rule, then the view, for a list filtered in
// Go.
func keepReadable[T any](r *http.Request, list []T, owner func(T) string) []T {
	out := make([]T, 0, len(list))
	for _, x := range list {
		if readsAgency(r, owner(x)) {
			out = append(out, x)
		}
	}
	return agencyctx.Keep(r, out, owner)
}

// auditExportScope is what the caller's audit export holds (CF-5): everything
// for a global administrator; the rows of the agencies an agency's
// administrator administers, and no authentication events.
func auditExportScope(r *http.Request) settings.ExportScope {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		return settings.ExportScope{}
	}
	if id.GlobalAdmin(auth.PermConfigureApp) {
		return settings.ExportScope{All: true, AuthEvents: true}
	}
	return settings.ExportScope{Agencies: id.AgenciesFor(auth.PermConfigureApp)}
}

// jobAgency is the agency of the job with uid (agencyid.JobOwnerSQL).
func (s *Server) jobAgency(ctx context.Context, uid string) string {
	return s.ownerBy(ctx, `SELECT `+agencyid.JobOwnerSQL("j")+` FROM jobs j WHERE j.uid = ?`, uid)
}

// workflowAgency is the agency of the workflow with uid.
func (s *Server) workflowAgency(ctx context.Context, uid string) string {
	return s.ownerBy(ctx, `SELECT `+agencyid.WorkflowOwnerSQL+` FROM workflows w WHERE w.uid = ?`, uid)
}

// runAgency is the owner of the run traceID (1480).
func (s *Server) runAgency(ctx context.Context, traceID string) string {
	return settings.RowAgency(ctx, s.db, "runs", "owner_agency", traceID)
}

// workflowRunAgency is the agency of the workflow run traceID (1520).
func (s *Server) workflowRunAgency(ctx context.Context, traceID string) string {
	return settings.RowAgency(ctx, s.db, "workflow_runs", "agency_id", traceID)
}

// definitionOwner is the table, alias and owner expression of a definition
// kind ('job', 'workflow', 'schedule').
func definitionOwner(kind string) (table, alias, expr string) {
	switch kind {
	case "job":
		return "jobs", "j", agencyid.JobOwnerSQL("j")
	case "workflow":
		return "workflows", "w", agencyid.WorkflowOwnerSQL
	default:
		return "schedules", "sc", agencyid.ColumnOwnerSQL("sc", "owner_agency")
	}
}

// definitionAgency is the agency of the definition of kind with uid.
func (s *Server) definitionAgency(ctx context.Context, kind, uid string) string {
	table, alias, expr := definitionOwner(kind)
	return s.ownerBy(ctx, `SELECT `+expr+` FROM `+table+` `+alias+` WHERE `+alias+`.uid = ?`, uid)
}

// memberAgency is the agency, read now, of a member of an agency by its kind
// as the membership routes name it (scope, secret, env-var, ssh-credential,
// runner): after a repair, the agency the object ended up in (D1).
func (s *Server) memberAgency(ctx context.Context, kind, id string) string {
	table := map[string]string{
		"scope": "scopes", "secret": "secrets", "env-var": "env_vars",
		"ssh-credential": "ssh_credentials", "runner": "runners",
	}[kind]
	if table == "" {
		return agencyid.Global
	}
	return settings.RowAgency(ctx, s.db, table, "owner_agency", id)
}

// agencyOrGlobal is the no-owner rule for an agency already read.
func agencyOrGlobal(a string) string {
	if a == "" {
		return agencyid.Global
	}
	return a
}

func (s *Server) ownerBy(ctx context.Context, query string, arg any) string {
	var a string
	if err := s.db.QueryRowContext(ctx, query, arg).Scan(&a); err != nil || a == "" {
		return agencyid.Global
	}
	return a
}

package api

import (
	"context"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// The agency an audit row is of is its OBJECT's, never the agency the request
// was viewing (2.5.0 C1; the owner's answer to question 74). These read it for
// the objects the handlers here change; each is Global's when the object is
// not there (any longer).

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

// binnedAgency is the agency of what the recycle bin holds of kind under name,
// read before a purge: the one owner they have, else Global's (a purge by name
// takes every binned row of it).
func (s *Server) binnedAgency(ctx context.Context, kind, name string) string {
	table, alias, expr := definitionOwner(kind)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+expr+` FROM `+table+` `+alias+
		` WHERE `+alias+`.source = 'cronomicon' AND `+alias+`.name = ? AND `+alias+`.deleted_at IS NOT NULL`, name)
	if err != nil {
		return agencyid.Global
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var o string
		if rows.Scan(&o) == nil {
			owners = append(owners, o)
		}
	}
	if len(owners) != 1 {
		return agencyid.Global
	}
	return owners[0]
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

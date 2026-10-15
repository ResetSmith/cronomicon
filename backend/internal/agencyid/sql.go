package agencyid

// Whose a definition is, as SQL (2.4.0, §0a). Here, in a package everything
// may import, so that the engine, the reactor, dispatch and the notices ask it
// in the same words.
//
// Global's repository's id is the literal 'global' (repoid.Global); it is
// spelled out so that this package stays a leaf.

// WorkflowSQL is the agency of the row `w` of `workflows`: a Git workflow's
// repository's (Global's for a row with none), and the owner of one built in
// the app. NULL for a Git workflow whose repository is gone.
const WorkflowSQL = `(CASE WHEN w.source = 'git'
	THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(w.repo_id, 'global'))
	ELSE w.owner_agency END)`

// JobSQL is the agency of the row `alias` of `jobs`: a Git job's repository's,
// and the owner of one built in the app (migration 1400; until then it was
// worked out from its scope's name, which cannot say which scope once names
// are per agency, I2). A Git job that records no repository (none in an
// installation since 1310, which stamped Global's; a test's) is its owner's
// too, as it is confined by nothing (runref.RepoScopeMismatch). NULL for a Git
// job whose repository is gone.
func JobSQL(alias string) string {
	return `(CASE WHEN ` + alias + `.source = 'git' AND ` + alias + `.repo_id IS NOT NULL
	THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = ` + alias + `.repo_id)
	ELSE ` + alias + `.owner_agency END)`
}

// ScopeIn is "the row `alias` of `scopes` is the scope named nameExpr in the
// agency agencyExpr (an id)". A scope's name is unique within its agency (I2,
// IN-5), not across the installation: every lookup of a scope by name says
// whose. A scope's agency is its owner_agency (1410): its one agency, or
// Global's for a scope several share.
func ScopeIn(alias, nameExpr, agencyExpr string) string {
	return `(` + alias + `.name = ` + nameExpr + ` AND ` + alias + `.owner_agency = ` + agencyExpr + `)`
}

// RunScopeOwnerSQL is the agency a run's scope is looked for in, from the run's
// agency snapshot (agenciesJSONExpr, a JSON list of agency names): its one
// agency, Global's for a run of several (a run on a scope several share, which
// is Global's, 1410) or none. execspec.RunScopeOwner is the same rule in Go.
func RunScopeOwnerSQL(agenciesJSONExpr string) string {
	return `COALESCE((SELECT CASE WHEN json_array_length(COALESCE(` + agenciesJSONExpr + `, '[]')) = 1
	                    THEN (SELECT ag.id FROM agencies ag WHERE ag.name = json_extract(` + agenciesJSONExpr + `, '$[0]')) END),
	         '` + Global + `')`
}

// DefinitionSQL is the agency of a definition by its uid (?1) and kind (?2):
// a job's (JobSQL), a workflow's (WorkflowSQL). NULL for no such row.
var DefinitionSQL = DefinitionOf("?1", "?2")

// DefinitionOf is DefinitionSQL over two SQL expressions, the definition's uid
// and its kind ('job' or 'workflow'), for a statement that has them as columns.
func DefinitionOf(uidExpr, kindExpr string) string {
	return `(CASE ` + kindExpr + `
	WHEN 'job' THEN (SELECT ` + JobSQL("dj") + ` FROM jobs dj WHERE dj.uid = ` + uidExpr + `)
	ELSE (SELECT ` + WorkflowSQL + ` FROM workflows w WHERE w.uid = ` + uidExpr + `)
	END)`
}

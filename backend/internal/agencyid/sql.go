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

// JobSQL is the agency of the row `alias` of `jobs`: a Git job's repository's
// (Global's for a row with none), and the owner of one built in the app
// (migration 1400; until then it was worked out from its scope's name, which
// cannot say which scope once names are per agency, I2). NULL for a Git job
// whose repository is gone.
func JobSQL(alias string) string {
	return `(CASE WHEN ` + alias + `.source = 'git'
	THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(` + alias + `.repo_id, 'global'))
	ELSE ` + alias + `.owner_agency END)`
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

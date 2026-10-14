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

// DefinitionSQL is the agency of a definition by its uid (?1) and kind (?2):
// a Git job's or workflow's repository's (Global's for a row with none); a job
// built in the app, its scope's when one agency alone owns it, and Global's
// otherwise (no scope, or one several share), as its runs are; a workflow
// built in the app, its owner. NULL for no such row.
const DefinitionSQL = `(CASE ?2
	WHEN 'job' THEN (SELECT CASE WHEN j.source = 'git'
	                   THEN (SELECT g.agency_id FROM git_repos g WHERE g.id = COALESCE(j.repo_id, 'global'))
	                   ELSE COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
	                                    FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
	                                   WHERE sc.name = j.scope), '` + Global + `') END
	                   FROM jobs j WHERE j.uid = ?1)
	ELSE (SELECT ` + WorkflowSQL + ` FROM workflows w WHERE w.uid = ?1)
	END)`

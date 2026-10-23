package api_test

// stampRunAgencies gives every seeded run that has none the snapshot a
// producer would have written: its scope's agency (I2: a run's scope is looked
// for in its agency, and a run with no snapshot is Global's). A seeded run of
// no job also takes that agency as its owner (1480's trigger made it Global's,
// as no job named one), as a run of a job in that scope would be owned; since
// 2.5.0 C2 a run is read by whose it is. Its workflow run likewise.
func stampRunAgencies(exec func(string, ...any)) {
	exec(`UPDATE runs SET agencies_json = COALESCE((SELECT json_array(a.name) FROM scopes sc JOIN agencies a ON a.id = sc.owner_agency WHERE sc.name = runs.scope), '["Global"]')
	       WHERE scope IS NOT NULL AND (agencies_json IS NULL OR agencies_json IN ('', '[]', '["Global"]'))`)
	exec(`UPDATE runs SET owner_agency = (SELECT sc.owner_agency FROM scopes sc WHERE sc.name = runs.scope)
	       WHERE scope IS NOT NULL AND job_uid IS NULL AND owner_agency = 'global'
	         AND (SELECT COUNT(*) FROM scopes sc WHERE sc.name = runs.scope) = 1`)
	exec(`UPDATE workflow_runs SET agency_id = (SELECT ru.owner_agency FROM runs ru WHERE ru.workflow_run_id = workflow_runs.id LIMIT 1)
	       WHERE agency_id = 'global' AND workflow_uid IS NULL
	         AND EXISTS (SELECT 1 FROM runs ru WHERE ru.workflow_run_id = workflow_runs.id AND ru.owner_agency <> 'global')`)
}

package api_test

// stampRunAgencies gives every seeded run that has none the snapshot a
// producer would have written: its scope's agency (I2: a run's scope is looked
// for in its agency, and a run with no snapshot is Global's).
func stampRunAgencies(exec func(string, ...any)) {
	exec(`UPDATE runs SET agencies_json = COALESCE((SELECT json_array(a.name) FROM scopes sc JOIN agencies a ON a.id = sc.owner_agency WHERE sc.name = runs.scope), '["Global"]')
	       WHERE scope IS NOT NULL AND (agencies_json IS NULL OR agencies_json IN ('', '[]', '["Global"]'))`)
}

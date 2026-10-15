package gitlab

import (
	"context"
	"fmt"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// Confinement at sync (2.4.0, GR-14).
//
// A repository is authority over its own agency and no other. A job in an
// agency's repository must name a scope that agency owns; a job with no scope
// is refused there, because a job with no scope is Global's. Global's
// repository is confined the same way (§0a, IN-3: until then its jobs might
// name any agency's scope): a scope its job names must be Global's, and a job
// with no scope is Global's already, so it is kept.
//
// This is the check that keeps a file out. It is not the control: a scope can
// be moved to another agency after the sync that accepted a job, and what
// refuses that job's runs is the check made when a run is produced (GR-15).

// The two things a refused file is told. Neither says whether the scope it
// named exists: "another agency has a scope of that name" is theirs to know.
const (
	refusedNoScope    = "a job in an agency's repository must name one of that agency's scopes (spec.scope): with none it would be Global's; this job was not synced"
	refusedOtherScope = "the scope %q is not one of this repository's agency's scopes: a job in an agency's repository must name a scope its agency owns; this job was not synced"
)

// confineJobs splits the jobs of the sync that is under way into the ones that
// may be written and the ones that are refused, each refusal as its file's
// error.
//
// A scope counts as the agency's when it is a scope that agency alone owns
// now, or when it is one of THIS sync's inventories whose name is free in the
// agency (a scope's name is unique within an agency only, I2): jobs
// are written before scopes, and a repository's first sync brings a job and
// the scope it names in one commit.
func (s *Service) confineJobs(ctx context.Context, jobs []JobYAML, parsed []inventoryScope) (kept []JobYAML, refused []ValidationError, err error) {
	global := s.repo() == repoid.Global
	arriving := map[string]bool{}
	for _, sc := range parsed {
		arriving[sc.Name] = true
	}
	owned := map[string]bool{} // scope name → is this agency's
	isOwn := func(scope string) (bool, error) {
		if v, done := owned[scope]; done {
			return v, nil
		}
		var exists, agencies, mine int
		if err := s.db.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM scopes WHERE name = ?1 AND owner_agency = ?2),
			       (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name = ?1 AND sc.owner_agency = ?2),
			       (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
			         WHERE sc.name = ?1 AND sc.owner_agency = ?2 AND sa.agency_id = ?2)`, scope, s.agency()).Scan(&exists, &agencies, &mine); err != nil {
			return false, err
		}
		v := (exists == 1 && agencies == 1 && mine == 1) || (exists == 0 && arriving[scope])
		owned[scope] = v
		return v, nil
	}
	for _, j := range jobs {
		file := j.SourcePath
		if file == "" {
			file = "jobs/" + j.Metadata.Name + ".yaml"
		}
		scope := strings.TrimSpace(j.Spec.Scope)
		if scope == "" && global {
			kept = append(kept, j)
			continue
		}
		if scope == "" {
			refused = append(refused, ValidationError{File: file, Field: "spec.scope", Message: refusedNoScope})
			continue
		}
		ok, qerr := isOwn(scope)
		if qerr != nil {
			return nil, nil, fmt.Errorf("read the agency of the scope a job names: %w", qerr)
		}
		if !ok {
			refused = append(refused, ValidationError{File: file, Field: "spec.scope", Message: fmt.Sprintf(refusedOtherScope, scope)})
			continue
		}
		kept = append(kept, j)
	}
	return kept, refused, nil
}

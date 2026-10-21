package api

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
)

// A stored secret's or an SSH key's Usage (2.5.1 SG2.4, SG-14): what refers to
// it, which runs used it, and what was done to it and by whom. The agency that
// owns the row reads it (the reading rule; 404 for anyone else), so an agency
// can review its own secrets; Global's, the everywhere-readers.
//
// A reference is listed only when the resolver would pick THIS row for it:
// two agencies (or two scopes) may hold a secret of one name, and a binding of
// that name in another agency's job is that agency's row's reference, not this
// one's. The judgement is runref.LookupEntityID, the dispatch resolver's own.

// usageReference is one thing that would inject or connect with the row.
type usageReference struct {
	// Via: binding (a job's or a script's declared binding), become (a job's
	// become password), connectAs (a job's connect-as key), host or bastion
	// (a record that authenticates with the key).
	Via       string `json:"via"`
	OwnerKind string `json:"ownerKind"` // job | script | host | bastion
	OwnerID   string `json:"ownerId"`   // a job's or script's uid, a host's or bastion's id
	Name      string `json:"name"`
	Scope     string `json:"scope,omitempty"`
	As        string `json:"as,omitempty"`
}

// usageUse is one run's use, from secret_uses.
type usageUse struct {
	At         string `json:"at"`
	TraceID    string `json:"traceId"`
	Via        string `json:"via"`
	JobUID     string `json:"jobUid,omitempty"`
	JobName    string `json:"jobName,omitempty"`
	RunnerName string `json:"runnerName,omitempty"`
}

// usageCounts are the uses in the last 7, 30 and 90 days.
type usageCounts struct {
	Days7  int `json:"days7"`
	Days30 int `json:"days30"`
	Days90 int `json:"days90"`
}

// usageChange is one change_log row about the row.
type usageChange struct {
	At            string `json:"at"`
	Actor         string `json:"actor"`
	Action        string `json:"action"`
	Details       string `json:"details,omitempty"`
	ActorRelation string `json:"actorRelation,omitempty"`
	ActingAgency  string `json:"actingAgency,omitempty"`
}

// rowUsage is the Usage view's answer.
type rowUsage struct {
	Kind       string           `json:"kind"` // secret | key
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Owner      string           `json:"owner"`
	References []usageReference `json:"references"`
	Uses       usageCounts      `json:"uses"`
	Recent     []usageUse       `json:"recent"`
	Changes    []usageChange    `json:"changes"`
	// For a key: the host records and bastions that name it by id, as the
	// route answered before 2.5.1 (the delete's refusal reads them too).
	Hosts    []sshkeys.Ref `json:"hosts,omitempty"`
	Bastions []sshkeys.Ref `json:"bastions,omitempty"`
}

const (
	usageRecentLimit  = 25
	usageChangesLimit = 100
)

// computeRowUsage answers for row id of kind (runref.KindSecret or KindKey),
// whose key or label is name and whose owner is owner (an agency id).
func (s *Server) computeRowUsage(ctx context.Context, kind runref.Kind, id, name, owner string) (*rowUsage, error) {
	if owner == "" {
		owner = agencyid.Global
	}
	u := &rowUsage{Kind: string(kind), ID: id, Name: name, Owner: owner,
		References: []usageReference{}, Recent: []usageUse{}, Changes: []usageChange{}}
	refs, err := s.rowReferences(ctx, kind, id, name, owner)
	if err != nil {
		return nil, err
	}
	u.References = refs
	if err := s.rowUses(ctx, kind, id, u); err != nil {
		return nil, err
	}
	category := "Secrets"
	if kind == runref.KindKey {
		category = "SSH Keys"
	}
	if u.Changes, err = s.rowChanges(ctx, category, id, name, owner); err != nil {
		return nil, err
	}
	return u, nil
}

// judgedJob is a job that names the row's name somehow, with what the
// resolver needs to judge which row it would get.
type judgedJob struct {
	uid, name, scope, agencyName, as string
}

// resolvesHere reports whether a run of the job would be given row id.
func (s *Server) resolvesHere(ctx context.Context, kind runref.Kind, refName, id string, j judgedJob) (bool, error) {
	agency := j.agencyName
	if agency == "" {
		agency = agencyid.GlobalName
	}
	got, found, err := runref.LookupEntityID(ctx, s.db, kind, refName, j.scope, []string{agency})
	if errors.Is(err, runref.ErrAmbiguousReference) {
		return false, nil // a run would be refused; it is not this row's use
	}
	if err != nil {
		return false, err
	}
	return found && got == id, nil
}

// jobsNaming lists live jobs whose `where` (over jobs j) matches arg, with the
// job's scope and its agency's name; extra selects a binding's alias.
func (s *Server) jobsNaming(ctx context.Context, from, where string, args ...any) ([]judgedJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.uid, j.name, COALESCE(j.scope, ''), COALESCE((SELECT a.name FROM agencies a WHERE a.id = `+agencyid.JobSQL("j")+`), ''), `+from+`
		  WHERE j.deleted_at IS NULL AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []judgedJob
	for rows.Next() {
		var j judgedJob
		if err := rows.Scan(&j.uid, &j.name, &j.scope, &j.agencyName, &j.as); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Server) rowReferences(ctx context.Context, kind runref.Kind, id, name, owner string) ([]usageReference, error) {
	out := []usageReference{}
	add := func(via string, jobs []judgedJob, refName string) error {
		for _, j := range jobs {
			ok, err := s.resolvesHere(ctx, kind, refName, id, j)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, usageReference{Via: via, OwnerKind: "job", OwnerID: j.uid, Name: j.name, Scope: j.scope, As: j.as})
			}
		}
		return nil
	}
	// Bindings a job declares.
	bound, err := s.jobsNaming(ctx, `rb.alias FROM reference_bindings rb JOIN jobs j ON rb.owner_kind = 'job' AND j.uid = rb.owner_uid`,
		`rb.ref_kind = ? AND rb.ref_name = ?`, string(kind), name)
	if err != nil {
		return nil, err
	}
	if err := add("binding", bound, name); err != nil {
		return nil, err
	}
	if kind == runref.KindSecret {
		become, err := s.jobsNaming(ctx, `'' FROM jobs j`, `j.become_password_secret = ?`, name)
		if err != nil {
			return nil, err
		}
		if err := add("become", become, name); err != nil {
			return nil, err
		}
	} else {
		connect, err := s.jobsNaming(ctx, `'' FROM jobs j`, `j.ssh_credential = ?`, name)
		if err != nil {
			return nil, err
		}
		if err := add("connectAs", connect, name); err != nil {
			return nil, err
		}
	}
	// A script's binding resolves in each job that runs it; a script of the
	// row's own agency that binds the name is listed as one.
	rows, err := s.db.QueryContext(ctx, `
		SELECT sx.uid, sx.name, rb.alias
		  FROM reference_bindings rb JOIN scripts sx ON rb.owner_kind = 'script' AND sx.uid = rb.owner_uid
		 WHERE rb.ref_kind = ? AND rb.ref_name = ? AND `+agencyid.ScriptOwnerSQL("sx")+` = ?`, string(kind), name, owner)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r usageReference
		if err := rows.Scan(&r.OwnerID, &r.Name, &r.As); err != nil {
			rows.Close()
			return nil, err
		}
		r.Via, r.OwnerKind = "binding", "script"
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if kind == runref.KindKey {
		for _, t := range []struct{ table, nameCol, via string }{{"ssh_hosts", "hostname", "host"}, {"bastions", "hostname", "bastion"}} {
			hrows, err := s.db.QueryContext(ctx, `SELECT id, `+t.nameCol+` FROM `+t.table+` WHERE auth_credential_id = ? ORDER BY `+t.nameCol, id) //nolint:gosec // fixed identifiers
			if err != nil {
				return nil, err
			}
			for hrows.Next() {
				var r usageReference
				if err := hrows.Scan(&r.OwnerID, &r.Name); err != nil {
					hrows.Close()
					return nil, err
				}
				r.Via, r.OwnerKind = t.via, t.via
				out = append(out, r)
			}
			hrows.Close()
			if err := hrows.Err(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (s *Server) rowUses(ctx context.Context, kind runref.Kind, id string, u *rowUsage) error {
	now := time.Now().UTC()
	day := func(n int) string { return now.AddDate(0, 0, -n).Format(time.RFC3339) }
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(at >= ?), 0), COALESCE(SUM(at >= ?), 0), COALESCE(SUM(at >= ?), 0)
		  FROM secret_uses WHERE kind = ? AND row_id = ?`, day(7), day(30), day(90), string(kind), id).
		Scan(&u.Uses.Days7, &u.Uses.Days30, &u.Uses.Days90); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, trace_id, via, COALESCE(job_uid, ''), COALESCE(job_name, ''), COALESCE(runner_name, '')
		  FROM secret_uses WHERE kind = ? AND row_id = ? ORDER BY at DESC, id DESC LIMIT ?`, string(kind), id, usageRecentLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r usageUse
		if err := rows.Scan(&r.At, &r.TraceID, &r.Via, &r.JobUID, &r.JobName, &r.RunnerName); err != nil {
			return err
		}
		u.Recent = append(u.Recent, r)
	}
	return rows.Err()
}

// rowChanges reads the row's change-log rows filed under its owner: those
// that name its id ("id=<id>", since 2.5.1), and, before 2.5.1, those that
// named it by key or label with no details. A dispatch's "injected" row is a
// run's, read through the uses instead.
func (s *Server) rowChanges(ctx context.Context, category, id, name, owner string) ([]usageChange, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, actor, action, COALESCE(details, ''), COALESCE(actor_relation, ''), COALESCE(acting_agency, '')
		  FROM change_log
		 WHERE category = ? AND action <> 'injected' AND COALESCE(agency_id, 'global') = ?
		   AND (details = ? OR details LIKE ? OR (target = ? AND COALESCE(details, '') = ''))
		 ORDER BY at DESC, id DESC LIMIT ?`,
		category, owner, "id="+id, "id="+id+" %", name, usageChangesLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageChange{}
	for rows.Next() {
		var c usageChange
		if err := rows.Scan(&c.At, &c.Actor, &c.Action, &c.Details, &c.ActorRelation, &c.ActingAgency); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// rowOwnerOrGlobal reads a secret's or key's owner agency id.
func (s *Server) rowOwnerOrGlobal(ctx context.Context, table, id string) (string, error) {
	var owner sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT owner_agency FROM `+table+` WHERE id = ?`, id).Scan(&owner) //nolint:gosec // fixed identifiers
	if err != nil {
		return "", err
	}
	if owner.String == "" {
		return agencyid.Global, nil
	}
	return owner.String, nil
}

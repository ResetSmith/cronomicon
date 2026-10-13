package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// A repository per agency (2.4.0; GR-1, GR-9). Global's repository is the row
// 'global', which every installation has in effect whether or not the row is
// there. An agency has at most one other (git_repos.agency_id is unique), and
// it is connected here: by the agency's own administrator, with the agency's
// own token, to a host a global administrator has listed (GR-10).

// Repo is one repository: whose it is, its connection as the connection's
// form reads and writes it, what its last sync was, and how much it supplies.
type Repo struct {
	ID         string `json:"id"`
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
	*GitlabConfig
	LastSHA      string `json:"lastSha"`
	LastSyncedAt string `json:"lastSyncedAt"`
	LastStatus   string `json:"lastStatus"`
	// Supplies is what the repository's sync has written and a disconnect
	// would remove (GR-28: "the confirm lists the counts").
	Supplies RepoCounts `json:"supplies"`
}

// RepoCounts is the rows one repository supplies.
type RepoCounts struct {
	Jobs      int `json:"jobs"`
	Workflows int `json:"workflows"`
	Scripts   int `json:"scripts"`
	Schedules int `json:"schedules"`
	Scopes    int `json:"scopes"`
}

// ErrNoSuchAgency is PutAgencyRepo's answer for an agency that is not there.
var ErrNoSuchAgency = errors.New("no such agency")

// RepoOfAgency is the id of the repository an agency has connected. Global's
// is 'global' whether or not its row is there; any other agency with none is
// ErrRepoNotFound.
func RepoOfAgency(ctx context.Context, database *sql.DB, agencyID string) (string, error) {
	if agencyID == agencyid.Global {
		return repoid.Global, nil
	}
	var id string
	err := database.QueryRowContext(ctx, `SELECT id FROM git_repos WHERE agency_id = ?`, agencyID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRepoNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read the agency's repository: %w", err)
	}
	return id, nil
}

// GetRepo reads one repository. ErrRepoNotFound for one that is not there
// (never for Global's).
func GetRepo(ctx context.Context, database *sql.DB, appCfg *config.Config, repoID string) (*Repo, error) {
	conn, err := GetGitlabConfig(ctx, database, appCfg, repoID)
	if err != nil {
		return nil, err
	}
	r := &Repo{ID: repoID, AgencyID: agencyid.Global, GitlabConfig: conn}
	var agency, sha, at, status sql.NullString
	err = database.QueryRowContext(ctx,
		`SELECT agency_id, last_sha, last_synced_at, last_status FROM git_repos WHERE id = ?`, repoID).Scan(&agency, &sha, &at, &status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read git_repos: %w", err)
	}
	if agency.String != "" {
		r.AgencyID = agency.String
	}
	r.LastSHA, r.LastSyncedAt, r.LastStatus = sha.String, at.String, status.String
	if err := database.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, r.AgencyID).Scan(&r.AgencyName); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read the repository's agency: %w", err)
	}
	if r.Supplies, err = CountRepoRows(ctx, database, repoID); err != nil {
		return nil, err
	}
	return r, nil
}

// ListRepos is every repository, Global's first (present whether or not its
// row is), the others by their agency's name.
func ListRepos(ctx context.Context, database *sql.DB, appCfg *config.Config) ([]*Repo, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT g.id FROM git_repos g LEFT JOIN agencies a ON a.id = g.agency_id
		 WHERE g.id <> ? ORDER BY COALESCE(a.name, ''), g.id`, repoid.Global)
	if err != nil {
		return nil, fmt.Errorf("list git_repos: %w", err)
	}
	ids := []string{repoid.Global}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]*Repo, 0, len(ids))
	for _, id := range ids {
		r, err := GetRepo(ctx, database, appCfg, id)
		if errors.Is(err, ErrRepoNotFound) {
			continue // disconnected since it was listed
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// CountRepoRows counts what a repository supplies. A row of Global's
// repository from before a repository was a row records none, and is
// Global's.
func CountRepoRows(ctx context.Context, database interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, repoID string) (RepoCounts, error) {
	var c RepoCounts
	for _, q := range []struct {
		into  *int
		query string
	}{
		{&c.Jobs, `SELECT COUNT(*) FROM jobs WHERE source = 'git' AND COALESCE(repo_id, 'global') = ?`},
		{&c.Workflows, `SELECT COUNT(*) FROM workflows WHERE source = 'git' AND COALESCE(repo_id, 'global') = ?`},
		{&c.Scripts, `SELECT COUNT(*) FROM scripts WHERE COALESCE(repo_id, 'global') = ?`},
		{&c.Schedules, `SELECT COUNT(*) FROM schedules WHERE source = 'git' AND COALESCE(repo_id, 'global') = ?`},
		{&c.Scopes, `SELECT COUNT(*) FROM scopes WHERE source = 'git' AND COALESCE(repo_id, 'global') = ?`},
	} {
		if err := database.QueryRowContext(ctx, q.query, repoID).Scan(q.into); err != nil {
			return c, fmt.Errorf("count the repository's rows: %w", err)
		}
	}
	return c, nil
}

// PutAgencyRepo writes the connection of an agency's repository, connecting
// one when the agency has none. created reports which it was.
//
// Global's is the row it always was, written as PUT /settings/gitlab writes
// it, and its URL is not held to the allowed hosts (GR-10). Any other
// agency's must be a URL, and CheckRepoURL decides it before anything is
// written: the scheme, no user in it, and a host on the list.
//
// The caller starts or restarts the repository's sync service afterwards
// (gitlab.Registry.Restart), which is what first syncs a new connection.
func PutAgencyRepo(ctx context.Context, database *sql.DB, appCfg *config.Config, agencyID string, inp GitlabConfig, actor string) (repo *Repo, created bool, err error) {
	if agencyID == agencyid.Global {
		if _, err := UpdateGitlabConfig(ctx, database, appCfg, repoid.Global, inp, actor); err != nil {
			return nil, false, err
		}
		repo, err = GetRepo(ctx, database, appCfg, repoid.Global)
		return repo, false, err
	}
	var one int
	if err := database.QueryRowContext(ctx, `SELECT 1 FROM agencies WHERE id = ?`, agencyID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrNoSuchAgency
		}
		return nil, false, fmt.Errorf("read the agency: %w", err)
	}
	inp.RepoUrl = strings.TrimSpace(inp.RepoUrl)
	if inp.RepoUrl == "" {
		return nil, false, fmt.Errorf("%w: repoUrl is required: an agency's repository is connected by its URL", ErrValidation)
	}
	// Defaults for what a first write may leave out, as the columns default.
	if strings.TrimSpace(inp.WriteBranch) == "" {
		inp.WriteBranch = "main"
	}
	if strings.TrimSpace(inp.BotName) == "" {
		inp.BotName = "cronomicon-bot"
	}
	if strings.TrimSpace(inp.BotEmail) == "" {
		inp.BotEmail = "cronomicon-bot@cronomicon.io"
	}
	if inp.TokenExpiryNotifyDays <= 0 {
		inp.TokenExpiryNotifyDays = 7
	}

	repoID, err := RepoOfAgency(ctx, database, agencyID)
	switch {
	case errors.Is(err, ErrRepoNotFound):
		repoID, created = "repo-"+db.NewID(), true
	case err != nil:
		return nil, false, err
	}
	if err := CheckRepoURL(ctx, database, repoID, inp.RepoUrl); err != nil {
		return nil, false, err
	}
	if created {
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := database.ExecContext(ctx,
			`INSERT INTO git_repos (id, agency_id, created_by, created_at, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?, ?, ?)`,
			repoID, agencyID, actor, now, actor, now); err != nil {
			// Two administrators connecting at once: the agency has one
			// repository (UNIQUE), and the second write is of that one.
			if existing, rerr := RepoOfAgency(ctx, database, agencyID); rerr == nil {
				repoID, created = existing, false
			} else {
				return nil, false, fmt.Errorf("connect the agency's repository: %w", err)
			}
		}
	}
	if _, err := UpdateGitlabConfig(ctx, database, appCfg, repoID, inp, actor); err != nil {
		if created {
			_, _ = database.ExecContext(ctx, `DELETE FROM git_repos WHERE id = ?`, repoID)
		}
		return nil, false, err
	}
	repo, err = GetRepo(ctx, database, appCfg, repoID)
	return repo, created, err
}

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
	// Leftover marks the built-in agency's connection from before 2.5.3
	// (CA-6): it does not sync, and is only removed. Set by the route that
	// shows it.
	Leftover   bool   `json:"leftover,omitempty"`
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

// ErrCentralHasNoRepository is PutAgencyRepo's answer for the built-in agency
// (2.5.3, CA-6): it runs nothing, so it keeps no definitions in Git. Templates
// live in an ordinary agency's repository (CA-19).
var ErrCentralHasNoRepository = errors.New("the built-in administration agency has no repository: " +
	"it runs nothing, so it keeps no definitions in Git; connect the repository to an ordinary agency instead")

// RepoOfAgency is the id of the repository an agency has connected;
// ErrRepoNotFound for an agency with none. The built-in agency has none since
// 2.5.3 (CA-6), whatever row 1310 left for it: a connection made before is a
// leftover (CentralLeftoverRepo) that does not sync and is only removed.
func RepoOfAgency(ctx context.Context, database *sql.DB, agencyID string) (string, error) {
	if agencyID == agencyid.Global {
		return "", ErrRepoNotFound
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

// CentralLeftoverRepo reports whether the built-in agency still holds a
// repository from before 2.5.3 (CA-6): its row with a URL, or with a commit it
// synced, or rows of definitions still recorded as its repository's. The
// second and third are the installation whose Global repository came from
// CRONOMICON_GITLAB_BASE_URL, which never wrote the URL into the row (found by
// the review of CA2), and the dev seed's. A leftover does not sync; a global
// administrator removes it ("Remove the leftover repository", question 203).
// The unconfigured row 1310 gave every installation, with nothing synced, is
// no leftover.
func CentralLeftoverRepo(ctx context.Context, database interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (bool, error) {
	var left bool
	err := database.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM git_repos WHERE id = ?1
		                  AND (COALESCE(url, '') <> '' OR COALESCE(last_sha, '') <> ''))
		    OR EXISTS (SELECT 1 FROM git_repos WHERE id = ?1)
		       AND (EXISTS (SELECT 1 FROM jobs      WHERE source = 'git' AND repo_id = ?1)
		         OR EXISTS (SELECT 1 FROM workflows WHERE source = 'git' AND repo_id = ?1)
		         OR EXISTS (SELECT 1 FROM scripts   WHERE repo_id = ?1)
		         OR EXISTS (SELECT 1 FROM schedules WHERE source = 'git' AND repo_id = ?1)
		         OR EXISTS (SELECT 1 FROM scopes    WHERE source = 'git' AND repo_id = ?1))`, repoid.Global).Scan(&left)
	if err != nil {
		return false, fmt.Errorf("read the built-in agency's repository: %w", err)
	}
	return left, nil
}

// ListRepos is every agency's repository, by its agency's name. The built-in
// agency's row is not one (2.5.3, CA-6): a leftover connection is read on its
// own (CentralLeftoverRepo, GET /agencies/global/git-repo).
func ListRepos(ctx context.Context, database *sql.DB, appCfg *config.Config) ([]*Repo, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT g.id FROM git_repos g LEFT JOIN agencies a ON a.id = g.agency_id
		 WHERE g.id <> ? ORDER BY COALESCE(a.name, ''), g.id`, repoid.Global)
	if err != nil {
		return nil, fmt.Errorf("list git_repos: %w", err)
	}
	var ids []string
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
// The built-in agency has no repository (2.5.3, CA-6): ErrCentralHasNoRepository.
// An agency's must be a URL, and CheckRepoURL decides it before anything is
// written: the scheme, no user in it, and a host on the list.
//
// The caller starts or restarts the repository's sync service afterwards
// (gitlab.Registry.Restart), which is what first syncs a new connection.
func PutAgencyRepo(ctx context.Context, database *sql.DB, appCfg *config.Config, agencyID string, inp GitlabConfig, actor string) (repo *Repo, created bool, err error) {
	if agencyID == agencyid.Global {
		return nil, false, ErrCentralHasNoRepository
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
	if created {
		// The connection itself is a row in the agency's own log (2.5.0 C1,
		// the owner's answer to question 75); the settings saved with it have
		// theirs ("GitLab Connection", naming the repository).
		audit(ctx, database, agencyID, actor, "Settings", "connected", "Repository connection", "repository "+repoID)
	}
	repo, err = GetRepo(ctx, database, appCfg, repoID)
	return repo, created, err
}

// RepoProblem is one thing a repository's last sync had to say about a file
// or about the repository (GR-30, git_sync_problems).
type RepoProblem struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
}

// ListRepoProblems reads a repository's problems as of its last sync: errors
// first, then by file.
func ListRepoProblems(ctx context.Context, database *sql.DB, repoID string) ([]RepoProblem, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT path, kind, severity, message, first_seen, last_seen
		  FROM git_sync_problems WHERE repo_id = ?
		 ORDER BY (severity = 'error') DESC, path, kind, message`, repoID)
	if err != nil {
		return nil, fmt.Errorf("read the repository's problems: %w", err)
	}
	defer rows.Close()
	out := []RepoProblem{}
	for rows.Next() {
		var p RepoProblem
		if err := rows.Scan(&p.Path, &p.Kind, &p.Severity, &p.Message, &p.FirstSeen, &p.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RepoAgency is the agency a repository belongs to. The id 'global' is the
// built-in agency's whether or not its row is there (its history is still
// read, and its leftover removed, since 2.5.3: CA-6); any other without a row
// is ErrRepoNotFound.
func RepoAgency(ctx context.Context, database *sql.DB, repoID string) (string, error) {
	if repoID == "" || repoID == repoid.Global {
		return agencyid.Global, nil
	}
	var agency string
	err := database.QueryRowContext(ctx, `SELECT agency_id FROM git_repos WHERE id = ?`, repoID).Scan(&agency)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRepoNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read the repository's agency: %w", err)
	}
	return agency, nil
}

// RepoTest is what a test of a connection reaches: the URL, the token and the
// branch, each from the request when the request gives it and from the stored
// connection otherwise.
type RepoTest struct {
	URL, Token, Branch string
}

// ResolveRepoTest works out what "test this connection" is to reach. A URL,
// token or branch the request leaves out (for the token: or sends as the mask
// a read returned) is the stored one, when a repository is connected. The URL
// of an agency's repository is held to GR-10 before anything is reached.
func ResolveRepoTest(ctx context.Context, database *sql.DB, appCfg *config.Config, repoID string, connected bool, inp GitlabConfig) (RepoTest, error) {
	t := RepoTest{URL: strings.TrimSpace(inp.RepoUrl), Branch: strings.TrimSpace(inp.WriteBranch)}
	if inp.Pat != "" && !isMaskedSecret(inp.Pat) {
		t.Token = inp.Pat
	}
	if connected || repoID == repoid.Global {
		storedURL, storedToken, err := ResolveRepoRuntime(ctx, database, appCfg, repoID)
		if err != nil && !errors.Is(err, ErrRepoNotFound) {
			return t, err
		}
		if t.URL == "" {
			t.URL = storedURL
		}
		if t.Token == "" {
			t.Token = storedToken
		}
		if t.Branch == "" {
			if conn, cerr := GetGitlabConfig(ctx, database, appCfg, repoID); cerr == nil {
				t.Branch = conn.WriteBranch
			}
		}
	}
	if t.URL == "" {
		return t, fmt.Errorf("%w: repoUrl is required to test a connection", ErrValidation)
	}
	if t.Branch == "" {
		t.Branch = "main"
	}
	if err := CheckRepoURL(ctx, database, repoID, t.URL); err != nil {
		return t, err
	}
	return t, nil
}

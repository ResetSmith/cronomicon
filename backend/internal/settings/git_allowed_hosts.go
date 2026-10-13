package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// The hosts an agency's repository may be on (2.4.0; GR-10).
//
// An agency's administrator connects the agency's repository (GR-9), which is
// a URL the SERVER then fetches, with whatever the server can reach. So the
// URL is held to a list that a global administrator maintains, and to the two
// schemes the outbound guard covers. go-git also takes file://, a bare path
// and ssh://, and a path is a way to read the server's own disk: the clone of
// another repository, for one.
//
// Global's repository is exempt, as it always was: a global administrator
// writes it, and the same person writes this list.
//
// The list is one setting, "gitAllowedHosts", a JSON list of entries. An entry
// is a host name, which allows that host on any port, or host:port, which
// allows that port alone. Compared without regard to case. An empty list
// allows no host: until a global administrator names one, no agency's
// repository can be connected or synced.

const gitAllowedHostsKey = "gitAllowedHosts"

// ErrRepoURLNotAllowed marks an agency's repository URL that GR-10 refuses.
// It wraps ErrValidation: a handler answers 422 with the sentence.
var ErrRepoURLNotAllowed = fmt.Errorf("%w: repository URL not allowed", ErrValidation)

// GitAllowedHosts reads the list, normalized. A stored value that cannot be
// read is an empty list: nothing is allowed that the list does not say.
func GitAllowedHosts(ctx context.Context, database *sql.DB) ([]string, error) {
	var raw sql.NullString
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, gitAllowedHostsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", gitAllowedHostsKey, err)
	}
	return parseGitAllowedHosts(raw.String), nil
}

func parseGitAllowedHosts(raw string) []string {
	var in []string
	if json.Unmarshal([]byte(raw), &in) != nil {
		return []string{}
	}
	out, err := normalizeGitAllowedHosts(in)
	if err != nil {
		return []string{}
	}
	return out
}

// normalizeGitAllowedHosts validates a list as it is written: each entry a
// host or host:port and nothing else, lower-cased, without duplicates, sorted.
func normalizeGitAllowedHosts(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		host, port, err := splitHostEntry(e)
		if err != nil {
			return nil, fmt.Errorf("%w: gitAllowedHosts entry %q: %s", ErrValidation, e, err)
		}
		if port != "" {
			e = net.JoinHostPort(host, port)
		} else {
			e = host
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out, nil
}

// splitHostEntry reads "host" or "host:port" (an IPv6 address in brackets when
// it carries a port). It refuses anything that is more than that: a scheme, a
// path, a user, a wildcard.
func splitHostEntry(e string) (host, port string, err error) {
	if strings.ContainsAny(e, "/@?#* \t") || strings.Contains(e, "://") {
		return "", "", errors.New("a host name, or host:port, and nothing else (no scheme, path, user or wildcard)")
	}
	host = e
	if h, p, serr := net.SplitHostPort(e); serr == nil {
		host, port = h, p
		n := 0
		if _, perr := fmt.Sscanf(p, "%d", &n); perr != nil || n < 1 || n > 65535 || fmt.Sprint(n) != p {
			return "", "", errors.New("the port is not a number from 1 to 65535")
		}
	} else if strings.Count(e, ":") == 1 {
		return "", "", errors.New("the port is not a number from 1 to 65535")
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", "", errors.New("no host")
	}
	return host, port, nil
}

func writeGitAllowedHosts(ctx context.Context, database *sql.DB, hosts []string, actor, now string) error {
	b, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx,
		`INSERT INTO settings (key, value, last_modified_by, last_modified_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value,
		  last_modified_by=excluded.last_modified_by,
		  last_modified_at=excluded.last_modified_at`,
		gitAllowedHostsKey, string(b), actor, now)
	if err != nil {
		return fmt.Errorf("upsert setting %q: %w", gitAllowedHostsKey, err)
	}
	return nil
}

// CheckRepoURL is GR-10 for one repository's URL. Nil for Global's repository,
// whatever its URL, and for an empty URL (a connection that is not configured
// fetches nothing). For an agency's: http or https, no user in the URL (the
// token is stored apart, encrypted; a URL is shown and logged), and a host
// that the list allows.
//
// It is asked when the connection is written and tested, and again by every
// sync (gitlab.Service), so that a host taken off the list stops being
// fetched, and a row written around the API is held to it too.
func CheckRepoURL(ctx context.Context, database *sql.DB, repoID, rawURL string) error {
	if repoID == "" || repoID == repoid.Global || strings.TrimSpace(rawURL) == "" {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: an agency's repository is named by an https:// (or http://) URL", ErrRepoURLNotAllowed)
	}
	if s := strings.ToLower(u.Scheme); s != "https" && s != "http" {
		return fmt.Errorf("%w: an agency's repository is reached over https (or http); %s:// is not accepted", ErrRepoURLNotAllowed, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%w: the URL must not carry a user or a password; the token is entered apart", ErrRepoURLNotAllowed)
	}
	allowed, err := GitAllowedHosts(ctx, database)
	if err != nil {
		return err
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	for _, e := range allowed {
		eh, ep, serr := splitHostEntry(e)
		if serr != nil || eh != host {
			continue
		}
		if ep == "" || ep == port || (port == "" && ep == defaultPort(u.Scheme)) {
			return nil
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("%w: no host is allowed for an agency's repository yet. A global administrator lists the "+
			"allowed hosts in Settings (gitAllowedHosts)", ErrRepoURLNotAllowed)
	}
	return fmt.Errorf("%w: the host %s is not on the list of hosts an agency's repository may be on. A global "+
		"administrator maintains it in Settings (gitAllowedHosts)", ErrRepoURLNotAllowed, u.Host)
}

func defaultPort(scheme string) string {
	if strings.EqualFold(scheme, "http") {
		return "80"
	}
	return "443"
}

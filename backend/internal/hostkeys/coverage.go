// Package hostkeys answers one question from the database alone: which of a
// scope's hosts does a runner trust (the scope-bound-runners plan, SB band).
//
// It is a leaf — execspec and the standard library only — because two packages
// that cannot import each other both ask: the runner service (the coverage
// view, the scope scan) and settings (the readiness line of the bind preview).
//
// "Trusts" means: a line in the runner's known_hosts file would match the
// address the runner dials for that host. Two sources say so, and they are kept
// apart all the way to the screen:
//
//	approved   an in-force row in host_key_ledger — approved in Cronomicon
//	queued     approved, and not yet sent to the runner
//	in-file    reported by the runner from its own file (runner_known_hosts),
//	           with no approval here behind it
//
// The aim is never to say "trusted" for a host the runner would refuse. The
// other error — a false alarm — costs a scan; this one costs a failed run that
// the screen said would work.
package hostkeys

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
)

// Trust states of one scope host on one runner.
const (
	StateApproved = "approved" // approved here, and nothing says it is not on the runner
	StateQueued   = "queued"   // approved here, not yet sent to the runner
	StateInFile   = "in-file"  // in the runner's own file; not approved here
	StateNone     = ""         // not trusted: a run on this host fails
)

// Queryer is the read surface these functions need; *sql.DB and *sql.Tx both
// satisfy it.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ScopeHost is one host of a scope as a runner would reach it.
type ScopeHost struct {
	// Host is the scope's own name for it.
	Host string `json:"host"`
	// Pattern is the known_hosts host the runner's verifier looks up when it
	// connects: the dial address, bracketed with its port when that is not 22.
	Pattern string `json:"pattern"`
	// Target is what a scan dials (address, or address:port). Empty when the
	// host cannot be scanned; NotScannable then says why.
	Target       string `json:"target"`
	NotScannable string `json:"notScannable,omitempty"`
	// Via is the bastion the host is reached through, as the runner is given
	// it, and ViaPattern the known_hosts host of that hop. The runner verifies
	// BOTH hops against its file, so such a host is trusted only when both
	// keys are. The bastion itself is dialled directly and can be scanned.
	Via        string `json:"via,omitempty"`
	ViaPattern string `json:"viaPattern,omitempty"`
}

// Target is the string a scan is asked to dial for a host record, and the
// string the scanned key comes back under: the bare address on port 22,
// address:port otherwise.
func Target(t execspec.Target) string {
	addr := t.Address
	if addr == "" {
		addr = t.Name
	}
	if t.Port == 0 || t.Port == 22 {
		return addr
	}
	return net.JoinHostPort(addr, strconv.Itoa(t.Port))
}

// Pattern is the known_hosts host a scan target (or any host[:port]) is keyed
// under. It is knownhosts.Normalize, named, so every caller that has to agree
// on the form goes through one function.
func Pattern(target string) string { return knownhosts.Normalize(target) }

// PlanScope expands a scope into its hosts. A host with no SSH host record has
// no address on file — a runner reaches it by its inventory name, and a scope
// scan leaves it out. A host reached through a bastion cannot be dialled
// directly, and scanning through a bastion is not supported: its key has to be
// provided. Both are returned, with the reason; nothing is dropped silently.
func PlanScope(ctx context.Context, database *sql.DB, scopeName string) ([]ScopeHost, error) {
	names, err := execspec.ScopeHosts(ctx, database, scopeName)
	if err != nil {
		return nil, err
	}
	out := make([]ScopeHost, 0, len(names))
	for _, n := range names {
		t, err := execspec.HostByName(ctx, database, scopeName, n)
		if err != nil {
			return nil, err
		}
		switch {
		case t == nil:
			out = append(out, ScopeHost{Host: n, Pattern: Pattern(n),
				NotScannable: "no SSH host record, so there is no address to scan"})
		case t.Via != "":
			out = append(out, ScopeHost{Host: n, Pattern: Pattern(Target(*t)), Via: t.Via, ViaPattern: Pattern(t.Via),
				NotScannable: "reached through bastion " + t.Via + "; not scannable, provide the key"})
		default:
			tg := Target(*t)
			out = append(out, ScopeHost{Host: n, Pattern: Pattern(tg), Target: tg})
		}
	}
	return out, nil
}

// Trusted is what a runner is known to trust.
type Trusted struct {
	// approved maps a known_hosts host to StateApproved or StateQueued, for the
	// keys in force in the ledger that are — as far as anything shows — on the
	// runner, or on their way there.
	approved map[string]string
	// lines are the plain, named lines of the runner's reported file, each with
	// its host patterns; the verifier's own matching rules are applied to them.
	lines [][]filePattern
}

type filePattern struct {
	negate     bool
	host, port string
}

// LoadTrusted reads both sources for one runner.
//
// An in-force ledger key counts as approved only while nothing contradicts it:
// one not yet sent is "queued", and one the runner's own report shows is NOT in
// its file is not counted at all — the scope would otherwise read as covered
// while the runner refuses the host. (A truncated report proves no absence, and
// a runner that has never reported is taken at the ledger's word.)
//
// File lines that are hashed, or carry a marker (@revoked, @cert-authority),
// contribute nothing to host matching: the first cannot be matched to a name,
// the second is not a statement that the host is trusted.
func LoadTrusted(ctx context.Context, q Queryer, runnerID string) (Trusted, error) {
	t := Trusted{approved: map[string]string{}}

	var reported sql.NullString
	var truncated bool
	if err := q.QueryRowContext(ctx,
		`SELECT known_hosts_reported_at, known_hosts_truncated FROM runners WHERE id = ?`, runnerID).
		Scan(&reported, &truncated); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return t, err
	}
	fileIsKnown := reported.Valid && !truncated

	type fileKey struct {
		hosts  []string
		hashed bool
	}
	held := map[string][]fileKey{} // key type + fingerprint → the lines carrying it
	rows, err := q.QueryContext(ctx, `
		SELECT hosts, hashed, key_type, fingerprint FROM runner_known_hosts
		 WHERE runner_id = ? AND marker = ''`, runnerID)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var hosts, keyType, fingerprint string
		var hashed bool
		if err := rows.Scan(&hosts, &hashed, &keyType, &fingerprint); err != nil {
			rows.Close()
			return t, err
		}
		names := strings.Split(hosts, ",")
		held[keyType+"\x00"+fingerprint] = append(held[keyType+"\x00"+fingerprint], fileKey{names, hashed})
		if hashed {
			continue
		}
		var line []filePattern
		for _, h := range names {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			p := filePattern{negate: strings.HasPrefix(h, "!")}
			p.host, p.port = knownhostsline.HostPort(strings.TrimPrefix(h, "!"))
			line = append(line, p)
		}
		t.lines = append(t.lines, line)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return t, err
	}
	rows.Close()

	rows, err = q.QueryContext(ctx, `
		SELECT host, key_type, fingerprint, delivered_at IS NOT NULL FROM host_key_ledger
		 WHERE runner_id = ? AND decision = 'approved' AND superseded_at IS NULL`, runnerID)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var host, keyType, fingerprint string
		var delivered bool
		if err := rows.Scan(&host, &keyType, &fingerprint, &delivered); err != nil {
			return t, err
		}
		switch {
		case !delivered:
			t.approved[host] = StateQueued
		case fileIsKnown && !slices.ContainsFunc(held[keyType+"\x00"+fingerprint], func(k fileKey) bool {
			return k.hashed || slices.Contains(k.hosts, host)
		}):
			// Sent, and the runner's own file says it is not there.
		default:
			t.approved[host] = StateApproved
		}
	}
	return t, rows.Err()
}

// State says whether the runner trusts a scope host. A host behind a bastion
// takes two verified hops, so it is trusted only as far as the weaker of the
// two: untrusted if either key is missing, and "approved" only if both were
// approved here.
func (t Trusted) State(h ScopeHost) string {
	s := t.pattern(h.Pattern)
	if h.ViaPattern == "" || s == StateNone {
		return s
	}
	b := t.pattern(h.ViaPattern)
	switch {
	case b == StateNone:
		return StateNone
	case s == StateQueued || b == StateQueued:
		return StateQueued
	case s == StateApproved && b == StateApproved:
		return StateApproved
	default:
		return StateInFile
	}
}

// pattern answers for one known_hosts host. The file's lines are matched the
// way the runner's verifier matches them: a line trusts the host when one of
// its patterns matches the host by wildcard AND names the same port (an
// unbracketed pattern, `*` included, means port 22 only), and none of its
// negated patterns does.
func (t Trusted) pattern(p string) string {
	if s := t.approved[p]; s != "" {
		return s
	}
	host, port := knownhostsline.HostPort(p)
	for _, line := range t.lines {
		matched := false
		for _, fp := range line {
			if fp.port != port || !wildcardMatch(fp.host, host) {
				continue
			}
			if fp.negate {
				matched = false
				break
			}
			matched = true
		}
		if matched {
			return StateInFile
		}
	}
	return StateNone
}

// wildcardMatch implements the two known_hosts wildcards: '*' (any run of
// characters) and '?' (exactly one).
func wildcardMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for i := 0; i <= len(s); i++ {
				if wildcardMatch(pattern[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
		}
		pattern, s = pattern[1:], s[1:]
	}
	return len(s) == 0
}

// Missing counts the hosts of a scope a runner does not trust, with the scope's
// host count, so a caller can say "3 of 12".
func Missing(ctx context.Context, database *sql.DB, scopeName, runnerID string) (missing, total int, err error) {
	hosts, err := PlanScope(ctx, database, scopeName)
	if err != nil {
		return 0, 0, err
	}
	trusted, err := LoadTrusted(ctx, database, runnerID)
	if err != nil {
		return 0, 0, err
	}
	for _, h := range hosts {
		if trusted.State(h) == StateNone {
			missing++
		}
	}
	return missing, len(hosts), nil
}

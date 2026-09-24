package tagutil

import (
	"net/url"
	"strings"
)

// Filter is a normalized tag-filter request extracted from a list endpoint's
// query string: the set of wanted tags plus whether a row must carry all of them
// (AND) or at least one (OR, the default). It powers both the SQL predicate
// (SQLFilter) and the in-memory predicate (Match) so every tagged list endpoint
// filters identically.
type Filter struct {
	Tags     []string
	MatchAll bool // true ⇒ AND (row must carry every tag); false ⇒ OR (any).
}

// Empty reports whether the filter selects nothing (no tags given) — callers
// skip filtering entirely in that case.
func (f Filter) Empty() bool { return len(f.Tags) == 0 }

// ParseQuery extracts a tag Filter from a list endpoint's query values. It
// accepts repeated ?tag=a&tag=b as well as a comma-joined ?tags=a,b (both are
// unioned), and ?tagMatch=all|any (default any ⇒ OR). Values are trimmed and
// de-duped case-insensitively (first casing wins); blanks are dropped. This is
// the single parser shared by all tagged list handlers so their filter grammar
// can't drift apart.
func ParseQuery(q url.Values) Filter {
	raw := append([]string(nil), q["tag"]...)
	for _, csv := range q["tags"] {
		raw = append(raw, strings.Split(csv, ",")...)
	}
	seen := make(map[string]bool, len(raw))
	tags := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		tags = append(tags, t)
	}
	return Filter{Tags: tags, MatchAll: strings.EqualFold(q.Get("tagMatch"), "all")}
}

// SQLFilter returns a WHERE-clause fragment (already parenthesized) plus its bind
// args that keep only rows whose JSON-array tags column contains the wanted tags.
// Matching is per whole JSON element (json_each) — NOT a substring LIKE — so tag
// "prod" never matches "production". col is a trusted caller-supplied column
// reference (e.g. "j.tags") and is interpolated, never bound; the tag VALUES are
// always bound. Returns "", nil when the filter is empty (caller appends nothing).
func (f Filter) SQLFilter(col string) (string, []any) {
	if f.Empty() {
		return "", nil
	}
	clauses := make([]string, len(f.Tags))
	args := make([]any, len(f.Tags))
	for i, t := range f.Tags {
		clauses[i] = "EXISTS (SELECT 1 FROM json_each(" + col + ") WHERE value = ?)"
		args[i] = t
	}
	joiner := " OR "
	if f.MatchAll {
		joiner = " AND "
	}
	return "(" + strings.Join(clauses, joiner) + ")", args
}

// Match is the in-memory equivalent of SQLFilter for handlers that filter an
// already-loaded slice rather than in SQL (env vars, secrets, SSH creds,
// runners). entTags is the row's parsed tag set. An empty filter matches
// everything. Tag comparison is exact (whole element), matching SQLFilter.
func (f Filter) Match(entTags []string) bool {
	if f.Empty() {
		return true
	}
	set := make(map[string]bool, len(entTags))
	for _, t := range entTags {
		set[t] = true
	}
	if f.MatchAll {
		for _, w := range f.Tags {
			if !set[w] {
				return false
			}
		}
		return true
	}
	for _, w := range f.Tags {
		if set[w] {
			return true
		}
	}
	return false
}

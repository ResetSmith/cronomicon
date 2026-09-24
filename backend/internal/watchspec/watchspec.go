// Package watchspec owns the file-arrival watch vocabulary (ET-D).
//
// It is its own leaf package for the reason cronutil holds ParseSpec and the
// concurrency-policy vocabulary: BOTH authoring paths must agree on what a
// watch is — the Git YAML sync and the in-app compose API — and gitlab cannot
// import api. Putting it anywhere else means two parsers, which is how
// `Replace` came to be accepted by both and honoured by neither.
package watchspec

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// DefaultStableSeconds is how long a file's size must hold steady before it
// counts as arrived, when a watch does not say.
//
// Five seconds is chosen to be obviously a stability check rather than a
// throttle: it catches the common case of a file still being written or copied
// in, without making a small file wait noticeably. A large file over a slow
// link needs more, which is why the field is per-watch.
const DefaultStableSeconds = 5

// MaxWatchesPerJob bounds a single job's watch list. A Go constant, not a
// setting: it is a footgun guard, and the agent scans every glob on every tick.
const MaxWatchesPerJob = 10

// Watch is one path glob a job listens on.
type Watch struct {
	Path          string `json:"path" yaml:"path"`
	StableSeconds int    `json:"stableSeconds,omitempty" yaml:"stable_seconds,omitempty"`
}

// Parse reads the stored JSON array. An empty or absent value is no watches,
// which is every job that does not use the feature.
func Parse(raw string) ([]Watch, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "[]" {
		return nil, nil
	}
	var out []Watch
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse watch spec: %w", err)
	}
	return out, nil
}

// Marshal serializes a watch list for storage. An empty list stores as NULL
// rather than "[]", so "has no watches" is one value and not two.
func Marshal(ws []Watch) any {
	if len(ws) == 0 {
		return nil
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return nil
	}
	return string(b)
}

// Validate returns the problems with a watch list, empty when it is usable.
//
// The rules are deliberately about what would FAIL SILENTLY rather than about
// taste: a relative path resolves against whatever directory the agent happens
// to have been started in, and a glob ending in a separator would match a
// directory and fire on every poll forever.
func Validate(ws []Watch) []string {
	var errs []string
	if len(ws) > MaxWatchesPerJob {
		errs = append(errs, fmt.Sprintf("a job may declare at most %d watches", MaxWatchesPerJob))
	}
	seen := map[string]bool{}
	for i, w := range ws {
		p := strings.TrimSpace(w.Path)
		switch {
		case p == "":
			errs = append(errs, fmt.Sprintf("watch %d: path is required", i+1))
			continue
		case !filepath.IsAbs(p):
			errs = append(errs, fmt.Sprintf("watch %d: path must be absolute (%q would resolve against the agent's working directory)", i+1, p))
			continue
		case strings.Contains(p, ".."):
			errs = append(errs, fmt.Sprintf("watch %d: path may not contain '..'", i+1))
			continue
		case strings.HasSuffix(p, "/"):
			errs = append(errs, fmt.Sprintf("watch %d: path must name files, not a directory (try %s*)", i+1, p))
			continue
		}
		if _, err := filepath.Match(p, "/probe"); err != nil {
			errs = append(errs, fmt.Sprintf("watch %d: %q is not a valid glob: %v", i+1, p, err))
			continue
		}
		if seen[p] {
			errs = append(errs, fmt.Sprintf("watch %d: duplicate path %q", i+1, p))
		}
		seen[p] = true
		if w.StableSeconds < 0 || w.StableSeconds > 3600 {
			errs = append(errs, fmt.Sprintf("watch %d: stableSeconds must be between 0 and 3600", i+1))
		}
	}
	return errs
}

// Normalize trims and applies defaults, so the stored form and the wire form
// agree and the agent never has to guess.
func Normalize(ws []Watch) []Watch {
	out := make([]Watch, 0, len(ws))
	for _, w := range ws {
		p := strings.TrimSpace(w.Path)
		if p == "" {
			continue
		}
		if w.StableSeconds <= 0 {
			w.StableSeconds = DefaultStableSeconds
		}
		w.Path = p
		out = append(out, w)
	}
	return out
}

// PathAllowed reports whether a watch path sits under one of the agent's
// allowlisted roots.
//
// This is the agent-side half of the security posture, and the reason the
// allowlist exists at all: the SERVER tells a runner which paths to watch, so
// without it a careless or compromised server could point a runner at a
// sensitive directory and learn what lands there. The check is the agent's,
// applied to what the server sent — never the other way round.
//
// An empty allowlist permits NOTHING rather than everything, matching the
// scope-grant rule (empty means zero access, "*" is the explicit sentinel).
func PathAllowed(path string, roots []string) bool {
	if len(roots) == 0 {
		return false
	}
	clean := filepath.Clean(path)
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" || root == "." {
			continue
		}
		if clean == root {
			return true
		}
		if strings.HasPrefix(clean, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

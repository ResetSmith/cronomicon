// Package tagutil holds the shared helpers for the free-form label sets carried
// by jobs, scripts, workflows, schedules, env vars, secrets and SSH credentials.
// Parse (a tags JSON-array column → slice) is used by both the api and settings
// packages; Normalize enforces the input caps for the tag-write handlers. It is a
// leaf package (no internal imports) so both packages can depend on it (CC.15).
package tagutil

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxCount and MaxLen bound a single entity's tag set (scripts-tags.md D6): light
// caps so one entity's tags can't balloon the JSON column or the table cell.
const (
	MaxCount = 30
	MaxLen   = 64
)

// Parse decodes a tags JSON-array column into a non-nil slice: empty / "[]" /
// malformed / null all yield []string{}.
func Parse(raw string) []string {
	if raw == "" || raw == "[]" {
		return []string{}
	}
	var t []string
	if err := json.Unmarshal([]byte(raw), &t); err != nil || t == nil {
		return []string{}
	}
	return t
}

// Normalize applies the D6 rules: trim each tag, drop blanks, reject control
// characters, de-dupe case-insensitively (first casing wins), and enforce the
// length + count caps. Returns a non-nil slice so an empty/cleared set marshals
// to "[]". A violation is an error (handlers map it to a 422).
func Normalize(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > MaxLen {
			return nil, fmt.Errorf("tag %q exceeds %d characters", t, MaxLen)
		}
		for _, r := range t {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("tag %q contains a control character", t)
			}
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		if len(out) >= MaxCount {
			return nil, fmt.Errorf("too many tags (max %d)", MaxCount)
		}
		seen[key] = true
		out = append(out, t)
	}
	return out, nil
}

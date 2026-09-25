package gitlab

import (
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// The two body helpers sync.go uses. They lived in migrate.go — the one-time
// `cronomicon migrate-scripts` engine — until the DD band deleted it (v1.5.41);
// they are sync's, not the migration's.

// readBodyAt resolves an executable body to its raw bytes against dir: an inline
// command/script returns its string bytes; a repo-relative scriptPath is read
// from the clone behind the canonical path-traversal guard (Clean → reject
// ../abs → Join → EvalSymlinks → Rel re-check → ReadFile). Reading once and
// returning the bytes lets a caller both hash AND scan the exact same bytes (no
// double read, no mid-sync tree-rewrite skew between the hash and the lint scan).
func readBodyAt(dir, command, script, scriptPath string) ([]byte, error) {
	switch {
	case command != "":
		return []byte(command), nil
	case script != "":
		return []byte(script), nil
	case scriptPath != "":
		// Hashing + body-lint need the FULL bytes (limit 0 = uncapped). Shares the
		// one path-traversal guard with the SSH executor and the content endpoint.
		data, _, err := execspec.SafeReadRepoFile(dir, scriptPath, 0)
		if err != nil {
			return nil, err
		}
		return data, nil
	default:
		return nil, fmt.Errorf("no executable body")
	}
}

// hashBodyAt computes the Decision-8 content hash of an executable body resolved
// against dir (inline string, or the bytes of a repo-relative scriptPath file).
func hashBodyAt(dir, command, script, scriptPath string) (string, error) {
	raw, err := readBodyAt(dir, command, script, scriptPath)
	if err != nil {
		return "", err
	}
	return ContentHash(raw), nil
}

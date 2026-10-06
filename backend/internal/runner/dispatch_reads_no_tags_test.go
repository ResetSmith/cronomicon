package runner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDispatchReadsNoTags is the SB band's standing rule as a build failure: a
// tag is a label, and nothing that decides WHERE or WHETHER a run executes may
// read one. For three years of releases that was true of every tagged entity
// but one — a runner's tags were matched by the claim query through a
// `runner_tags` projection — and the one exception was enough to make "tags are
// only for organising" a sentence nobody could say about the product.
//
// It reads the production source of every file on the dispatch path — the two
// claim queries, the eligibility probe, the unclaimable explainer, the executor
// resolver, the binding rules, file-watch distribution and the run-row writer —
// and fails on any SQL-ish mention of a tags column or the retired projection.
// Comments are stripped first, so the history those files keep in prose (and
// this band adds to) does not trip it.
//
// Need a tag to decide something? That is a new concept, and it wants its own
// name and its own column — which is what a scope's runner binding is.
func TestDispatchReadsNoTags(t *testing.T) {
	files := []string{
		"poll.go",
		"filewatch.go",
		filepath.Join("..", "sshexec", "sshexec.go"),
		filepath.Join("..", "execspec", "execspec.go"),
		filepath.Join("..", "execspec", "unclaimable.go"),
		filepath.Join("..", "execspec", "executor.go"),
		filepath.Join("..", "execspec", "scopebinding.go"),
		filepath.Join("..", "scheduler", "runrow.go"),
	}
	lineComment := regexp.MustCompile(`(?m)//.*$`)
	sqlComment := regexp.MustCompile(`(?m)--.*$`)
	// A tags column (bare or qualified: `tags`, `rn.tags`, `runners.tags`), a
	// json_each over one, or either name of the retired pin.
	forbidden := regexp.MustCompile(`(?i)\b(runner_tags?|[a-z_]*\.tags|json_each\([^)]*tags[^)]*\))\b|\btags\s*(=|,|\bFROM\b|\bIN\b)`)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := sqlComment.ReplaceAllString(lineComment.ReplaceAllString(string(raw), ""), "")
		for i, line := range strings.Split(src, "\n") {
			if forbidden.MatchString(line) {
				t.Errorf("%s:%d reads a tag on the dispatch path: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

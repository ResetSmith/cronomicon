package gitlab

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/storage/memory"
)

// ConnectionTest is the answer to "test this connection" (2.4.0 Phase R6;
// GR-9). OK: the remote answered and listed its refs. BranchFound: the branch
// the connection names is among them. Error is the remote's own words when it
// did not answer, with nothing of the token in them.
type ConnectionTest struct {
	OK          bool     `json:"ok"`
	Branch      string   `json:"branch"`
	BranchFound bool     `json:"branchFound"`
	Branches    []string `json:"branches"`
	Error       string   `json:"error,omitempty"`
}

// testConnectionBound is how long a test may take: it is a person waiting at a
// form, not a transfer.
const testConnectionBound = 30 * time.Second

// TestConnection asks a remote for its refs, as a sync's fetch begins by
// doing, through the same outbound guard, and fetches nothing. The caller has
// held the URL to GR-10.
func TestConnection(ctx context.Context, cfg *config.Config, url, token, branch string) ConnectionTest {
	res := ConnectionTest{Branch: branch, Branches: []string{}}
	(&Service{Cfg: cfg}).installGuardedGitTransport() // SU-7/SU-8: guard go-git http(s) egress (once)
	remote := gogit.NewRemote(memory.NewStorage(), &gogitconfig.RemoteConfig{Name: "origin", URLs: []string{url}})
	opts := &gogit.ListOptions{}
	if auth := (&Service{token: token}).gitAuth(); auth != nil {
		opts.Auth = auth
	}
	tctx, cancel := context.WithTimeout(ctx, testConnectionBound)
	defer cancel()
	refs, err := remote.ListContext(tctx, opts)
	if err != nil {
		res.Error = err.Error()
		if token != "" {
			res.Error = strings.ReplaceAll(res.Error, token, "••••")
		}
		return res
	}
	res.OK = true
	for _, ref := range refs {
		if ref.Name().IsBranch() {
			name := ref.Name().Short()
			res.Branches = append(res.Branches, name)
			if name == branch {
				res.BranchFound = true
			}
		}
	}
	sort.Strings(res.Branches)
	return res
}

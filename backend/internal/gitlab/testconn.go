package gitlab

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/storage/memory"
)

// ConnectionTest is the answer to "test this connection" (2.4.0 Phase R6;
// GR-9). OK: the remote answered and listed its refs. BranchFound: the branch
// the connection names is among them. Error is the remote's own words when it
// did not answer, with nothing of the token in them.
//
// CanPush (2.4.0 Phase I3; §0a, IN-2; the owner's answer to question 38): the
// remote also offered to RECEIVE a push with this token — the handshake `git
// push` makes before it sends anything. Nothing is sent. GitLab, GitHub and
// Gitea refuse the handshake to a token that may only read. A protected
// branch's rule is not part of it: that shows at the first push, with its
// reason. PushError is the remote's words when it refused.
type ConnectionTest struct {
	OK          bool     `json:"ok"`
	Branch      string   `json:"branch"`
	BranchFound bool     `json:"branchFound"`
	Branches    []string `json:"branches"`
	Error       string   `json:"error,omitempty"`
	CanPush     bool     `json:"canPush"`
	PushError   string   `json:"pushError,omitempty"`
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
	var pushAuth transport.AuthMethod
	if a := (&Service{token: token}).gitAuth(); a != nil {
		pushAuth = a
	}
	if err := receivePackHandshake(tctx, url, pushAuth); err != nil {
		res.PushError = err.Error()
		if token != "" {
			res.PushError = strings.ReplaceAll(res.PushError, token, "••••")
		}
	} else {
		res.CanPush = true
	}
	return res
}

// receivePackHandshake asks the remote for what it advertises to a push, as
// `git push` does before it sends a pack, and sends nothing. Through the same
// transports, and so the same outbound guard, as the listing above.
func receivePackHandshake(ctx context.Context, url string, auth transport.AuthMethod) error {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return err
	}
	cli, err := client.NewClient(ep)
	if err != nil {
		return err
	}
	sess, err := cli.NewReceivePackSession(ep, auth)
	if err != nil {
		return err
	}
	defer sess.Close()
	_, err = sess.AdvertisedReferencesContext(ctx)
	return err
}

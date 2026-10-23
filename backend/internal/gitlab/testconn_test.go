package gitlab

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// "Test this connection" asks the remote for its branches and fetches nothing
// (GR-9): the remote answered or it did not, the branches it has, and whether
// the connection's branch is among them.
func TestTestingAConnectionListsTheRemotesBranches(t *testing.T) {
	_, bare, first := bareFixture(t) // two branches: the first, and release
	ctx := context.Background()

	got := TestConnection(ctx, nil, bare, "", first)
	want := []string{first, "release"}
	if first > "release" {
		want = []string{"release", first}
	}
	if !got.OK || got.Error != "" || !got.BranchFound || got.Branch != first || !reflect.DeepEqual(got.Branches, want) {
		t.Errorf("testing a remote that answers: %+v, want ok with its branches %v and the branch found", got, want)
	}
	if got := TestConnection(ctx, nil, bare, "", "no-such-branch"); !got.OK || got.BranchFound || len(got.Branches) != 2 {
		t.Errorf("testing with a branch the remote does not have: %+v, want ok, the branch not found", got)
	}
	gone := TestConnection(ctx, nil, filepath.Join(t.TempDir(), "no-such-repo.git"), "", first)
	if gone.OK || gone.Error == "" || gone.BranchFound || len(gone.Branches) != 0 {
		t.Errorf("testing a remote that is not there: %+v, want not ok, with the error", gone)
	}
}

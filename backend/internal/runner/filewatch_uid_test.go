package runner

import (
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// R2-4 — the watch echo is the one place in the wire protocol where a value the
// CLIENT hands back selects which job the server runs. These pin what the echo
// may and may not do.

func TestWatchEchoMatchesByUID(t *testing.T) {
	// Two jobs sharing a NAME across the source pools — legal today, and the
	// shape per-agency naming generalises.
	specs := []runnerproto.WatchSpec{
		{JobSource: "git", JobName: "ingest", JobUID: "uid-git", Path: "/srv/in/*.csv"},
		{JobSource: "amadeus", JobName: "ingest", JobUID: "uid-ama", Path: "/srv/in/*.csv"},
	}
	got, ok := matchDistributedWatch(sightingIn{
		JobUID: "uid-ama", JobSource: "git", JobName: "ingest", Path: "/srv/in/a.csv"}, specs)
	if !ok {
		t.Fatal("a uid-bearing sighting matched no distributed watch")
	}
	// The uid decides, and the SPEC is what fires — note the report's jobSource
	// said git while its uid said the amadeus job. The spec wins, so a runner
	// cannot mix one watch's identity with another's name to pick a job.
	if got.JobUID != "uid-ama" || got.JobSource != "amadeus" {
		t.Errorf("matched spec = %+v, want the amadeus job the uid named", got)
	}
}

// A v10 agent sends no uid; the (source, name) pair must still match, or every
// runner that has not been upgraded silently stops triggering on file arrival.
func TestWatchEchoFallsBackToNamePairForOldAgents(t *testing.T) {
	specs := []runnerproto.WatchSpec{
		{JobSource: "git", JobName: "ingest", JobUID: "uid-git", Path: "/srv/in/*.csv"},
	}
	got, ok := matchDistributedWatch(sightingIn{
		JobSource: "git", JobName: "ingest", Path: "/srv/in/a.csv"}, specs)
	if !ok {
		t.Fatal("a v10 agent's sighting was refused; upgraded servers would stop firing its watches")
	}
	// And it still fires from the server's copy, so the run is attributed to the
	// right identity even though the agent could not name it.
	if got.JobUID != "uid-git" {
		t.Errorf("matched spec uid = %q, want uid-git from the server's own copy", got.JobUID)
	}
}

// The anti-widening rule must survive the new field: echoing a uid that was
// never distributed to this runner selects nothing.
func TestWatchEchoCannotInventAUID(t *testing.T) {
	specs := []runnerproto.WatchSpec{
		{JobSource: "git", JobName: "ingest", JobUID: "uid-git", Path: "/srv/in/*.csv"},
	}
	if _, ok := matchDistributedWatch(sightingIn{
		JobUID: "uid-someone-elses", JobSource: "git", JobName: "ingest", Path: "/srv/in/a.csv"}, specs); ok {
		t.Error("a runner selected a job by echoing a uid it was never sent")
	}
}

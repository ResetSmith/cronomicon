package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"strings"
	"time"
)

// ET-D, server side.
//
// The properties worth defending are the ones whose failure is invisible: a
// file firing twice (a double-import), a file never firing (silence), and a
// runner being told about — or reporting on — a directory it has no business
// seeing.

func seedWatchJob(t *testing.T, svc *Service, source, name, scope, watchJSON string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO jobs (uid, name, source, run_type, scope, concurrency_policy, enabled, synced_at, watch_json)
		VALUES ('uid-'||?, ?, ?, 'bash', ?, 'Allow', 1, 't', ?)`, name, name, source, nullIfBlank(scope), watchJSON); err != nil {
		t.Fatalf("seed watch job: %v", err)
	}
}

func nullIfBlank(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func setCaps(t *testing.T, svc *Service, runnerID string, caps []string, protocol int) {
	t.Helper()
	b, _ := json.Marshal(caps)
	if _, err := svc.db.Exec(
		`UPDATE runners SET capabilities = ?, protocol_version = ? WHERE id = ?`,
		string(b), protocol, runnerID); err != nil {
		t.Fatalf("set caps: %v", err)
	}
}

// A capable, agency-matching runner receives the spec.
func TestWatchesAreDistributedToCapableRunners(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "runner-one", "online", []string{"bash"})
	setCaps(t, svc, "r1", []string{"bash", "watch"}, 10)
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv","stableSeconds":5}]`)

	got := watchesForRunner(ctx, svc.db, "r1", []string{"bash", "watch"})
	if len(got) != 1 {
		t.Fatalf("distributed %d specs, want 1", len(got))
	}
	if got[0].Path != "/srv/incoming/*.csv" || got[0].JobName != "ingest" || got[0].StableSeconds != 5 {
		t.Errorf("spec = %+v, want the job's watch verbatim", got[0])
	}
}

// An agent without the capability gets nothing. (The protocol-9 case went with
// the floor: every registered agent speaks v12+.)
func TestWatchesWithheldFromIncapableAgents(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "runner-one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)

	if got := watchesForRunner(ctx, svc.db, "r1", []string{"bash"}); len(got) != 0 {
		t.Errorf("an agent without the watch capability received %d specs", len(got))
	}
}

// A disabled or binned job is not watched — a watch must not be a way to run
// something the catalog says is off.
func TestWatchesSkipDisabledAndBinnedJobs(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "runner-one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "off", "", `[{"path":"/srv/a/*"}]`)
	seedWatchJob(t, svc, "git", "binned", "", `[{"path":"/srv/b/*"}]`)
	if _, err := svc.db.Exec(`UPDATE jobs SET enabled=0 WHERE name='off'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE jobs SET deleted_at='2026-08-11T00:00:00Z' WHERE name='binned'`); err != nil {
		t.Fatal(err)
	}

	if got := watchesForRunner(ctx, svc.db, "r1", []string{"bash", "watch"}); len(got) != 0 {
		t.Errorf("distributed %d specs for disabled/binned jobs, want 0", len(got))
	}
}

// The same arrival reported by two runners on a shared mount fires ONCE. For a
// job that ingests the file, firing twice is a double-import.
func TestSightingDeDupesAcrossRunners(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	insertRunner(t, svc, "r2", "two", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)

	sg := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv",
		SizeBytes: 1024, MTime: "2026-08-11T00:00:00Z"}

	first, err := svc.recordAndFireSighting(ctx, "r1", sg, specFor(sg))
	if err != nil || !first {
		t.Fatalf("first sighting did not fire (%v, %v)", first, err)
	}
	second, err := svc.recordAndFireSighting(ctx, "r2", sg, specFor(sg))
	if err != nil {
		t.Fatalf("second sighting errored: %v", err)
	}
	if second {
		t.Error("the same file reported by a second runner fired again — that is a double-import")
	}

	var runs int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='ingest'`).Scan(&runs)
	if runs != 1 {
		t.Errorf("enqueued %d runs for one arrival, want 1", runs)
	}
}

// A file REPLACED at the same path fires again — the common nightly-drop shape —
// while an unchanged file does not.
func TestReplacedFileFiresAgain(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)

	base := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/daily.csv",
		SizeBytes: 100, MTime: "2026-08-11T00:00:00Z"}
	if ok, _ := svc.recordAndFireSighting(ctx, "r1", base, specFor(base)); !ok {
		t.Fatal("first arrival did not fire")
	}
	if ok, _ := svc.recordAndFireSighting(ctx, "r1", base, specFor(base)); ok {
		t.Error("an unchanged file fired again")
	}
	replaced := base
	replaced.SizeBytes = 200
	replaced.MTime = "2026-08-12T00:00:00Z"
	if ok, _ := svc.recordAndFireSighting(ctx, "r1", replaced, specFor(replaced)); !ok {
		t.Error("a replaced file at the same path did NOT fire — the nightly-drop case is broken")
	}
}

// The run carries the arrival's provenance, so the script knows which file.
func TestFiredRunCarriesTheArrivalContext(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)

	if ok, err := svc.recordAndFireSighting(ctx, "r1", sightingIn{
		JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv",
		SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}, specFor(sightingIn{
		JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv",
		SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"})); !ok || err != nil {
		t.Fatalf("did not fire: %v %v", ok, err)
	}

	var envJSON, actor, kind string
	if err := svc.db.QueryRow(
		`SELECT COALESCE(env_json,''), triggered_by, trigger_kind FROM runs WHERE job_name='ingest'`).
		Scan(&envJSON, &actor, &kind); err != nil {
		t.Fatalf("read run: %v", err)
	}
	var env map[string]string
	_ = json.Unmarshal([]byte(envJSON), &env)
	if env["CRONOMICON_WATCH_PATH"] != "/srv/incoming/a.csv" {
		t.Errorf("CRONOMICON_WATCH_PATH = %q, want the arrival's path", env["CRONOMICON_WATCH_PATH"])
	}
	if env["CRONOMICON_WATCH_FILE"] != "a.csv" {
		t.Errorf("CRONOMICON_WATCH_FILE = %q, want the basename", env["CRONOMICON_WATCH_FILE"])
	}
	if actor != "watcher:r1" {
		t.Errorf("triggered_by = %q, want watcher:r1", actor)
	}
	if kind != "webhook" {
		t.Errorf("trigger_kind = %q, want webhook (the external-event kind)", kind)
	}
}

// A sighting for a job that no longer exists records WHY rather than vanishing.
func TestRefusedSightingRecordsItsReason(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})

	ok, err := svc.recordAndFireSighting(ctx, "r1", sightingIn{
		JobSource: "git", JobName: "ghost", Path: "/srv/incoming/a.csv",
		SizeBytes: 1, MTime: "2026-08-11T00:00:00Z"}, specFor(sightingIn{
		JobSource: "git", JobName: "ghost", Path: "/srv/incoming/a.csv",
		SizeBytes: 1, MTime: "2026-08-11T00:00:00Z"}))
	if ok || err != nil {
		t.Fatalf("a sighting for a missing job fired (%v, %v)", ok, err)
	}
	var reason sql.NullString
	if err := svc.db.QueryRow(
		`SELECT refused_reason FROM file_watch_sightings WHERE job_name='ghost'`).Scan(&reason); err != nil {
		t.Fatalf("no sighting row recorded the refusal: %v", err)
	}
	if !reason.Valid || reason.String == "" {
		t.Error("the refusal has no reason — 'the file landed and nothing happened' would be unanswerable")
	}
}

// ─── The gates every other trigger path already applied (PF) ─────────────────
//
// The arrival path is the FOURTH producer of runs, and it originally skipped
// two gates the cron fire, the manual run and the reaction all apply for
// themselves. Nothing below Enqueue consults paused_jobs, and the fleet-wide cap
// is a fire-time gate with no claim-time backstop — so a gate missing here is
// not a cosmetic inconsistency: a paused job ran whenever a file landed, and a
// burst of arrivals walked the fleet straight past the operator's cap.

// pauseJob records the operator pause scheduler.IsPaused reads.
func pauseJob(t *testing.T, svc *Service, source, name string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		VALUES (?, 'job', ?, 'operator', '2026-08-11T00:00:00Z')`, source, name); err != nil {
		t.Fatalf("pause %s: %v", name, err)
	}
}

// setGlobalMaxConcurrent writes the fleet-wide cap scheduler.AtCapacity reads.
// The default is 5, so a test that wants the gate to trip must lower it rather
// than seed five holders.
func setGlobalMaxConcurrent(t *testing.T, svc *Service, n string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO settings (key, value) VALUES ('maxConcurrent', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, n); err != nil {
		t.Fatalf("set maxConcurrent: %v", err)
	}
}

// seedActiveRun occupies a fleet slot so the cap gate trips.
func seedActiveRun(t *testing.T, svc *Service, id, jobName string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind, created_at)
		VALUES (?, ?, 'git', 'bash', 'running', 'scheduler', 'scheduled', '2026-08-11T00:00:00Z')`,
		id, jobName); err != nil {
		t.Fatalf("seed active run: %v", err)
	}
}

// sightingReason returns the recorded refusal for an arrival, or "" if the row
// says nothing — the difference between an answerable and an unanswerable "the
// file landed and nothing happened".
func sightingReason(t *testing.T, svc *Service, path string) string {
	t.Helper()
	var reason sql.NullString
	if err := svc.db.QueryRow(
		`SELECT refused_reason FROM file_watch_sightings WHERE path = ?`, path).Scan(&reason); err != nil {
		t.Fatalf("no sighting row recorded for %s: %v", path, err)
	}
	return reason.String
}

func runCountFor(t *testing.T, svc *Service, jobName string) int {
	t.Helper()
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name = ?`, jobName).Scan(&n); err != nil {
		t.Fatalf("count runs for %s: %v", jobName, err)
	}
	return n
}

// A pause is an operator saying "not now". An arrival must honour it exactly as
// a cron fire does — and the sighting is still CLAIMED, so the file does not
// re-fire the moment the job resumes.
func TestPausedJobDoesNotFireOnArrival(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)
	pauseJob(t, svc, "git", "ingest")

	sg := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv",
		SizeBytes: 1024, MTime: "2026-08-11T00:00:00Z"}
	ok, err := svc.recordAndFireSighting(ctx, "r1", sg, specFor(sg))
	if err != nil {
		t.Fatalf("sighting errored: %v", err)
	}
	if ok {
		t.Error("a file arrival ran a PAUSED job — the pause is not a pause")
	}
	if n := runCountFor(t, svc, "ingest"); n != 0 {
		t.Errorf("enqueued %d runs for a paused job, want 0", n)
	}
	// FX-C: the refusal text is now the SHARED gate vocabulary, the same sentence
	// History records for a cron fire the pause stopped. One phrasing across every
	// producer is the point — an operator comparing a sighting to a run should not
	// have to work out that two different sentences mean the same thing.
	if got := sightingReason(t, svc, sg.Path); got != "Skipped: the job is paused" {
		t.Errorf("refused_reason = %q, want %q", got, "Skipped: the job is paused")
	}

	// Resuming must not retroactively fire the file that landed while paused: the
	// sighting was claimed by the UNIQUE index before the gate refused it.
	if _, err := svc.db.Exec(`DELETE FROM paused_jobs WHERE name='ingest'`); err != nil {
		t.Fatal(err)
	}
	if again, err := svc.recordAndFireSighting(ctx, "r1", sg, specFor(sg)); again || err != nil {
		t.Errorf("the paused-through arrival fired on resume (%v, %v) — a stale file would import itself", again, err)
	}
}

// The cap is fire-time-only, so an arrival that ignores it escapes it entirely.
// Both directions are pinned: at the cap nothing runs, below it the arrival
// still does.
func TestArrivalHonoursTheGlobalConcurrencyCap(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)
	setGlobalMaxConcurrent(t, svc, "1")
	seedActiveRun(t, svc, "run-holding-the-slot", "other")

	capped := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/first.csv",
		SizeBytes: 100, MTime: "2026-08-11T00:00:00Z"}
	ok, err := svc.recordAndFireSighting(ctx, "r1", capped, specFor(capped))
	if err != nil {
		t.Fatalf("sighting errored: %v", err)
	}
	if ok {
		t.Error("an arrival started a run with the fleet already at its cap")
	}
	if n := runCountFor(t, svc, "ingest"); n != 0 {
		t.Errorf("enqueued %d runs over the cap, want 0", n)
	}
	if got := sightingReason(t, svc, capped.Path); got != "Skipped: the global concurrency cap was reached" {
		t.Errorf("refused_reason = %q, want %q", got, "Skipped: the global concurrency cap was reached")
	}

	// The slot frees. The gate must not have become a permanent stop.
	if _, err := svc.db.Exec(`UPDATE runs SET status='success' WHERE id='run-holding-the-slot'`); err != nil {
		t.Fatal(err)
	}
	under := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/second.csv",
		SizeBytes: 100, MTime: "2026-08-11T00:00:00Z"}
	if ok, err := svc.recordAndFireSighting(ctx, "r1", under, specFor(under)); !ok || err != nil {
		t.Fatalf("an arrival below the cap did NOT fire (%v, %v)", ok, err)
	}
	if n := runCountFor(t, svc, "ingest"); n != 1 {
		t.Errorf("enqueued %d runs below the cap, want 1", n)
	}
	if got := sightingReason(t, svc, under.Path); got != "" {
		t.Errorf("a fired arrival carries refused_reason %q, want none", got)
	}
}

// The control: an ordinary arrival — job un-paused, fleet under its cap, with
// both gates present and actually evaluated — still fires. Without this, a gate
// that refused everything would look correct to the two tests above.
func TestUnpausedUnderCapArrivalStillFires(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)
	setGlobalMaxConcurrent(t, svc, "2")
	seedActiveRun(t, svc, "one-of-two", "other") // occupied, but not full

	// A pause on a DIFFERENT job must not be read as this job's pause.
	seedWatchJob(t, svc, "git", "other-watcher", "", `[{"path":"/srv/other/*.csv"}]`)
	pauseJob(t, svc, "git", "other-watcher")

	ok, err := svc.recordAndFireSighting(ctx, "r1", sightingIn{
		JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv",
		SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}, specFor(sightingIn{
		JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv",
		SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}))
	if !ok || err != nil {
		t.Fatalf("an ordinary arrival did not fire (%v, %v) — the gates are over-applied", ok, err)
	}
	if n := runCountFor(t, svc, "ingest"); n != 1 {
		t.Errorf("enqueued %d runs for an ordinary arrival, want 1", n)
	}
}

// An agent must not be able to widen its own remit by reporting a path nobody
// asked it to watch.
func TestSightingMustMatchADistributedWatch(t *testing.T) {
	specs := []runnerproto.WatchSpec{
		{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/*.csv"},
	}
	if _, ok := matchDistributedWatch(sightingIn{
		JobSource: "git", JobName: "ingest", Path: "/etc/shadow"}, specs); ok {
		t.Error("a sighting outside every distributed glob was accepted")
	}
	if _, ok := matchDistributedWatch(sightingIn{
		JobSource: "git", JobName: "other", Path: "/srv/incoming/a.csv"}, specs); ok {
		t.Error("a sighting attributed to a different job was accepted")
	}
	if _, ok := matchDistributedWatch(sightingIn{
		JobSource: "git", JobName: "ingest", Path: "/srv/incoming/a.csv"}, specs); !ok {
		t.Error("a legitimate sighting was refused")
	}
}

// FX-C — a fleet-wide change freeze stops an arrival.
//
// This producer applied pause and the cap (added in v1.0.0 after the review
// found both missing) and still consulted no calendar at all. An external event
// is no less subject to a freeze than a clock is — arguably more, since nobody
// chose its timing.
func TestGlobalFreezeStopsAnArrival(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	seedWatchJob(t, svc, "git", "ingest", "", `[{"path":"/srv/incoming/*.csv"}]`)

	// Control first: with no freeze the arrival runs. Without this, a broken
	// fixture below would look like a working gate.
	first := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/before.csv",
		SizeBytes: 1024, MTime: "2026-08-11T00:00:00Z"}
	if ok, err := svc.recordAndFireSighting(ctx, "r1", first, specFor(first)); err != nil || !ok {
		t.Fatalf("control: arrival with no freeze did not run (ok=%v err=%v)", ok, err)
	}

	// The freeze covers today in the zone the gate computes in. With no timezone
	// row stored, that zone is UTC — which is what the gate falls back to, and
	// what the app itself defaults to when the key is absent.
	day := time.Now().UTC().Format("2006-01-02")
	if _, err := svc.db.Exec(
		`INSERT INTO calendars (name, source, global, created_at)
		 VALUES ('change-freeze','cronomicon',1,'2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("seed calendar: %v", err)
	}
	if _, err := svc.db.Exec(
		`INSERT INTO calendar_days (calendar_source, calendar_name, day, label)
		 VALUES ('cronomicon','change-freeze',?,'Change freeze')`, day); err != nil {
		t.Fatalf("seed calendar day: %v", err)
	}

	frozen := sightingIn{JobSource: "git", JobName: "ingest", Path: "/srv/incoming/during.csv",
		SizeBytes: 1024, MTime: "2026-08-11T00:00:00Z"}
	ok, err := svc.recordAndFireSighting(ctx, "r1", frozen, specFor(frozen))
	if err != nil {
		t.Fatalf("sighting errored: %v", err)
	}
	if ok {
		t.Error("a file arrival ran during a fleet-wide change freeze")
	}
	if got := sightingReason(t, svc, frozen.Path); !strings.Contains(got, "calendar") {
		t.Errorf("refused_reason = %q, want it to name the calendar that stopped it", got)
	}
}

// specFor builds the watch spec the server would have distributed for a
// sighting, so tests that predate R2-4's "fire from the matched spec, not the
// echo" change keep exercising the same path.
func specFor(sg sightingIn) runnerproto.WatchSpec {
	return runnerproto.WatchSpec{
		JobSource: sg.JobSource, JobName: sg.JobName, JobUID: sg.JobUID, Path: sg.Path,
	}
}

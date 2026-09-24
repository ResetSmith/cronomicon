package execspec

import (
	"context"
	"strings"
	"testing"
)

// RT-1 — the runner-pin arm of the stuck-run diagnosis
// (the runner-targeting plan).
//
// A pinned run that nothing can claim has to say WHICH of two very different
// things went wrong (RT-G5). "Nothing carries this tag" is a typo or an untagged
// runner — the operator goes and edits tags. "Tagged runners exist but none is
// eligible" is an offline/wrong-agency/wrong-type problem — the operator goes and
// looks at those runners. One sentence for both would send half of them to the
// wrong screen, which is the exact failure RA-20(b) set out to fix.

// seedPinnedRun mirrors unclaimFixture.seedRun with a runner_tag pin.
func (f *unclaimFixture) seedPinnedRun(id, runType, agenciesJSON, requiresJSON, pin string) {
	f.exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind,
	                         agencies_json, requires_json, runner_tag, created_at)
	        VALUES(?, 'j', 'git', ?, 'queued', 'seed', 'manual', ?, ?, ?, 't')`,
		id, runType, agenciesJSON, requiresJSON, pin)
}

func (f *unclaimFixture) tag(runnerID, tag string) {
	f.exec(`INSERT INTO runner_tags(runner_id,tag) VALUES(?,?)`, runnerID, tag)
	f.exec(`UPDATE runners SET tags=json_insert(COALESCE(NULLIF(tags,''),'[]'), '$[#]', ?) WHERE id=?`, tag, runnerID)
}

func TestUnclaimableReasonDistinguishesPinCauses(t *testing.T) {
	f := newUnclaimFixture(t)

	// An online, in-agency, capable, injection-flagged runner — so the ONLY thing
	// that can hold these runs back is the pin.
	f.runner("r1", "online", `["ansible"]`, "a1", 1)

	t.Run("no runner carries the tag", func(t *testing.T) {
		f.seedPinnedRun("run-pin-1", "ansible", `["alpha"]`, `[]`, "vlan-dmz")
		got := f.reason(t, "run-pin-1")
		if !strings.Contains(got, "vlan-dmz") {
			t.Errorf("reason does not name the tag: %q", got)
		}
		if !strings.Contains(got, "no runner in the fleet carries that tag") {
			t.Errorf("reason = %q, want the fleet-wide 'nothing carries it' wording", got)
		}
	})

	t.Run("tagged runners exist but none eligible", func(t *testing.T) {
		// Same tag, but the only runner carrying it is offline. The fleet count
		// deliberately ignores status, so the wording must flip.
		f.runner("r-off", "offline", `["ansible"]`, "a1", 1)
		f.tag("r-off", "vlan-core")
		f.seedPinnedRun("run-pin-2", "ansible", `["alpha"]`, `[]`, "vlan-core")
		got := f.reason(t, "run-pin-2")
		if !strings.Contains(got, "none of them is online") {
			t.Errorf("reason = %q, want the 'exists but not eligible' wording", got)
		}
	})

	t.Run("pin satisfied yields no pin complaint", func(t *testing.T) {
		f.tag("r1", "vlan-ok")
		f.seedPinnedRun("run-pin-3", "ansible", `["alpha"]`, `[]`, "vlan-ok")
		if got := f.reason(t, "run-pin-3"); got != "" {
			t.Errorf("reason = %q, want \"\" (the run is claimable)", got)
		}
	})
}

// TestUnclaimableReasonChecksPinAfterAgency pins the GATE ORDER. A run that is
// both in the wrong agency and pinned to an unknown tag must report the agency
// problem, because that is the one an operator checks first — and because the
// pin arm builds its clause on top of the agency clause, an inversion here would
// mean the two are no longer composed.
func TestUnclaimableReasonChecksPinAfterAgency(t *testing.T) {
	f := newUnclaimFixture(t)
	f.runner("r1", "online", `["ansible"]`, "", 1) // online, capable, NO agency

	f.seedPinnedRun("run-order", "ansible", `["alpha"]`, `[]`, "vlan-nope")
	got := f.reason(t, "run-order")
	if !strings.Contains(got, "belongs to alpha") {
		t.Errorf("reason = %q, want the agency complaint to win over the pin complaint", got)
	}
}

// TestEligibleOnlineRunnerHonoursPin is the claim-query parity check. A probe
// that disagreed with claimRun would tell an operator a run is fine while it sits
// queued forever — the two must move together.
func TestEligibleOnlineRunnerHonoursPin(t *testing.T) {
	f := newUnclaimFixture(t)
	ctx := context.Background()
	f.runner("r1", "online", `["ansible"]`, "a1", 1)

	f.seedPinnedRun("run-e1", "ansible", `["alpha"]`, `[]`, "vlan-dmz")
	ok, _, err := EligibleOnlineRunnerForRun(ctx, f.pool, "run-e1")
	if err != nil {
		t.Fatalf("EligibleOnlineRunnerForRun: %v", err)
	}
	if ok {
		t.Error("pinned to a tag no runner carries: want not eligible")
	}

	f.tag("r1", "vlan-dmz")
	ok, _, err = EligibleOnlineRunnerForRun(ctx, f.pool, "run-e1")
	if err != nil {
		t.Fatalf("EligibleOnlineRunnerForRun: %v", err)
	}
	if !ok {
		t.Error("tag now carried by an eligible runner: want eligible")
	}

	// The unpinned no-regression case, asserted here too: this probe is what the
	// run-detail status line calls, so a regression would surface as every queued
	// run suddenly claiming to be unclaimable.
	f.seedRun("run-e2", "ansible", `["alpha"]`, `[]`)
	ok, _, err = EligibleOnlineRunnerForRun(ctx, f.pool, "run-e2")
	if err != nil {
		t.Fatalf("EligibleOnlineRunnerForRun: %v", err)
	}
	if !ok {
		t.Error("unpinned run: want eligible (RT-1 must be opt-in)")
	}
}

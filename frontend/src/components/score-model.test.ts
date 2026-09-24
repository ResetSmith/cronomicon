import { describe, expect, it } from "vitest";
import {
  HOURS_AHEAD,
  HOURS_BACK,
  NEAREST_TOLERANCE,
  STEM_MAX,
  STEM_MIN,
  CLUSTER_MS,
  executorLabel,
  frac,
  groupMarks,
  inWindow,
  nearestGroup,
  scoreWindow,
  severity,
  triggerLabel,
  stemHeight,
  ticks,
  worstStatus,
  type Mark,
} from "./score-model";

// The Score's maths (Phase D). The headline case is VU-21: a failed run
// coinciding with a successful one used to be painted over and vanish, on a
// component whose entire job is surfacing failures. That is a property of
// groupMarks, so it is tested here directly rather than through a rendered SVG.

const NOW = Date.parse("2026-07-27T19:05:00Z");
const W = scoreWindow(NOW);
const mark = (over: Partial<Mark> & Pick<Mark, "at">): Mark => ({ tense: "past", jobName: "job", ...over });

describe("window", () => {
  it("is a fixed 24h back / 12h ahead — no range picker (VU-Q8)", () => {
    expect(HOURS_BACK).toBe(24);
    expect(HOURS_AHEAD).toBe(12);
    expect(W.now - W.start).toBe(24 * 3600_000);
    expect(W.end - W.now).toBe(12 * 3600_000);
  });

  it("places now two-thirds along, and the edges at 0 and 1", () => {
    expect(frac(W, W.start)).toBe(0);
    expect(frac(W, W.end)).toBe(1);
    expect(frac(W, NOW)).toBeCloseTo(24 / 36, 6);
  });

  it("does not clamp out-of-window instants — callers filter", () => {
    // A silent clamp would stack off-window marks on the edge as though they
    // were real events there.
    expect(frac(W, W.start - 3600_000)).toBeLessThan(0);
    expect(inWindow(W, W.start - 1)).toBe(false);
    expect(inWindow(W, W.end + 1)).toBe(false);
    expect(inWindow(W, NOW)).toBe(true);
  });
});

describe("coincident runs (VU-21) — a failure can never be painted over", () => {
  it("collapses an exact-timestamp collision into ONE group carrying a count", () => {
    const groups = groupMarks([
      mark({ at: NOW - 3600_000, jobName: "python-metrics-export", status: "success" }),
      mark({ at: NOW - 3600_000, jobName: "win-update-check", status: "danger" }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].members).toHaveLength(2);
  });

  it("colours the group by its WORST member — the regression this guards", () => {
    // `0 6 * * *` fires python-metrics-export and win-update-check together. If
    // the successful one paints last, the failure disappears.
    const groups = groupMarks([
      mark({ at: NOW - 3600_000, jobName: "ok-job", status: "success" }),
      mark({ at: NOW - 3600_000, jobName: "bad-job", status: "danger" }),
    ]);
    expect(groups[0].status).toBe("danger");
    expect(groups[0].members).toHaveLength(2);

    // …and the same set in the opposite array order must give the same answer.
    const reversed = groupMarks([
      mark({ at: NOW - 3600_000, jobName: "bad-job", status: "danger" }),
      mark({ at: NOW - 3600_000, jobName: "ok-job", status: "success" }),
    ]);
    expect(reversed[0].status).toBe("danger");
  });

  it("orders members worst-first for the chord stack, without moving the anchor", () => {
    // The renderer draws members bottom-up from the staff line, so worst-first
    // puts the failure ON the line — but the sort must not move the group's
    // time: the anchor stays the EARLIEST member, or the group's identity key
    // (`${tense}@${at}`) would jump and drop the hover/keyboard selection.
    const groups = groupMarks([
      mark({ at: NOW - 3600_000, jobName: "early-ok", status: "success" }),
      mark({ at: NOW - 3600_000 + 60_000, jobName: "late-bad", status: "danger" }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].at).toBe(NOW - 3600_000);
    expect(groups[0].members[0].status).toBe("danger");
  });

  it("ranks warn above success and failure above warn", () => {
    expect(severity("danger")).toBeGreaterThan(severity("warning"));
    expect(severity("warning")).toBeGreaterThan(severity("success"));
    expect(worstStatus([mark({ at: 0, status: "success" }), mark({ at: 0, status: "warning" })])).toBe("warning");
    expect(worstStatus([mark({ at: 0, status: "warning" }), mark({ at: 0, status: "danger" })])).toBe("danger");
    // Wire aliases mean the same thing as their canonical form.
    expect(severity("failed")).toBe(severity("danger"));
    expect(severity("ok")).toBe(severity("success"));
  });

  it("never merges tenses — a run and a projection stay distinct at one instant", () => {
    const groups = groupMarks([
      mark({ at: NOW, status: "success", tense: "past" }),
      mark({ at: NOW, tense: "future" }),
    ]);
    expect(groups).toHaveLength(2);
    expect(new Set(groups.map((g) => g.tense))).toEqual(new Set(["past", "future"]));
  });

  it("does NOT dodge horizontally — every member keeps its true instant", () => {
    const at = NOW - 7200_000;
    const groups = groupMarks([mark({ at, status: "success" }), mark({ at, status: "danger" })]);
    expect(groups[0].at).toBe(at);
  });

  it("sizes the stem from the LONGEST member, not the first or the shortest", () => {
    const at = NOW - 1800_000;
    const groups = groupMarks([
      mark({ at, status: "success", durationMs: 14_000 }),
      mark({ at, status: "success", durationMs: 840_000 }),
    ]);
    expect(groups[0].durationMs).toBe(840_000);
  });

  // FX-5 re-based this fixture. It used to use instants a SECOND apart, which is
  // 1/100th of a pixel on this window — "distinct" in the data and one smudge on
  // screen. Distinctness that matters here is distinctness the operator can see.
  it("leaves visibly distinct instants alone", () => {
    const groups = groupMarks([mark({ at: NOW - 2 * 3600_000 }), mark({ at: NOW - 3600_000 })]);
    expect(groups).toHaveLength(2);
    expect(groups[0].at).toBeLessThan(groups[1].at); // and returns them in time order
  });
});

// FX-5 — the pile-up. Exact-instant grouping merged `0 6 * * *` firing two jobs
// (same millisecond) and nothing else, so runs launched seconds or minutes apart
// drew separate noteheads at indistinguishable x and smeared into each other.
// Clustering merges by RENDERED proximity instead, which is the thing the
// operator actually experiences.
describe("clustering by rendered proximity (FX-5)", () => {
  const AT = NOW - 6 * 3600_000;

  it("merges runs seconds apart — the case exact-instant grouping walked straight past", () => {
    const groups = groupMarks([
      mark({ at: AT, jobName: "one", status: "success" }),
      mark({ at: AT + 4_000, jobName: "two", status: "success" }),
      mark({ at: AT + 11_000, jobName: "three", status: "success" }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].members).toHaveLength(3);
  });

  it("keeps marks further apart than a notehead as separate marks", () => {
    const groups = groupMarks([mark({ at: AT }), mark({ at: AT + CLUSTER_MS + 1 })]);
    expect(groups).toHaveLength(2);
  });

  it("draws the cluster no further from a member than the notehead it is drawn with", () => {
    // The bound that lets this coexist with the recorded "no jitter" decision:
    // every member is inside one cluster-width of where the mark is drawn, so the
    // time distortion is smaller than the mark itself.
    const marks = [mark({ at: AT }), mark({ at: AT + CLUSTER_MS })];
    const [g] = groupMarks(marks);
    for (const m of g.members) expect(Math.abs(m.at - g.at)).toBeLessThanOrEqual(CLUSTER_MS);
  });

  // Without an anchor the sweep would chain: A merges B, B merges C, and a dense
  // enough sequence becomes one cluster spanning hours, breaking the bound above.
  it("does not chain — a long train of marks breaks into bounded clusters", () => {
    const step = Math.round(CLUSTER_MS * 0.75);
    const marks = Array.from({ length: 6 }, (_, i) => mark({ at: AT + i * step }));
    const groups = groupMarks(marks);
    expect(groups.length).toBeGreaterThan(1);
    for (const g of groups) {
      const span = Math.max(...g.members.map((m) => m.at)) - Math.min(...g.members.map((m) => m.at));
      expect(span).toBeLessThanOrEqual(CLUSTER_MS);
    }
  });

  it("anchors on the earliest member, so the identity survives a poll adding a later one", () => {
    // The plot holds hover and keyboard selection by `${tense}@${at}`. A mean
    // would shift on every arrival and drop the operator's selection.
    const first = groupMarks([mark({ at: AT }), mark({ at: AT + 5_000 })]);
    const afterPoll = groupMarks([mark({ at: AT }), mark({ at: AT + 5_000 }), mark({ at: AT + 9_000 })]);
    expect(first[0].at).toBe(AT);
    expect(afterPoll[0].at).toBe(AT);
  });

  it("still refuses to merge across tenses, however close in time", () => {
    const groups = groupMarks([
      mark({ at: AT, status: "success", tense: "past" }),
      mark({ at: AT + 1_000, tense: "future" }),
    ]);
    expect(groups).toHaveLength(2);
    expect(new Set(groups.map((g) => g.tense))).toEqual(new Set(["past", "future"]));
  });

  it("carries the worst status of the cluster, not of its anchor (VU-21 across a cluster)", () => {
    // The property that made grouping worth having in the first place, now
    // holding over marks that merely LOOK coincident rather than share a
    // millisecond: a failure seconds after a success cannot be painted over.
    const groups = groupMarks([
      mark({ at: AT, jobName: "ok", status: "success" }),
      mark({ at: AT + 30_000, jobName: "bad", status: "danger" }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].status).toBe("danger");
    expect(groups[0].durationMs).toBe(null);
  });

  it("sizes the cluster stem from its longest member", () => {
    const groups = groupMarks([
      mark({ at: AT, durationMs: 14_000 }),
      mark({ at: AT + 20_000, durationMs: 840_000 }),
    ]);
    expect(groups[0].durationMs).toBe(840_000);
  });

  // The escape hatch, and what every pre-FX-5 caller's behaviour was.
  it("falls back to exact-instant grouping at clusterMs 0", () => {
    const groups = groupMarks([mark({ at: AT }), mark({ at: AT + 1 })], 0);
    expect(groups).toHaveLength(2);
  });

  it("is derived from the geometry — about one notehead of a 36-hour window", () => {
    // Pinned so the constant cannot drift into "an arbitrary number of minutes".
    expect(CLUSTER_MS).toBe(Math.round(((HOURS_BACK + HOURS_AHEAD) * 3600_000 * 12) / 1400));
    expect(CLUSTER_MS / 60_000).toBeCloseTo(18.5, 1);
  });
});

describe("stems", () => {
  it("uses a log scale — linear would flatten every real duration into one height", () => {
    // Real durations cluster at 2–3 min while disk-usage-audit is ~14s. On a
    // linear scale those are indistinguishable.
    const short = stemHeight(14_000);
    const typical = stemHeight(150_000);
    const long = stemHeight(3600_000);
    expect(short).toBeLessThan(typical);
    expect(typical).toBeLessThan(long);
    // The 14s→2.5min step must be visible, not sub-pixel.
    expect(typical - short).toBeGreaterThan(3);
  });

  it("gives a scheduled (duration-less) mark the floor, and clamps the extremes", () => {
    expect(stemHeight(null)).toBe(stemHeight(undefined));
    expect(stemHeight(0)).toBe(stemHeight(null));
    expect(stemHeight(1)).toBeGreaterThanOrEqual(stemHeight(null));
    // Against the constant, not a literal: the ceiling moved when the plot grew
    // (D-9) and a hardcoded number made this a false failure rather than a real one.
    expect(stemHeight(99 * 3600_000)).toBeLessThanOrEqual(STEM_MAX);
    expect(stemHeight(null)).toBe(STEM_MIN);
  });
});

describe("nearest-mark resolution (D-3)", () => {
  // FX-5 re-based these too: `a` and `b` used to sit a minute apart, which is now
  // ONE cluster rather than two neighbours. Half an hour apart is the case the
  // resolution still has to serve — adjacent, nearly touching, but genuinely two
  // marks. (The one-minute case did not lose its cover: it is a cluster test now.)
  const groups = groupMarks([
    mark({ at: NOW - 3600_000, jobName: "a" }),
    mark({ at: NOW - 1800_000, jobName: "b" }),
    mark({ at: NOW + 3600_000, jobName: "c", tense: "future" }),
  ]);

  it("reaches a mark that a neighbour's notehead would cover", () => {
    // The prototype's failure mode: at notehead width these two nearly touch, and
    // per-mark hit boxes made the earlier one unhoverable.
    expect(nearestGroup(groups, W, NOW - 3600_000)?.members[0].jobName).toBe("a");
    expect(nearestGroup(groups, W, NOW - 1800_000)?.members[0].jobName).toBe("b");
  });

  it("snaps to whichever mark is closer in time", () => {
    expect(nearestGroup(groups, W, NOW - 3500_000)?.members[0].jobName).toBe("a");
    expect(nearestGroup(groups, W, NOW - 1900_000)?.members[0].jobName).toBe("b");
  });

  it("returns nothing past the tolerance rather than snapping across the plot", () => {
    const farAway = NOW - 12 * 3600_000;
    expect(nearestGroup(groups, W, farAway)).toBeNull();
    // Just inside the tolerance still resolves.
    const justInside = NOW - 3600_000 + NEAREST_TOLERANCE * (W.end - W.start) * 0.9;
    expect(nearestGroup(groups, W, justInside)).not.toBeNull();
  });

  it("returns nothing on an empty score", () => {
    expect(nearestGroup([], W, NOW)).toBeNull();
  });
});

describe("axis", () => {
  // Hour-of-day in the APPLICATION timezone. The fixture pins UTC; the point of
  // the injection is that the component passes the app-zone accessor, so the
  // 6-hour bar lines and the midnight rule land where the scheduler fires.
  const utcHour = (at: number) => new Date(at).getUTCHours();

  it("emits an hour tick across the whole window", () => {
    const t = ticks(W, utcHour);
    expect(t.length).toBeGreaterThanOrEqual(36);
    expect(t.every((x) => x.at >= W.start && x.at <= W.end)).toBe(true);
  });

  it("marks every sixth hour major and midnight as the day boundary", () => {
    const t = ticks(W, utcHour);
    expect(t.filter((x) => x.major).every((x) => utcHour(x.at) % 6 === 0)).toBe(true);
    const boundaries = t.filter((x) => x.dayBoundary);
    // A 36h window starting at 19:05 crosses midnight twice — the component has
    // to cope with more than one day rule, which is why this asserts the count
    // rather than assuming a single boundary.
    expect(boundaries).toHaveLength(2);
    expect(boundaries.every((b) => utcHour(b.at) === 0)).toBe(true);
    expect(boundaries.every((b) => b.major)).toBe(true); // midnight is also a bar line
  });

  it("follows the injected zone rather than the browser's", () => {
    // Same window, zone shifted by 5h30 — the boundary must move with it.
    const shifted = (at: number) => new Date(at + 5.5 * 3600_000).getUTCHours();
    const a = ticks(W, utcHour).find((x) => x.dayBoundary)!.at;
    const b = ticks(W, shifted).find((x) => x.dayBoundary)!.at;
    expect(a).not.toBe(b);
  });
});

// SR-1 — the readout's trigger and executor phrases.
describe("triggerLabel / executorLabel", () => {
  it("names the person for a manual run and the schedule for a scheduled one", () => {
    expect(triggerLabel({ triggerKind: "manual", triggeredBy: "ops@example.com" })).toBe("by ops@example.com");
    expect(triggerLabel({ triggerKind: "scheduled", scheduleName: "nightly" })).toBe("schedule nightly");
    expect(triggerLabel({ triggerKind: "scheduled" })).toBe("scheduled");
  });
  it("reads a person with no kind as manual, and never invents a label for an unknown kind", () => {
    expect(triggerLabel({ triggeredBy: "ops@example.com" })).toBe("by ops@example.com");
    expect(triggerLabel({ triggerKind: "promotion-adhoc" })).toBe("promotion-adhoc");
    expect(triggerLabel({})).toBe("");
  });
  it("names the runner when it is known and only the executor when it is not", () => {
    expect(executorLabel({ executor: "runner", runnerName: "nv-dmz-01" })).toBe("Runner · nv-dmz-01");
    expect(executorLabel({ executor: "runner" })).toBe("Runner");
    expect(executorLabel({ executor: "ssh" })).toBe("SSH");
    expect(executorLabel({})).toBe("");
  });
});

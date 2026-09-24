// @vitest-environment jsdom
// Nothing here renders — jsdom is needed only because importing the view pulls
// in theme.ts, which reads window.location at module load to resolve the saved
// theme. The tests below are pure-function tests.
import { afterEach, describe, expect, it } from "vitest";
import { activityKey, fmtSpan, groupConsecutive, type Entry } from "./Activity";
import { setAppZone } from "../utils/datetime";

// Activity's repeat collapsing (E-1 / VU-13). The grouping is a pure function so
// it is tested directly rather than through rendered rows — the same split
// score-model.ts uses. The defect this exists to prevent is a feed that lies
// about order: collapsing non-adjacent events would reorder the timeline, so
// that case is the headline test below.

const at = (iso: string): Entry => ({ kind: "sync", outcome: "failure", summary: "CRONOMICON_GITLAB_BASE_URL not configured", actor: "system", at: iso });

afterEach(() => setAppZone(undefined));

describe("activityKey", () => {
  it("folds on what the row renders, not on id or timestamp", () => {
    expect(activityKey(at("2026-07-27T14:02:00Z"))).toBe(activityKey({ ...at("2026-07-27T14:37:00Z"), id: 99 }));
  });

  it("separates entries differing in any rendered field", () => {
    const base = at("2026-07-27T14:02:00Z");
    const k = activityKey(base);
    expect(activityKey({ ...base, kind: "run" })).not.toBe(k);
    expect(activityKey({ ...base, outcome: "success" })).not.toBe(k);
    expect(activityKey({ ...base, actor: "someone@example.com" })).not.toBe(k);
    expect(activityKey({ ...base, summary: "something else" })).not.toBe(k);
    expect(activityKey({ ...base, jobName: "nightly-backup" })).not.toBe(k);
    expect(activityKey({ ...base, workflowName: "release" })).not.toBe(k);
  });

  it("does not conflate a field boundary — jobName 'a' + summary 'b' is not jobName 'ab'", () => {
    expect(activityKey({ kind: "k", jobName: "a", summary: "b" })).not.toBe(activityKey({ kind: "k", jobName: "ab", summary: "" }));
  });
});

describe("groupConsecutive", () => {
  it("collapses the nine-identical-rows default view into one row of nine", () => {
    const nine = Array.from({ length: 9 }, (_, i) => ({ ...at(`2026-07-27T14:0${i}:00Z`), id: i }));
    const groups = groupConsecutive(nine);
    expect(groups).toHaveLength(1);
    expect(groups[0].members).toHaveLength(9);
    expect(groups[0].head.id).toBe(0);
  });

  it("does NOT group across an intervening event — A A B A A is three rows", () => {
    const a = (n: number) => ({ ...at(`2026-07-27T14:0${n}:00Z`), id: n });
    const b = { ...at("2026-07-27T14:02:00Z"), kind: "run", summary: "ran", id: 2 };
    const groups = groupConsecutive([a(0), a(1), b, a(3), a(4)]);
    expect(groups.map((g) => g.members.length)).toEqual([2, 1, 2]);
    expect(groups[1].head.kind).toBe("run");
  });

  it("leaves a lone event as a group of one", () => {
    const groups = groupConsecutive([at("2026-07-27T14:02:00Z"), { ...at("2026-07-27T14:03:00Z"), kind: "run" }]);
    expect(groups.map((g) => g.members.length)).toEqual([1, 1]);
  });

  it("returns nothing for an empty feed", () => {
    expect(groupConsecutive([])).toEqual([]);
  });

  it("reports the span by instant, not by array position (feed arrives newest-first)", () => {
    const groups = groupConsecutive([at("2026-07-27T14:37:00Z"), at("2026-07-27T14:20:00Z"), at("2026-07-27T14:02:00Z")]);
    expect(groups[0].earliestAt).toBe("2026-07-27T14:02:00Z");
    expect(groups[0].latestAt).toBe("2026-07-27T14:37:00Z");
  });

  it("skips absent and unparseable timestamps rather than treating them as epoch 0", () => {
    const groups = groupConsecutive([{ ...at("2026-07-27T14:37:00Z"), at: undefined }, { ...at("2026-07-27T14:20:00Z"), at: "not-a-date" }, at("2026-07-27T14:02:00Z")]);
    expect(groups).toHaveLength(1);
    expect(groups[0].earliestAt).toBe("2026-07-27T14:02:00Z");
    expect(groups[0].latestAt).toBe("2026-07-27T14:02:00Z");
  });

  it("gives each group an id that survives a refetch prepending a new event", () => {
    const feed = [at("2026-07-27T14:37:00Z"), at("2026-07-27T14:02:00Z")];
    const before = groupConsecutive(feed)[0].id;
    const after = groupConsecutive([{ ...at("2026-07-27T15:00:00Z"), kind: "run" }, ...feed])[1].id;
    expect(after).toBe(before);
  });

  it("keeps two runs of the same content distinguishable when they are separated", () => {
    const other = { ...at("2026-07-27T14:10:00Z"), kind: "run" };
    const groups = groupConsecutive([at("2026-07-27T14:20:00Z"), other, at("2026-07-27T14:02:00Z")]);
    expect(groups[0].id).not.toBe(groups[2].id);
  });

  it("preserves member order as received", () => {
    const groups = groupConsecutive([{ ...at("2026-07-27T14:37:00Z"), id: 3 }, { ...at("2026-07-27T14:02:00Z"), id: 1 }]);
    expect(groups[0].members.map((m) => m.id)).toEqual([3, 1]);
  });
});

describe("fmtSpan", () => {
  it("drops the repeated date within one day: 'Jul 27, 2026, 14:02 – 14:37'", () => {
    setAppZone("UTC");
    expect(fmtSpan("2026-07-27T14:02:00Z", "2026-07-27T14:37:00Z")).toBe("Jul 27, 2026, 14:02 – 14:37");
  });

  it("keeps both dates across a day boundary", () => {
    setAppZone("UTC");
    expect(fmtSpan("2026-07-27T23:50:00Z", "2026-07-28T00:10:00Z")).toBe("Jul 27, 2026, 23:50 – Jul 28, 2026, 00:10");
  });

  it("renders in the APP zone, not the browser zone — the same instants land on different days", () => {
    setAppZone("UTC");
    const utc = fmtSpan("2026-07-27T23:50:00Z", "2026-07-28T00:10:00Z");
    setAppZone("America/New_York");
    const ny = fmtSpan("2026-07-27T23:50:00Z", "2026-07-28T00:10:00Z");
    expect(ny).not.toBe(utc);
    expect(ny).toBe("Jul 27, 2026, 19:50 – 20:10");
  });

  it("degrades to a single instant when the span has only one usable timestamp", () => {
    setAppZone("UTC");
    expect(fmtSpan(undefined, "2026-07-27T14:02:00Z")).toBe("Jul 27, 2026, 14:02");
    expect(fmtSpan("2026-07-27T14:02:00Z", "2026-07-27T14:02:00Z")).toBe("Jul 27, 2026, 14:02");
  });
});

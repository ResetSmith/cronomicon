// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { nextCronTimes, nextCronTimesDetailed, nextSpecTimes } from "./cron";
import { appZoneParts, fmtInAppZone, setAppZone } from "../../utils/datetime";

// expectedFires is the locale-proof reference: a brute-force minute-stepper
// over NUMERIC app-zone parts (appZoneParts), rendered through the same
// formatter the preview uses. The first version of these tests parsed the
// rendered strings with en-US regexes and failed on any non-English CI
// machine — fmtInAppZone renders in the MACHINE locale by design.
function expectedFires(
  matches: (p: { minute: number; hour: number; dow: number; dom: number; month: number }) => boolean,
  count: number,
  fromMs = Date.now() + 60000,
): string[] {
  const out: string[] = [];
  const cur = new Date(fromMs);
  cur.setSeconds(0, 0);
  for (let i = 0; i < 800000 && out.length < count; i++) {
    if (matches(appZoneParts(cur))) {
      out.push(
        fmtInAppZone(cur.toISOString(), { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }),
      );
    }
    cur.setTime(cur.getTime() + 60000);
  }
  return out;
}

// FX-F2 — the live cron preview matches only what it can honestly match, and
// it now matches all five fields. The old matcher destructured minute, hour and
// weekday and silently dropped day-of-month and month, so the SHIPPED preset
// "Monthly 1st 00:00" (0 0 1 * *) previewed as daily at 00:00. The server's
// projection was always right; the preview lied exactly where the operator was
// deciding.
//
// Zone pinned to +05:30 per the v1.0.0 #11 convention: it moves the minute as
// well as the hour, so a matcher comparing the wrong zone's wall clock cannot
// pass by coincidence.

afterEach(() => {
  setAppZone(null);
  vi.useRealTimers();
});

describe("nextCronTimes — all five fields (FX-F2)", () => {
  it("previews the Monthly preset monthly, not daily", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    const times = nextCronTimes("0 0 1 * *", 3);
    // Exact agreement with the numeric reference: midnight on the 1st, three
    // months running. The old matcher returned three consecutive DAYS.
    expect(times).toEqual(expectedFires((p) => p.minute === 0 && p.hour === 0 && p.dom === 1, 3));
  });

  it("matches a month restriction", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    const times = nextCronTimes("0 0 1 1 *", 1);
    expect(times).toEqual(
      expectedFires((p) => p.minute === 0 && p.hour === 0 && p.dom === 1 && p.month === 1, 1),
    );
  });

  it("applies the vixie OR rule when both dom and dow are restricted", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    // 1st of the month OR a Monday — mirroring robfig/cron v3, the library the
    // engine fires with.
    const times = nextCronTimes("0 0 1 * 1", 6);
    expect(times).toEqual(
      expectedFires((p) => p.minute === 0 && p.hour === 0 && (p.dom === 1 || p.dow === 1), 6),
    );
  });

  it("steps */N on day-of-month from 1, not 0", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    // */10 on dom means days 1, 11, 21, 31 (steps count from the field
    // minimum), not 10/20/30 — verified against robfig's parser, which expands
    // */N as min..max step N.
    const times = nextCronTimes("0 12 */10 * *", 4);
    expect(times).toEqual(
      expectedFires((p) => p.minute === 0 && p.hour === 12 && [1, 11, 21, 31].includes(p.dom), 4),
    );
  });

  it("still previews the simple daily shape", () => {
    setAppZone("Asia/Kolkata");
    const times = nextCronTimes("30 2 * * *", 2);
    expect(times).toEqual(expectedFires((p) => p.minute === 30 && p.hour === 2, 2));
  });

  it("treats */1 on dom as a star for the day rule, matching robfig", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    // robfig's parser RETAINS the star bit for step 1, so `0 0 */1 * 1` fires
    // Mondays only — a preview treating */1 as "restricted" would flip the day
    // rule to OR and show every day.
    const times = nextCronTimes("0 0 */1 * 1", 3);
    expect(times).toEqual(expectedFires((p) => p.minute === 0 && p.hour === 0 && p.dow === 1, 3));
  });

  it("does not skip a midnight fire across a spring-forward day (DST)", { timeout: 30000 }, () => {
    // Europe/London, two days before BST starts (2024-03-31, a 23-hour day
    // immediately preceding a matching 1st). A single full-length day-jump
    // lands at 01:00 on Apr 1 — past the 00:00 fire — and previews the Monthly
    // preset a month late. The two-stage jump cannot overshoot.
    setAppZone("Europe/London");
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2024-03-29T12:00:00Z"));
    const times = nextCronTimes("0 0 1 * *", 1);
    expect(times).toEqual(
      expectedFires((p) => p.minute === 0 && p.hour === 0 && p.dom === 1, 1, Date.now() + 60000),
    );
    // And concretely: the fire is April 1, not May 1.
    expect(times[0]).toBe(
      fmtInAppZone("2024-03-31T23:00:00Z", { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }),
    );
  });

  it("returns nothing for shapes it cannot honestly preview", () => {
    setAppZone("Asia/Kolkata");
    // Ranges/lists are unsupported by design — better no preview than a wrong one.
    expect(nextCronTimes("0 0 1-15 * *", 3)).toHaveLength(0);
    expect(nextCronTimes("0 0 * * 1,3", 3)).toHaveLength(0);
  });

  // FX2-E — sparse expressions. The 5000-iteration budget could not enumerate
  // three fires of a leap-day cron (~2,900 iterations per 4-year gap under
  // day-stepping) and exited early with a silently short list. The month-skip
  // collapses the walk and the restored budget covers the rest. Concrete
  // instants rather than the brute-force reference: the reference stepper's own
  // bound (~1.5 years) is the thing a 12-year sweep exceeds — but they are
  // still rendered through the same formatter, keeping the locale-proof rule.
  it("enumerates three leap days (FX2-E)", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2028-02-20T12:00:00Z"));
    const fmt = (iso: string) =>
      fmtInAppZone(iso, { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    // Midnight in Kolkata (+05:30) is 18:30Z the previous evening.
    expect(nextCronTimes("0 0 29 2 *", 3)).toEqual([
      fmt("2028-02-28T18:30:00Z"),
      fmt("2032-02-28T18:30:00Z"),
      fmt("2036-02-28T18:30:00Z"),
    ]);
  });

  it("enumerates an annual year-end fire across years (FX2-E)", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-12T12:00:00Z"));
    const fmt = (iso: string) =>
      fmtInAppZone(iso, { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    expect(nextCronTimes("0 0 31 12 *", 3)).toEqual([
      fmt("2026-12-30T18:30:00Z"),
      fmt("2027-12-30T18:30:00Z"),
      fmt("2028-12-30T18:30:00Z"),
    ]);
  });

  it("reports budget exhaustion instead of a silent short list (FX2-E)", { timeout: 30000 }, () => {
    setAppZone("Asia/Kolkata");
    // Feb 30 never exists: the search can only end on budget, and the flag is
    // what distinguishes "not found within the horizon" from "never fires" —
    // the preview must not render the two identically.
    const detail = nextCronTimesDetailed("0 0 30 2 *", 1);
    expect(detail.times).toHaveLength(0);
    expect(detail.truncated).toBe(true);
    // And a normal expression does not carry the flag.
    expect(nextCronTimesDetailed("0 0 1 * *", 3).truncated).toBe(false);
  });
});

describe("nextSpecTimes — interval strictly-after", () => {
  it("does not preview a fire that has already happened", () => {
    setAppZone("Asia/Kolkata");
    // An interval anchored so a fire lands exactly "now" (to the minute): the
    // backend's intervalSchedule.Next is strictly-after, so the preview's first
    // instant must be in the future, not the fire that just went.
    const now = Date.now();
    const anchor = now - 60 * 60000; // one hour ago, so a fire lands exactly now
    const times = nextSpecTimes({ cron: "", interval: "1h", startAt: new Date(anchor).toISOString() }, 1);
    expect(times).toHaveLength(1);
    // Pin the exact rendered instant: the NEXT fire (now+1h), not the one that
    // landed at `now`. The ceil-only version previews the elapsed fire, whose
    // rendering differs from this in the hour.
    const expected = fmtInAppZone(new Date(now + 60 * 60000).toISOString(), {
      weekday: "short",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
    expect(times[0]).toBe(expected);
  });
});

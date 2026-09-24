import { describe, it, expect, afterEach } from "vitest";
import { fmtInAppZone, setAppZone, appZone, appZoneParts } from "./datetime";

// fmtInAppZone is the single formatter every absolute timestamp routes through
// (timezone-update §5). These tests pin that the app zone is honored: a fixed UTC
// instant renders at different wall-clock hours in different app zones.

afterEach(() => setAppZone(undefined)); // reset the module holder between tests

describe("fmtInAppZone", () => {
  it("returns the em-dash placeholder for nullish input", () => {
    expect(fmtInAppZone(null)).toBe("—");
    expect(fmtInAppZone(undefined)).toBe("—");
    expect(fmtInAppZone("")).toBe("—");
  });

  it("returns the raw string for an unparseable instant", () => {
    expect(fmtInAppZone("not-a-date")).toBe("not-a-date");
  });

  it("renders a known UTC instant in the configured app zone", () => {
    // 2026-01-15T12:00:00Z → 07:00 in New York (EST, UTC-5) that day.
    const iso = "2026-01-15T12:00:00Z";
    setAppZone("America/New_York");
    const ny = fmtInAppZone(iso, { hour: "2-digit", minute: "2-digit", hour12: false });
    expect(ny).toContain("07:00");

    // Same instant in UTC → 12:00.
    setAppZone("UTC");
    const utc = fmtInAppZone(iso, { hour: "2-digit", minute: "2-digit", hour12: false });
    expect(utc).toContain("12:00");

    expect(ny).not.toBe(utc); // the zone is actually honored
  });
});

describe("setAppZone / appZone", () => {
  it("stores and clears the zone (empty ⇒ undefined browser default)", () => {
    setAppZone("Europe/Berlin");
    expect(appZone()).toBe("Europe/Berlin");
    setAppZone("");
    expect(appZone()).toBeUndefined();
    setAppZone("Asia/Tokyo");
    setAppZone(null);
    expect(appZone()).toBeUndefined();
  });
});

describe("appZoneParts", () => {
  it("derives wall-clock parts in the app zone (for the cron preview)", () => {
    // 2026-01-15T12:00:00Z is a Thursday; in NY it is 07:00 Thu.
    const d = new Date("2026-01-15T12:00:00Z");
    setAppZone("America/New_York");
    expect(appZoneParts(d)).toEqual({ minute: 0, hour: 7, dow: 4, dom: 15, month: 1 });
    setAppZone("UTC");
    expect(appZoneParts(d)).toEqual({ minute: 0, hour: 12, dow: 4, dom: 15, month: 1 });
  });
});

// VC.2: the Activity feed stopped rendering raw ISO UTC and now routes through
// fmtInAppZone with History's exact options. Assert the formatter (a) drops the
// raw-ISO markers (no "T"/"Z"), (b) honours the app zone, and (c) is 24-hour.
describe("fmtInAppZone — Activity/History format (VC.2)", () => {
  const ACTIVITY_OPTS = { month: "short", day: "numeric", year: "numeric", hour: "2-digit", minute: "2-digit", hour12: false } as const;

  it("renders a formatted date, not the raw ISO instant", () => {
    setAppZone("UTC");
    const out = fmtInAppZone("2026-07-01T21:43:55Z", ACTIVITY_OPTS);
    expect(out).not.toContain("T");
    expect(out).not.toContain("Z");
    expect(out).toContain("2026");
    expect(out).toMatch(/21:43/); // 24-hour, app zone (UTC)
  });

  it("shifts the wall-clock time into the configured app zone", () => {
    setAppZone("America/New_York"); // UTC-4 in July
    const out = fmtInAppZone("2026-07-01T21:43:55Z", ACTIVITY_OPTS);
    expect(out).toMatch(/17:43/);
  });
});

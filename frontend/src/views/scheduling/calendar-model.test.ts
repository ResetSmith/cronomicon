import { describe, expect, it } from "vitest";
import {
  calendarExpiry,
  calendarRollup,
  excludeSuppressed,
  isRealDate,
  parseDayLines,
  parseICS,
  suppressionFor,
  type DaysByCalendar,
} from "./calendar-model";

// The suppression maths, the expiry check and the two import parsers are where
// this feature's silent failures live, so they are tested as pure functions
// rather than through a rendered picker.

const DAYS: DaysByCalendar = {
  holidays: [{ day: "2026-07-03", label: "Independence Day (observed)" }, { day: "2026-12-25", label: "Christmas Day" }],
  weekdays: [{ day: "2026-07-06" }, { day: "2026-07-07" }],
  freeze: [{ day: "2026-07-06", label: "Q3 change freeze" }],
};

describe("suppressionFor", () => {
  it("suppresses a fire landing on a bound skip calendar's day, naming the label", () => {
    const s = suppressionFor("2026-07-03", ["holidays"], [], DAYS);
    expect(s).toEqual({ by: "holidays", label: "Independence Day (observed)", reason: "skip" });
  });

  it("lets a fire through on a day no bound calendar covers", () => {
    expect(suppressionFor("2026-07-02", ["holidays"], [], DAYS)).toBeNull();
  });

  it("suppresses a fire OUTSIDE an only calendar's day set", () => {
    const s = suppressionFor("2026-07-04", [], ["weekdays"], DAYS);
    expect(s?.reason).toBe("only");
    expect(s?.by).toBe("weekdays");
  });

  it("lets a fire through on a day inside the only set", () => {
    expect(suppressionFor("2026-07-06", [], ["weekdays"], DAYS)).toBeNull();
  });

  it("treats several only calendars as a union — either one is a run day", () => {
    expect(suppressionFor("2026-07-03", [], ["weekdays", "holidays"], DAYS)).toBeNull();
  });

  it("lets skip WIN over only on a day both cover (veto-before-only)", () => {
    // 2026-07-06 is a run day by `weekdays` AND a freeze day. The freeze wins:
    // an entry that may only run on weekdays must still not run during a freeze.
    const s = suppressionFor("2026-07-06", ["freeze"], ["weekdays"], DAYS);
    expect(s).toEqual({ by: "freeze", label: "Q3 change freeze", reason: "skip" });
  });

  it("applies a global calendar to an entry with no bindings of its own", () => {
    const s = suppressionFor("2026-07-06", [], [], DAYS, ["freeze"]);
    expect(s).toEqual({ by: "freeze", label: "Q3 change freeze", reason: "global" });
  });

  it("does not double-report a global calendar the entry also names explicitly", () => {
    const s = suppressionFor("2026-07-06", ["freeze"], [], DAYS, ["freeze"]);
    expect(s?.reason).toBe("skip");
  });

  it("treats a calendar whose days have not loaded as empty rather than throwing", () => {
    expect(suppressionFor("2026-07-03", ["not-fetched-yet"], [], DAYS)).toBeNull();
  });
});

describe("calendarExpiry", () => {
  it("flags a calendar whose last day has passed", () => {
    expect(calendarExpiry({ lastDay: "2025-12-25", daysRemaining: -30 }, 60)).toBe("expired");
  });

  it("flags a calendar inside the warning threshold, so there is time to renew", () => {
    expect(calendarExpiry({ lastDay: "2026-08-30", daysRemaining: 24 }, 60)).toBe("expiring");
  });

  it("stays quiet with coverage beyond the threshold", () => {
    expect(calendarExpiry({ lastDay: "2027-12-25", daysRemaining: 500 }, 60)).toBeNull();
  });

  it("treats an empty calendar as expired — it has never suppressed anything", () => {
    expect(calendarExpiry({ lastDay: null, daysRemaining: null }, 60)).toBe("expired");
  });
});

describe("excludeSuppressed", () => {
  it("drops annotated instants so 'what runs next' never names one that will not", () => {
    const items = [
      { at: "2026-07-02T06:00:00Z" },
      { at: "2026-07-03T06:00:00Z", suppressed: true, suppressedBy: "holidays" },
      { at: "2026-07-04T06:00:00Z" },
    ];
    expect(excludeSuppressed(items).map((i) => i.at)).toEqual([
      "2026-07-02T06:00:00Z",
      "2026-07-04T06:00:00Z",
    ]);
  });
});

describe("calendarRollup", () => {
  it("collapses agreeing entries to the bare calendar names", () => {
    expect(
      calendarRollup([{ skipCalendars: ["holidays"] }, { skipCalendars: ["holidays"] }]),
    ).toBe("holidays");
  });

  it("spells out disagreement rather than hiding it", () => {
    expect(
      calendarRollup([{ skipCalendars: ["holidays"] }, { skipCalendars: ["holidays"] }, {}]),
    ).toBe("holidays (2 of 3 entries)");
  });

  it("counts an unbound entry toward the denominator — that is the entry an auditor wants", () => {
    expect(calendarRollup([{ skipCalendars: ["holidays"] }, {}])).toBe("holidays (1 of 2 entries)");
  });

  it("counts one entry once even when it names a calendar in both polarities", () => {
    expect(calendarRollup([{ skipCalendars: ["freeze"], onlyCalendars: ["freeze"] }])).toBe("freeze");
  });

  it("says nothing when no entry binds anything", () => {
    expect(calendarRollup([{}, {}])).toBe("");
  });
});

describe("isRealDate", () => {
  it("accepts a real date", () => {
    expect(isRealDate("2026-02-28")).toBe(true);
  });

  it("rejects a well-formed but non-existent date", () => {
    expect(isRealDate("2026-02-30")).toBe(false);
    expect(isRealDate("2026-13-01")).toBe(false);
  });

  it("rejects anything that is not strict YYYY-MM-DD", () => {
    expect(isRealDate("07/04/2026")).toBe(false);
    expect(isRealDate("2026-7-4")).toBe(false);
  });
});

describe("parseDayLines", () => {
  it("parses dates with and without labels, sorted and deduped", () => {
    const { days, errors } = parseDayLines(
      ["2026-12-25,Christmas Day", "2026-01-01", "2026-12-25,duplicate ignored"].join("\n"),
    );
    expect(errors).toEqual([]);
    expect(days).toEqual([{ day: "2026-01-01" }, { day: "2026-12-25", label: "Christmas Day" }]);
  });

  it("keeps a label containing commas intact", () => {
    const { days } = parseDayLines("2026-11-26,Thanksgiving, observed");
    expect(days[0].label).toBe("Thanksgiving, observed");
  });

  it("skips blanks, comments and a CSV header without flagging them", () => {
    const { days, errors } = parseDayLines(["date,label", "", "# holidays", "2026-07-04,Independence Day"].join("\n"));
    expect(errors).toEqual([]);
    expect(days).toEqual([{ day: "2026-07-04", label: "Independence Day" }]);
  });

  it("reports a bad line by number and keeps the good ones — a paste is not all-or-nothing", () => {
    const { days, errors } = parseDayLines(["2026-07-04", "not-a-date", "2026-12-25"].join("\n"));
    expect(days).toHaveLength(2);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain("line 2");
  });
});

describe("parseICS", () => {
  const ics = [
    "BEGIN:VCALENDAR",
    "BEGIN:VEVENT",
    "DTSTART;VALUE=DATE:20260704",
    "SUMMARY:Independence Day",
    "END:VEVENT",
    "BEGIN:VEVENT",
    "DTSTART:20261225T000000Z",
    "SUMMARY:Christmas Day",
    "END:VEVENT",
    "END:VCALENDAR",
  ].join("\r\n");

  it("reads DTSTART dates and SUMMARY labels, date-only and date-time alike", () => {
    const { days, errors } = parseICS(ics);
    expect(errors).toEqual([]);
    expect(days).toEqual([
      { day: "2026-07-04", label: "Independence Day" },
      { day: "2026-12-25", label: "Christmas Day" },
    ]);
  });

  it("unfolds RFC 5545 continuation lines so a wrapped SUMMARY is not truncated", () => {
    const folded = ["BEGIN:VCALENDAR", "BEGIN:VEVENT", "DTSTART;VALUE=DATE:20260704", "SUMMARY:Independence", "  Day", "END:VEVENT", "END:VCALENDAR"].join("\r\n");
    expect(parseICS(folded).days[0].label).toBe("Independence Day");
  });

  it("says so when the file has no events, rather than silently importing nothing", () => {
    expect(parseICS("just some text").errors[0]).toContain("no VEVENT");
  });
});

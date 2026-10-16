// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";

// An entry is judged by its definition's agency's calendars (2.4.0, I2):
// bound names resolve within that agency, and only its global calendars apply.
// Two agencies may each have a calendar of a name. The calendar picker and the
// run note read each calendar with its owner, and offer and judge only the
// definition's agency's (2.4.1); by the name alone they offered both and read
// one's days for the other.

const DAY = "2027-03-01";
const gets: { name?: string; agencyId?: unknown }[] = [];

vi.mock("./calendars", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./calendars")>();
  return {
    ...actual,
    dayInAppZone: () => DAY,
    useCalendars: () => ({
      calendars: [
        { name: "holidays", global: false, ownerAgency: "ag-fin" },
        { name: "holidays", global: false, ownerAgency: "ag-tax" },
        { name: "freeze", global: true, ownerAgency: "ag-fin" },
        { name: "freeze", global: true, ownerAgency: "ag-tax" },
      ],
      expiryWarningDays: 7,
      loading: false,
      error: null,
    }),
  };
});
vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (_path: string, init?: { params?: { path?: { name?: string }; query?: { agencyId?: string } } }) => {
        const name = init?.params?.path?.name;
        const agencyId = init?.params?.query?.agencyId;
        gets.push({ name, agencyId });
        // Finance's calendars cover the day; Tax's do not.
        return { data: { name, days: agencyId === "ag-fin" ? [{ day: DAY, label: name === "freeze" ? "close" : "holiday" }] : [] } };
      }),
    } as unknown as typeof actual.api,
  };
});

import { CalendarPicker } from "./CalendarPicker";
import { RunCalendarNote } from "./RunCalendarNote";

afterEach(() => {
  cleanup();
  gets.length = 0;
});

const once = { cron: "", interval: null, startAt: `${DAY}T09:00:00Z`, endAt: null };

describe("the calendar picker, per agency", () => {
  it("offers one agency's calendars, and judges a one-shot by that agency's days", async () => {
    render(<CalendarPicker value={{ skipCalendars: ["holidays"], onlyCalendars: [] }} onChange={vi.fn()} spec={once} agency="ag-tax" />);
    // One "holidays" per role, Tax's: not Finance's beside it.
    await waitFor(() => expect(gets.length).toBeGreaterThan(0));
    expect(screen.getAllByRole("button", { name: /^holidays/ })).toHaveLength(2);
    expect(gets.every((g) => g.agencyId === "ag-tax")).toBe(true);
    // Tax's days do not cover the instant: no warning (Finance's would).
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(/a suppressed one-shot never runs/)).toBeNull();
  });

  it("warns when the definition's own agency's calendar covers the instant", async () => {
    render(<CalendarPicker value={{ skipCalendars: ["holidays"], onlyCalendars: [] }} onChange={vi.fn()} spec={once} agency="ag-fin" />);
    expect(await screen.findByText(/a suppressed one-shot never runs/)).toBeTruthy();
    expect(gets.every((g) => g.agencyId === "ag-fin")).toBe(true);
  });
});

describe("the run note, per agency", () => {
  it("names the definition's agency's global calendar, and not another's of the name", async () => {
    const { unmount } = render(<RunCalendarNote entries={[{ scheduleName: "nightly" }]} at={null} agency="ag-tax" />);
    await waitFor(() => expect(gets.some((g) => g.name === "freeze")).toBe(true));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(/would be suppressed/)).toBeNull();
    unmount();
    gets.length = 0;
    render(<RunCalendarNote entries={[{ scheduleName: "nightly" }]} at={null} agency="ag-fin" />);
    expect(await screen.findByText(/Schedule "nightly" would be suppressed today by close \(freeze\)\./)).toBeTruthy();
  });
});

// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

// Since 2.4.0 (§0a) a calendar is one agency's, and so is the reach of its
// "global": the scheduler unions a global calendar's days into the skip set of
// its own agency's entries only (calendar.EvaluateFor). The installation-wide
// stop is the change freeze, a global administrator's. The screens said
// "fleet-wide" and "every schedule"; they say whose.

const today = new Date().toISOString().slice(0, 10);

vi.mock("./calendars", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./calendars")>();
  return {
    ...actual,
    dayInAppZone: () => today,
    useCalendars: () => ({
      calendars: [{ name: "freeze", global: true, ownerAgency: "ag-fin", daysRemaining: 30, lastDay: "2099-01-01" }],
      expiryWarningDays: 7,
      loading: false,
      error: null,
    }),
    useCalendarDays: () => ({ days: { "ag-fin/freeze": [{ day: today, label: "quarter close" }] }, loading: false }),
    calendarExpiry: () => null,
  };
});
vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: { GET: vi.fn(async () => ({ data: [{ id: "global", name: "Global" }, { id: "ag-fin", name: "Finance" }] })) } as unknown as typeof actual.api,
  };
});

import { GlobalCalendarBanner } from "./GlobalCalendarBanner";

afterEach(cleanup);

describe("a global calendar reaches its own agency", () => {
  it("says whose runs are suppressed, and that other agencies' are not", async () => {
    render(<GlobalCalendarBanner />);
    expect(await screen.findByText(/Nothing of Finance starts today/)).toBeTruthy();
    expect(screen.queryByText(/fleet-wide/)).toBeNull();
    expect(screen.getByText(/A freeze stops a person.s Run too; other agencies are unaffected\./)).toBeTruthy();
  });
});

// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";

// Two agencies may each have a global calendar of one name (2.4.0, I2). The
// banner reads each one's days by its name AND its owner (`?agencyId=`); by
// the name alone it read one calendar's days for both, and said the wrong
// agency's runs were suppressed (2.4.1).

const today = new Date().toISOString().slice(0, 10);
const gets: { name?: string; agencyId?: unknown }[] = [];

vi.mock("./calendars", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./calendars")>();
  return {
    ...actual,
    dayInAppZone: () => today,
    useCalendars: () => ({
      calendars: [
        { name: "freeze", global: true, ownerAgency: "ag-fin", daysRemaining: 30, lastDay: "2099-01-01" },
        { name: "freeze", global: true, ownerAgency: "ag-tax", daysRemaining: 30, lastDay: "2099-01-01" },
      ],
      expiryWarningDays: 7,
      loading: false,
      error: null,
    }),
    calendarExpiry: () => null,
  };
});
vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, init?: { params?: { path?: { name?: string }; query?: { agencyId?: string } } }) => {
        if (path === "/agencies") return { data: [{ id: "global", name: "Global" }, { id: "ag-fin", name: "Finance" }, { id: "ag-tax", name: "Tax" }] };
        if (path === "/calendars/{name}") {
          const agencyId = init?.params?.query?.agencyId;
          gets.push({ name: init?.params?.path?.name, agencyId });
          // Finance's freeze covers today; Tax's does not.
          return { data: { name: "freeze", days: agencyId === "ag-fin" ? [{ day: today, label: "quarter close" }] : [{ day: "2099-01-01" }] } };
        }
        return { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});

import { GlobalCalendarBanner } from "./GlobalCalendarBanner";

afterEach(() => {
  cleanup();
  gets.length = 0;
});

describe("the active-global banner, two agencies' calendars of one name", () => {
  it("reads each by its owner, and names only the agency whose calendar covers today", async () => {
    render(<GlobalCalendarBanner />);
    expect(await screen.findByText(/Scheduled runs of Finance are suppressed today/)).toBeTruthy();
    expect(screen.queryByText(/Tax/)).toBeNull();
    await waitFor(() => expect(gets.map((g) => g.agencyId).sort()).toEqual(["ag-fin", "ag-tax"]));
  });
});

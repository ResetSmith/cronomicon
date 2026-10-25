// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.3 CA-25 to CA-29 on Schedules → Calendars. In an agency: its own
// calendars and, apart, its copies from Central, each with its state and, for
// its calendar editors, Update, the update mode and Make it our own; and Add
// from Central. In Central: the catalogue, each calendar with its copies, Add
// to agencies and Push the update.

type Ctx = { agency: string; agencyName: string; onOverview: boolean; choices: unknown[] };
const { inUse } = vi.hoisted(() => ({ inUse: { current: null as null | Ctx } }));
vi.mock("../../agency", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../agency")>();
  return { ...actual, useAgencyOptional: () => inUse.current };
});
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => null };
});

let CALENDARS: Record<string, unknown>[] = [];
let CATALOGUE: Record<string, unknown>[] = [];
let pushAnswer: unknown = {};
const writes: { method: string; path: string; body?: unknown; query?: unknown; path_?: unknown }[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  type Init = { body?: unknown; params?: { query?: unknown; path?: unknown } };
  const record = (method: string) => async (path: string, init?: Init) => {
    writes.push({ method, path, body: init?.body, query: init?.params?.query, path_: init?.params?.path });
    if (path === "/calendars/catalogue/{uid}/push") return { data: pushAnswer };
    return { data: { items: [] }, response: { ok: true, status: 200 } };
  };
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({ compose: true, composeAdmin: true })),
    api: {
      GET: vi.fn(async (path: string, init?: Init) => {
        if (path === "/calendars") return { data: { items: CALENDARS } };
        if (path === "/calendars/catalogue") return { data: { items: CATALOGUE } };
        if (path === "/agencies")
          return { data: [{ id: "global", name: "Central" }, { id: "ag-ops", name: "Operations" }, { id: "ag-tax", name: "Tax" }, { id: "ag-hr", name: "HR" }] };
        if (path === "/calendars/{name}") {
          const name = (init?.params?.path as { name: string }).name;
          return { data: { ...CALENDARS.find((cal) => cal.name === name), days: [] } };
        }
        return { data: [] };
      }),
      POST: vi.fn(record("POST")),
      PUT: vi.fn(record("PUT")),
      DELETE: vi.fn(record("DELETE")),
    } as unknown as typeof actual.api,
  };
});

import { CalendarsTab } from "./CalendarsTab";
import { setGlobalAgencyName } from "../../globalName";

const OPS: Ctx = { agency: "ag-ops", agencyName: "Operations", onOverview: false, choices: [] };
const CENTRAL: Ctx = { agency: "global", agencyName: "Central", onOverview: false, choices: [] };
const OWN = { name: "on-call", ownerAgency: "ag-ops", dayCount: 4, canEdit: true, canDelete: true };
const COPY = {
  name: "us-holidays",
  ownerAgency: "ag-ops",
  dayCount: 11,
  canEdit: false,
  canDelete: true,
  fromCentral: {
    catalogueUid: "cal-1",
    catalogueName: "us-holidays",
    updateMode: "ask",
    state: "update_available",
    pending: { daysAdded: ["2027-01-01", "2027-07-05"], daysRemoved: [], daysRelabelled: [], descriptionChanged: false },
  },
};

beforeEach(() => {
  inUse.current = OPS;
  // An installation made since 2.5.2: the built-in agency is Central, and the
  // screens read its name, never a literal.
  setGlobalAgencyName("Central");
  CALENDARS = [OWN, COPY];
  CATALOGUE = [];
  pushAnswer = {};
  writes.length = 0;
});
afterEach(() => {
  cleanup();
  setGlobalAgencyName(null);
});

const show = () =>
  render(
    <MemoryRouter>
      <CalendarsTab />
    </MemoryRouter>,
  );
const rowOf = (name: string) => screen.getByText(name, { selector: "span" }).closest("tr") as HTMLElement;

describe("Calendars in an agency (CA-26 to CA-29)", () => {
  it("shows its own calendars and its copies from Central apart, each copy with its state", async () => {
    show();
    expect(await screen.findByText("Our calendars")).toBeTruthy();
    expect(screen.getByText("From Central")).toBeTruthy();
    const copy = rowOf("us-holidays");
    expect(within(copy).getByText("update available").getAttribute("title")).toBe("2 days added");
    expect(within(rowOf("on-call")).queryByText(/update/)).toBeNull();
  });

  it("updates a copy, changes how it takes updates, and makes it its own after a confirmation", async () => {
    show();
    await screen.findByText("From Central");
    const copy = rowOf("us-holidays");
    fireEvent.click(within(copy).getByRole("button", { name: "Update" }));
    await waitFor(() => expect(writes.some((w) => w.path === "/calendars/{name}/update")).toBe(true));
    expect(writes.find((w) => w.path === "/calendars/{name}/update")).toMatchObject({ method: "POST", query: { agencyId: "ag-ops" }, path_: { name: "us-holidays" } });

    fireEvent.change(within(rowOf("us-holidays")).getByRole("combobox", { name: "How us-holidays takes updates from Central" }), { target: { value: "auto" } });
    await waitFor(() => expect(writes.some((w) => w.path === "/calendars/{name}/update-mode")).toBe(true));
    expect(writes.find((w) => w.path === "/calendars/{name}/update-mode")).toMatchObject({ method: "PUT", body: { updateMode: "auto" } });

    fireEvent.click(within(rowOf("us-holidays")).getByRole("button", { name: "Make it our own" }));
    expect(await screen.findByText(/takes no more updates from it/)).toBeTruthy();
    expect(writes.some((w) => w.path === "/calendars/{name}/detach")).toBe(false);
    const dialogs = screen.getAllByRole("button", { name: "Make it our own" });
    fireEvent.click(dialogs[dialogs.length - 1]);
    await waitFor(() => expect(writes.some((w) => w.path === "/calendars/{name}/detach")).toBe(true));
  });

  it("offers no copy actions to someone who may not change its calendars", async () => {
    CALENDARS = [OWN, { ...COPY, canDelete: false }];
    show();
    await screen.findByText("From Central");
    expect(within(rowOf("us-holidays")).queryByRole("button", { name: "Update" })).toBeNull();
    expect(within(rowOf("us-holidays")).getByRole("button", { name: "View" })).toBeTruthy();
  });

  it("opens a copy read-only, says why, and keeps Delete", async () => {
    show();
    await screen.findByText("From Central");
    fireEvent.click(within(rowOf("us-holidays")).getByRole("button", { name: "View" }));
    expect(await screen.findByText(/This calendar is a copy of us-holidays from Central: its days and settings change only by an update/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete calendar" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save calendar" })).toBeNull();
  });

  it("adds a calendar from Central under a name and an update mode of the agency's choosing", async () => {
    CATALOGUE = [{ uid: "cal-2", name: "uk-holidays", global: false, recordSuppressed: false, dayCount: 8, lastDay: "2027-12-28", hash: "h", copiedAs: [] }];
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Add from Central" }));
    fireEvent.click(await screen.findByRole("radio", { name: /uk-holidays/ }));
    fireEvent.change(screen.getByLabelText("Name here"), { target: { value: "bank-holidays" } });
    fireEvent.click(screen.getByRole("radio", { name: /Update automatically/ }));
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(writes.some((w) => w.path === "/calendars/from-central")).toBe(true));
    expect(writes.find((w) => w.path === "/calendars/from-central")!.body).toEqual({ catalogueUid: "cal-2", name: "bank-holidays", updateMode: "auto" });
  });
});

describe("Calendars in Central: the catalogue (CA-25, CA-26, CA-28)", () => {
  const CAT = {
    name: "us-holidays",
    uid: "cal-1",
    ownerAgency: "global",
    dayCount: 12,
    canEdit: true,
    canDelete: true,
    copies: [
      { agencyId: "ag-ops", agencyName: "Operations", name: "us-holidays", updateMode: "ask", state: "update_available" },
      { agencyId: "ag-tax", agencyName: "Tax", name: "holidays", updateMode: "auto", state: "up_to_date" },
    ],
  };
  beforeEach(() => {
    inUse.current = CENTRAL;
    CALENDARS = [CAT];
  });

  it("is the catalogue, with each calendar's copies, and no Add from Central", async () => {
    show();
    expect(await screen.findByText(/Central's catalogue: calendars the agencies take a copy of/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add from Central" })).toBeNull();
    expect(within(rowOf("us-holidays")).getByText(/2 agencies/).textContent).toBe("2 agencies · 1 behind");
  });

  it("adds a catalogue calendar to the agencies that have none", async () => {
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Add to agencies…" }));
    expect(await screen.findByRole("checkbox", { name: "HR" })).toBeTruthy();
    expect(screen.queryByRole("checkbox", { name: "Tax" })).toBeNull(); // has a copy
    expect(screen.queryByRole("checkbox", { name: "Central" })).toBeNull();
    fireEvent.click(screen.getByRole("checkbox", { name: "HR" }));
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(writes.some((w) => w.path === "/calendars/from-central")).toBe(true));
    expect(writes.find((w) => w.path === "/calendars/from-central")!.body).toEqual({ catalogueUid: "cal-1", agencyIds: ["ag-hr"], updateMode: "ask" });
  });

  it("pushes the update to the copies behind it, and says what happened in each", async () => {
    pushAnswer = { updated: [{ agencyId: "ag-ops", name: "us-holidays" }], upToDate: [], blocked: [], noCopy: [] };
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Push the update…" }));
    const ops = (await screen.findByRole("checkbox", { name: "Operations" })) as HTMLInputElement;
    expect(ops.checked).toBe(true);
    expect(screen.queryByRole("checkbox", { name: "Tax" })).toBeNull(); // up to date
    fireEvent.click(screen.getByRole("button", { name: "Push the update" }));
    await waitFor(() => expect(writes.some((w) => w.path === "/calendars/catalogue/{uid}/push")).toBe(true));
    expect(writes.find((w) => w.path === "/calendars/catalogue/{uid}/push")).toMatchObject({ body: { agencyIds: ["ag-ops"] }, path_: { uid: "cal-1" } });
    expect(await screen.findByText("Updated in Operations.")).toBeTruthy();
  });

  it("offers no Push when no copy is behind", async () => {
    CALENDARS = [{ ...CAT, copies: [CAT.copies[1]] }];
    show();
    await screen.findByText("Add to agencies…");
    expect(screen.queryByRole("button", { name: "Push the update…" })).toBeNull();
  });
});

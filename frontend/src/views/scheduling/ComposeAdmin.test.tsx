// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// In-app schedules and working calendars per agency (2.4.0 I2; R8).
//
// Until 2.4.0 a reusable schedule and a calendar belonged to no agency, and the
// server took their writes only from `composeAdmin` (GC, 2.2.2). Since I2 each
// is one agency's (`ownerAgency`), two agencies may each have one of a name,
// and a write takes Compose on the owner. Pinned here, for each:
//   · no compose                    → no authoring controls;
//   · compose on Finance            → New enabled; Finance's row editable, its
//                                     links and reads naming the agency; Tax's
//                                     row disabled (or read-only) with whose it is;
//   · a global composer             → everything enabled.
// Reactions are still a global composer's (the last block).

let caps: Record<string, boolean> = {};
let ACCESS: unknown = null;
const gets: { path: string; query?: Record<string, unknown> }[] = [];

const FIN_SCHEDULE = { name: "nightly", source: "cronomicon", cron: "0 2 * * *", description: "Every night", tags: [], usedBy: [], ownerAgency: "ag-fin" };
const TAX_SCHEDULE = { name: "weekly", source: "cronomicon", cron: "0 3 * * 1", description: "Every week", tags: [], usedBy: [], ownerAgency: "ag-tax" };
const CALENDARS = [
  { name: "holidays", description: "Finance holidays", global: false, dayCount: 3, lastDay: "2027-12-25", daysRemaining: 400, usedBy: [], days: [], ownerAgency: "ag-fin" },
  { name: "freeze", description: "Tax freeze", global: false, dayCount: 2, lastDay: "2027-12-20", daysRemaining: 390, usedBy: [], days: [], ownerAgency: "ag-tax" },
];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({
      vault: false, apprise: false, compose: false, composeUnbound: false, manageRoles: false,
      configureApp: false, manageEnvVars: false, publishSchedule: false, triggerJobs: false,
      killJobs: false, unrestricted: false,
      ...caps,
    })),
    api: {
      GET: vi.fn(async (path: string, init?: { params?: { path?: { name?: string }; query?: Record<string, unknown> } }) => {
        gets.push({ path, query: init?.params?.query });
        if (path === "/schedule-defs") return { data: [FIN_SCHEDULE, TAX_SCHEDULE] };
        if (path === "/schedule-defs/{name}") return { data: init?.params?.path?.name === "weekly" ? TAX_SCHEDULE : FIN_SCHEDULE };
        if (path === "/calendars") return { data: { items: CALENDARS, expiryWarningDays: 30 } };
        if (path === "/calendars/{name}") return { data: CALENDARS.find((x) => x.name === init?.params?.path?.name) };
        if (path === "/agencies") return { data: [{ id: "global", name: "Global" }, { id: "ag-fin", name: "Finance" }, { id: "ag-tax", name: "Tax" }] };
        if (path === "/jobs" || path === "/workflows") return { data: { items: [], totalItems: 0 } };
        return { data: [] };
      }),
      PUT: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
      POST: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
  };
});
vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, useMyAccess: () => ACCESS, fetchMyAccess: vi.fn(async () => ACCESS) };
});

import { Schedules } from "../Schedules";
import { ScheduleBuilder } from "../ScheduleBuilder";
import { ReactionsEditor, emptyReaction } from "./ReactionsEditor";

const WHY = /shared by every agency — only a global administrator \(a role on every agency\) can change them\./;

const FIN_COMPOSER = { compose: true };
const COMPOSE_ADMIN = { compose: true, configureApp: true, configureAppGlobal: true, composeAdmin: true, unrestricted: true };
const finGrant = { role: "author", allScopes: false, agencyId: "ag-fin", agencyName: "Finance", scopes: [], permissions: ["compose"], groups: [], origin: "group" };

beforeEach(() => {
  caps = {};
  ACCESS = { grants: [] };
  gets.length = 0;
  localStorage.clear();
});
afterEach(cleanup);

const at = (url: string, node: React.ReactNode) => render(<MemoryRouter initialEntries={[url]}>{node}</MemoryRouter>);
const btn = (name: string | RegExp) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("Schedules catalog — a schedule is its agency's (I2; R8)", () => {
  const openCatalog = async () => {
    at("/schedules?tab=catalog", <Schedules />);
    await waitFor(() => expect(screen.getByText("nightly")).toBeTruthy());
  };
  const expandRow = async (name: string) => {
    fireEvent.click(screen.getByText(name));
    await waitFor(() => expect(screen.getByText(/Tags are stored in Cronomicon only/)).toBeTruthy());
  };

  it("hides New schedule, Edit and Delete from a caller who composes nowhere", async () => {
    await openCatalog();
    await expandRow("nightly");
    expect(screen.queryByRole("button", { name: "+ New schedule" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.getByText(/authoring requires the Compose capability/)).toBeTruthy();
  });

  it("lets a Finance composer author Finance's schedule, naming the agency, and not Tax's", async () => {
    caps = FIN_COMPOSER;
    ACCESS = { grants: [finGrant] };
    await openCatalog();
    const add = await waitFor(() => btn("+ New schedule"));
    expect(add.disabled).toBe(false);
    expect(add.closest("a")?.getAttribute("href")).toBe("/schedule-builder");

    await expandRow("nightly");
    await waitFor(() => expect(btn("Edit").closest("a")?.getAttribute("href")).toBe("/schedule-builder?name=nightly&agencyId=ag-fin"));
    expect(btn("Delete").disabled).toBe(false);
    expect(gets.find((g) => g.path === "/schedule-defs/{name}")?.query).toMatchObject({ agencyId: "ag-fin" });
    cleanup();

    await openCatalog();
    await expandRow("weekly");
    await waitFor(() => expect(btn("Edit").disabled).toBe(true));
    expect(btn("Edit").title).toBe("This schedule belongs to Tax — only that agency's composers can change it.");
    expect(btn("Delete").disabled).toBe(true);
    expect(screen.getByRole("note").textContent).toMatch(/belongs to Tax/);
  });

  it("enables everything for a global composer", async () => {
    caps = COMPOSE_ADMIN;
    await openCatalog();
    await expandRow("weekly");
    await waitFor(() => expect(btn("Edit").closest("a")?.getAttribute("href")).toBe("/schedule-builder?name=weekly&agencyId=ag-tax"));
    expect(btn("Delete").disabled).toBe(false);
    expect(screen.queryByRole("note")).toBeNull();
  });
});

describe("Schedule Builder — the page itself (I2; R8)", () => {
  it("says what it takes to a caller who composes nowhere", async () => {
    at("/schedule-builder", <ScheduleBuilder />);
    const note = await waitFor(() => screen.getByRole("note"));
    expect(note.textContent).toMatch(/Compose<\/strong>|Compose capability on the agency the schedule belongs to/);
    expect(screen.queryByRole("button", { name: /Create schedule|Save/ })).toBeNull();
  });

  it("opens for a Finance composer, whose new schedule is Finance's", async () => {
    caps = FIN_COMPOSER;
    ACCESS = { grants: [finGrant] };
    at("/schedule-builder", <ScheduleBuilder />);
    await waitFor(() => expect(screen.queryByRole("note")).toBeNull());
    const agency = (await screen.findByLabelText("Agency")) as HTMLSelectElement;
    await waitFor(() => expect(agency.value).toBe("ag-fin"));
  });

  it("reads the schedule it edits from the agency the address names", async () => {
    caps = COMPOSE_ADMIN;
    at("/schedule-builder?name=weekly&agencyId=ag-tax", <ScheduleBuilder />);
    await waitFor(() => expect(gets.find((g) => g.path === "/schedule-defs/{name}")?.query).toMatchObject({ agencyId: "ag-tax" }));
  });
});

describe("Calendars tab — a calendar is its agency's (I2; R8)", () => {
  const openCalendars = async () => {
    at("/schedules?tab=calendars", <Schedules />);
    await waitFor(() => expect(screen.getByText("holidays")).toBeTruthy());
  };
  const rowButton = (cal: string) => within(screen.getByText(cal).closest("tr") as HTMLElement).getByRole("button") as HTMLButtonElement;

  it("hides New calendar from a caller who composes nowhere, and opens rows read-only", async () => {
    await openCalendars();
    expect(screen.queryByRole("button", { name: "+ New calendar" })).toBeNull();
    expect(rowButton("holidays").textContent).toBe("View");
  });

  it("lets a Finance composer edit Finance's calendar, naming the agency, and only view Tax's", async () => {
    caps = FIN_COMPOSER;
    ACCESS = { grants: [finGrant] };
    await openCalendars();
    expect(btn("+ New calendar").disabled).toBe(false);
    await waitFor(() => expect(rowButton("holidays").textContent).toBe("Edit"));
    expect(rowButton("freeze").textContent).toBe("View");

    fireEvent.click(rowButton("freeze"));
    await waitFor(() => expect(screen.getByText(/Viewing the working calendar/)).toBeTruthy());
    await waitFor(() => expect(screen.getByRole("note").textContent).toMatch(/Tax's — only that agency's composers/));
    expect(gets.find((g) => g.path === "/calendars/{name}")?.query).toMatchObject({ agencyId: "ag-tax" });
    expect(screen.queryByRole("button", { name: /Save calendar/ })).toBeNull();
  });

  it("enables New calendar and Edit for a global composer", async () => {
    caps = COMPOSE_ADMIN;
    await openCalendars();
    expect(btn("+ New calendar").disabled).toBe(false);
    fireEvent.click(rowButton("freeze"));
    await waitFor(() => expect(btn(/Save calendar/).disabled).toBe(false));
    expect(screen.queryByRole("note")).toBeNull();
  });
});

// The reactions editor is hosted by the Job Composer and the Workflow Editor;
// each passes `disabledReason` when the caller is not a compose administrator.
// What matters is that the existing reactions stay READABLE and that nothing in
// the list can change — an unchanged list is never dirty, so the host never
// issues the PUT /reactions/… the server would refuse.
describe("ReactionsEditor — disabledReason (GC)", () => {
  const value = [{ ...emptyReaction(1), name: "after-extract", onName: "extract" }];

  it("renders the reactions read-only with the reason when the host passes one", async () => {
    const onChange = vi.fn();
    render(
      <ReactionsEditor
        value={value}
        onChange={onChange}
        ownerKind="job"
        ownerName="load"
        ownerSource="cronomicon"
        disabledReason="Reusable schedules, calendars, reactions and the recycle bin are shared by every agency — only a global administrator (a role on every agency) can change them."
      />,
    );
    expect(screen.getByRole("note").textContent).toMatch(WHY);
    const name = screen.getByDisplayValue("after-extract") as HTMLInputElement;
    expect(name.disabled).toBe(true);
    expect(btn("Remove").disabled).toBe(true);
    const add = btn("+ Add reaction");
    expect(add.disabled).toBe(true);
    expect(add.title).toMatch(WHY);

    fireEvent.click(add);
    fireEvent.click(btn("Remove"));
    expect(onChange).not.toHaveBeenCalled();
  });

  it("is editable when no reason is passed", async () => {
    const onChange = vi.fn();
    render(<ReactionsEditor value={value} onChange={onChange} ownerKind="job" ownerName="load" ownerSource="cronomicon" />);
    expect(screen.queryByRole("note")).toBeNull();
    expect((screen.getByDisplayValue("after-extract") as HTMLInputElement).disabled).toBe(false);
    fireEvent.click(btn("+ Add reaction"));
    expect(onChange).toHaveBeenCalledTimes(1);
  });
});

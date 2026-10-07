// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// GC (v2.2.2, gate closing) — reusable schedules, working calendars and
// reactions belong to NO agency: a reusable schedule retimes everyone who
// references it, a `global` calendar vetoes every agency's fires, a reaction
// couples two definitions whoever owns them. The server takes their writes only
// from `composeAdmin` — compose AND configureApp on one all-agencies grant.
//
// The UI used to gate all three on the flat `compose` flag, which a departmental
// composer holds. So these pin the three states of each authoring control:
//   · no compose                → hidden, as before (irrelevance hides);
//   · compose, not composeAdmin → shown DISABLED with the reason (FX-7);
//   · composeAdmin              → enabled.
// A schedule put directly on a job or workflow in its composer is not affected
// and is not under test here.

let caps: Record<string, boolean> = {};
const puts: string[] = [];

const SCHEDULE = { name: "nightly", source: "cronomicon", cron: "0 2 * * *", description: "Every night", tags: [], usedBy: [] };
const CALENDAR = { name: "holidays", description: "Company holidays", global: false, dayCount: 3, lastDay: "2027-12-25", daysRemaining: 400, usedBy: [], days: [] };

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
      GET: vi.fn(async (path: string) => {
        if (path === "/schedule-defs") return { data: [SCHEDULE] };
        if (path === "/schedule-defs/{name}") return { data: SCHEDULE };
        if (path === "/calendars") return { data: { items: [CALENDAR], expiryWarningDays: 30 } };
        if (path === "/calendars/{name}") return { data: CALENDAR };
        if (path === "/jobs" || path === "/workflows") return { data: { items: [], totalItems: 0 } };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string) => {
        puts.push(path);
        return { data: {}, response: { ok: true, status: 200 } };
      }),
      POST: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
  };
});

import { Schedules } from "../Schedules";
import { ScheduleBuilder } from "../ScheduleBuilder";
import { ReactionsEditor, emptyReaction } from "./ReactionsEditor";

const WHY = /shared by every agency — only a global administrator \(a role on every agency\) can change them\./;

const COMPOSER = { compose: true };
const COMPOSE_ADMIN = { compose: true, configureApp: true, configureAppGlobal: true, composeAdmin: true, unrestricted: true };

beforeEach(() => {
  caps = {};
  puts.length = 0;
  localStorage.clear();
});
afterEach(cleanup);

const at = (url: string, node: React.ReactNode) => render(<MemoryRouter initialEntries={[url]}>{node}</MemoryRouter>);
const btn = (name: string | RegExp) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("Schedules catalog — authoring a reusable schedule (GC)", () => {
  const openCatalog = async () => {
    at("/schedules?tab=catalog", <Schedules />);
    await waitFor(() => expect(screen.getByText("nightly")).toBeTruthy());
  };
  const expandRow = async () => {
    fireEvent.click(screen.getByText("nightly"));
    await waitFor(() => expect(screen.getByText(/Tags are stored in Cronomicon only/)).toBeTruthy());
  };

  it("hides New schedule, Edit and Delete from a caller who composes nowhere", async () => {
    await openCatalog();
    await expandRow();
    expect(screen.queryByRole("button", { name: "+ New schedule" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.getByText(/authoring requires the Compose capability/)).toBeTruthy();
  });

  it("shows them DISABLED with the reason to a departmental composer", async () => {
    caps = COMPOSER;
    await openCatalog();
    const add = await waitFor(() => btn("+ New schedule"));
    expect(add.disabled).toBe(true);
    expect(add.title).toMatch(WHY);
    // Not wrapped in a live link: a disabled button inside an <a> still navigates.
    expect(add.closest("a")).toBeNull();

    await expandRow();
    for (const name of ["Edit", "Delete"]) {
      expect(btn(name).disabled, name).toBe(true);
      expect(btn(name).title, name).toMatch(WHY);
    }
    // And the sentence is on the page, not only in a tooltip.
    expect(screen.getByRole("note").textContent).toMatch(WHY);
  });

  it("enables them for a compose administrator", async () => {
    caps = COMPOSE_ADMIN;
    await openCatalog();
    const add = await waitFor(() => btn("+ New schedule"));
    expect(add.disabled).toBe(false);
    expect(add.closest("a")?.getAttribute("href")).toBe("/schedule-builder");

    await expandRow();
    expect(btn("Edit").disabled).toBe(false);
    expect(btn("Edit").closest("a")?.getAttribute("href")).toBe("/schedule-builder?name=nightly");
    expect(btn("Delete").disabled).toBe(false);
    expect(screen.queryByRole("note")).toBeNull();
  });
});

describe("Schedule Builder — the page itself (GC)", () => {
  it("refuses a departmental composer with the reason, and says what they CAN author", async () => {
    caps = COMPOSER;
    at("/schedule-builder", <ScheduleBuilder />);
    const note = await waitFor(() => screen.getByRole("note"));
    expect(note.textContent).toMatch(WHY);
    expect(note.textContent).toMatch(/directly on one of your own jobs or workflows/);
    expect(screen.queryByRole("button", { name: /Create schedule|Save/ })).toBeNull();
  });

  it("opens for a compose administrator", async () => {
    caps = COMPOSE_ADMIN;
    at("/schedule-builder", <ScheduleBuilder />);
    await waitFor(() => expect(screen.queryByRole("note")).toBeNull());
    await waitFor(() => expect(screen.getAllByRole("textbox").length).toBeGreaterThan(0));
  });
});

describe("Calendars tab — authoring a working calendar (GC)", () => {
  const openCalendars = async () => {
    at("/schedules?tab=calendars", <Schedules />);
    await waitFor(() => expect(screen.getByText("holidays")).toBeTruthy());
  };

  it("hides New calendar from a caller who composes nowhere, and opens rows read-only", async () => {
    await openCalendars();
    expect(screen.queryByRole("button", { name: "+ New calendar" })).toBeNull();
    expect(btn("View")).toBeTruthy();
  });

  it("shows New calendar DISABLED with the reason to a departmental composer, and opens rows read-only with the reason", async () => {
    caps = COMPOSER;
    await openCalendars();
    const add = await waitFor(() => btn("+ New calendar"));
    expect(add.disabled).toBe(true);
    expect(add.title).toMatch(WHY);

    // The row button says View, not Edit — and the editor it opens cannot save.
    fireEvent.click(btn("View"));
    await waitFor(() => expect(screen.getByText(/Viewing the working calendar/)).toBeTruthy());
    await waitFor(() => expect(screen.getByRole("note").textContent).toMatch(WHY));
    expect(screen.queryByRole("button", { name: /Save calendar/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Delete calendar/ })).toBeNull();
  });

  it("enables New calendar and Edit for a compose administrator", async () => {
    caps = COMPOSE_ADMIN;
    await openCalendars();
    const add = await waitFor(() => btn("+ New calendar"));
    expect(add.disabled).toBe(false);
    expect(add.title).toBe("");

    fireEvent.click(btn("Edit"));
    await waitFor(() => expect(btn(/Save calendar/).disabled).toBe(false));
    expect(screen.queryByRole("note")).toBeNull();
    expect(btn(/Delete calendar/)).toBeTruthy();
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

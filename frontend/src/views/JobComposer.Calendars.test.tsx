// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.3 CA-30: a job's own Forbid and Allow calendars. The compose PUT is a
// full replace, and a list left out is cleared (until CA5 the composer did not
// send them, so any edit cleared them): an edit sends them as loaded, the
// pickers change them, and the same calendar in both is refused before save.

const JOB = {
  id: 7,
  name: "nightly-backup",
  source: "cronomicon",
  scriptRef: "tools/backup.sh",
  scope: "ops-local",
  env: {},
  prompts: [],
  schedules: [],
  enabled: true,
  forbidCalendars: ["holidays"],
  allowCalendars: ["weekdays"],
};
const CALENDARS = [
  { name: "holidays", ownerAgency: "ag-ops", dayCount: 3 },
  { name: "weekdays", ownerAgency: "ag-ops", dayCount: 200 },
  { name: "freeze", ownerAgency: "ag-ops", global: true, dayCount: 5 },
];

const writes: { method: string; path: string; body: Record<string, unknown> }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number } } }) => {
        if (path === "/jobs/{jobId}") return opts?.params?.path?.jobId === 7 ? { data: JOB } : { data: undefined, error: {} };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        if (path === "/scripts/{name}") return { data: { name: "tools/backup.sh", runType: "bash", variables: [], prompts: [] } };
        if (path === "/calendars") return { data: { items: CALENDARS } };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { body: Record<string, unknown> }) => {
        writes.push({ method: "POST", path, body: opts.body });
        return { data: { id: 99 }, response: { ok: true, status: 200 } };
      }),
      PUT: vi.fn(async (path: string, opts: { body: Record<string, unknown> }) => {
        writes.push({ method: "PUT", path, body: opts.body });
        return { data: {}, response: { ok: true, status: 200 } };
      }),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ compose: true, composeUnbound: true, manageEnvVars: true })),
  };
});

import { JobComposer } from "./JobComposer";

beforeEach(() => {
  writes.length = 0;
});
afterEach(cleanup);

const renderAt = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <JobComposer />
    </MemoryRouter>,
  );
// The job's own pickers: the two blocks under "Calendars (optional)" (an inline
// schedule row carries its own entry pickers).
const field = () => screen.getByText("Calendars (optional)").parentElement!.parentElement as HTMLElement;
const roleBlock = (label: RegExp) => within(field()).getByText(label).parentElement as HTMLElement;
const chip = (label: RegExp, name: string) => within(roleBlock(label)).getByRole("button", { name: new RegExp(`^${name}`) });
const jobWrite = () => writes.find((w) => w.path === "/jobs/{jobId}" || w.path === "/jobs");

describe("JobComposer — the job's own calendars (2.5.3 CA-30)", () => {
  it("sends them as loaded on an edit that does not touch them", async () => {
    renderAt("/compose?id=7");
    await screen.findByText("Calendars (optional)");
    await waitFor(() => expect(chip(/^Forbid — /, "holidays").getAttribute("style")).toContain("background"));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(jobWrite()).toBeTruthy());
    expect(jobWrite()!.body).toMatchObject({ forbidCalendars: ["holidays"], allowCalendars: ["weekdays"] });
  });

  it("sends what the pickers change", async () => {
    renderAt("/compose?id=7");
    await screen.findByText("Calendars (optional)");
    fireEvent.click(chip(/^Forbid — /, "holidays")); // off
    fireEvent.click(chip(/^Forbid — /, "freeze")); // on
    fireEvent.click(chip(/^Allow — /, "weekdays")); // off
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(jobWrite()).toBeTruthy());
    expect(jobWrite()!.body).toMatchObject({ forbidCalendars: ["freeze"], allowCalendars: [] });
  });

  it("does not offer a freeze calendar as an Allow calendar", async () => {
    renderAt("/compose?id=7");
    await screen.findByText("Calendars (optional)");
    expect((chip(/^Allow — /, "freeze") as HTMLButtonElement).disabled).toBe(true);
  });

  it("refuses the same calendar as both Forbid and Allow, before saving", async () => {
    renderAt("/compose?id=7");
    await screen.findByText("Calendars (optional)");
    fireEvent.click(chip(/^Allow — /, "holidays"));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findAllByText(/"holidays" cannot be both a Forbid and an Allow calendar on one job\./)).toBeTruthy();
    expect(jobWrite()).toBeUndefined();
  });

  it("sends both lists, empty, on a new job", async () => {
    renderAt("/compose?script=tools%2Fbackup.sh");
    await waitFor(() => expect(screen.getAllByText("tools/backup.sh").length).toBeGreaterThan(0));
    fireEvent.change(screen.getByPlaceholderText("nightly-backup"), { target: { value: "new-job" } });
    const combo = screen.getByRole("combobox", { name: "Scope" });
    fireEvent.focus(combo);
    fireEvent.change(combo, { target: { value: "ops-local" } });
    fireEvent.keyDown(combo, { key: "Enter" });
    fireEvent.click(screen.getByRole("button", { name: "Create job" }));
    await waitFor(() => expect(jobWrite()).toBeTruthy());
    expect(jobWrite()!.body).toMatchObject({ forbidCalendars: [], allowCalendars: [] });
  });
});

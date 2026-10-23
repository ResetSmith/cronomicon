// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RB-24 — Run/Kill gate on the SERVER's per-row answer, not on the flat
// /capabilities union.
//
// The flat flag says "may trigger somewhere". It cannot answer per-row truth: an
// operator scoped to Finance reports triggerJobs=true and still may not touch a Tax
// job. This file pins that the row wins, because the failure mode is silent — a UI
// that keeps using the union looks correct on every single-department install and
// only diverges once departments exist.
//
// The mock therefore reports the capability TRUE while one row says canRun FALSE.
// A component reading the union shows both buttons and fails here.

const JOBS = {
  items: [
    { id: 1, name: "tax-job", type: "bash", scope: "tax", source: "git", status: "idle", schedule: "manual", agencies: ["Tax"], canRun: true, canKill: true },
    { id: 2, name: "finance-job", type: "bash", scope: "finance", source: "git", status: "idle", schedule: "manual", agencies: ["Finance"], canRun: false, canKill: false },
    // A job with no scope is Global's: a department may run it against its own
    // scope, and may not pause it for everyone (LR-24).
    { id: 3, name: "platform-job", type: "bash", scope: null, source: "git", status: "idle", schedule: "0 2 * * *", canRun: true, canKill: false, canPause: false },
    { id: 4, name: "tax-nightly", type: "bash", scope: "tax", source: "git", status: "idle", schedule: "0 2 * * *", agencies: ["Tax"], canRun: true, canKill: true, canPause: true },
    // Stop without Pause: a department's own run of a Global job is theirs to stop.
    { id: 5, name: "platform-running", type: "bash", scope: null, source: "git", status: "running", schedule: "0 3 * * *", canRun: true, canKill: true, canPause: false },
    // A server that predates canPause: the row falls back to canKill, as before.
    { id: 6, name: "old-server-job", type: "bash", scope: "tax", source: "git", status: "idle", schedule: "0 4 * * *", agencies: ["Tax"], canRun: true, canKill: true },
  ],
  totalItems: 6,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/jobs") {
          await laterTick();
          return { data: JOBS };
        }
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      PATCH: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: false,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: false,
      // The union says yes — deliberately. The rows must still win.
      triggerJobs: true,
      killJobs: true,
    })),
  };
});

import { Jobs } from "./Jobs";
import { AuthProvider } from "../auth";

afterEach(cleanup);

const renderJobs = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Jobs />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("tax-job")).toBeTruthy());
  return q;
};

describe("Jobs — per-row authority overrides the flat capability (RB-24)", () => {
  it("offers Run on the row the server permits", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("tax-job").closest("tr")!);
    await waitFor(() => expect(q.getByRole("button", { name: /▶ Run/ })).toBeTruthy());
  });

  it("withholds Run on a row the server denies, despite triggerJobs=true", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("finance-job").closest("tr")!);
    // Positive control: the row expanded, so a missing button is a gate and not an
    // unrendered panel.
    await waitFor(() => expect(q.getByText("finance-job")).toBeTruthy());
    expect(q.queryByRole("button", { name: /▶ Run/ })).toBeNull();
  });

  it("renders the derived agency for each row (RB-23)", async () => {
    const q = await renderJobs();
    await waitFor(() => expect(q.getAllByText("Tax").length).toBeGreaterThan(0));
    expect(q.getByText("Finance")).toBeTruthy();
  });

  // Pause and Resume change the job for everyone, so they follow canPause, not
  // canKill: until 2.3.0 one flag served both, and a department's operator was
  // offered Pause on a job with no scope and refused on the click.
  it("offers Run but not Pause on a job with no scope the caller may only run", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("platform-job").closest("tr")!);
    await waitFor(() => expect(q.getByRole("button", { name: /▶ Run/ })).toBeTruthy());
    expect(q.queryByRole("button", { name: "Pause" })).toBeNull();
  });

  it("offers Pause where the server says the caller may pause", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("tax-nightly").closest("tr")!);
    await waitFor(() => expect(q.getByRole("button", { name: "Pause" })).toBeTruthy());
  });

  it("offers Stop without Pause on a running job the caller may only stop", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("platform-running").closest("tr")!);
    await waitFor(() => expect(q.getByRole("button", { name: /Stop run/ })).toBeTruthy());
    expect(q.queryByRole("button", { name: "Pause" })).toBeNull();
  });

  it("falls back to canKill for Pause when the row carries no canPause", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("old-server-job").closest("tr")!);
    await waitFor(() => expect(q.getByRole("button", { name: "Pause" })).toBeTruthy());
  });
});

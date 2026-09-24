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
  ],
  totalItems: 2,
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
    await waitFor(() => expect(q.getByText("Tax")).toBeTruthy());
    expect(q.getByText("Finance")).toBeTruthy();
  });
});

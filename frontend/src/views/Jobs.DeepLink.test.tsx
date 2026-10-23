// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

// 2.5.2 OP-3 — /jobs?job=<uid>, where a run's "Open job" lands: the job's
// folder opens on the page that holds it, its row is expanded, and the
// parameter goes so the page is the person's again. A uid this agency's list
// does not hold says so.

// Thirty jobs in one folder: the target sorts last, onto the folder's second
// page (25 a page), so landing on page one would not show it.
const filler = Array.from({ length: 29 }, (_, i) => ({
  id: 100 + i,
  uid: `u-${i}`,
  name: `job-${String(i).padStart(2, "0")}`,
  type: "bash",
  scope: "Prod",
  source: "git",
  status: "idle",
  schedule: "manual",
  sourcePath: `jobs/nightly/job-${String(i).padStart(2, "0")}.yml`,
}));
const JOBS = {
  items: [
    ...filler,
    { id: 999, uid: "u-target", name: "zz-target", type: "bash", scope: "Prod", source: "git", status: "idle", schedule: "manual", sourcePath: "jobs/nightly/zz-target.yml" },
    { id: 1, uid: "u-root", name: "root-job", type: "bash", scope: "Prod", source: "git", status: "idle", schedule: "manual", sourcePath: "jobs/root-job.yml" },
  ],
  totalItems: 31,
  totalPages: 1,
  page: 1,
  pageSize: 200,
};

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/jobs") return { data: JOBS };
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        if (path === "/jobs/{jobId}") return { data: undefined };
        if (path === "/runs") return { data: { items: [] } };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: false,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: false,
      triggerJobs: true,
      killJobs: true,
    })),
  };
});

import { Jobs } from "./Jobs";
import { AuthProvider } from "../auth";

function Where() {
  const loc = useLocation();
  return <span data-testid="where">{loc.search}</span>;
}

const renderAt = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <AuthProvider>
        <Where />
        <Jobs />
      </AuthProvider>
    </MemoryRouter>,
  );

beforeEach(() => localStorage.clear());
afterEach(cleanup);

describe("Jobs ?job= (2.5.2 OP-3)", () => {
  it("opens the job's folder on its page, expands its row, and drops the parameter", async () => {
    renderAt("/jobs?job=u-target&agency=ag-a");
    await waitFor(() => expect(document.getElementById("job-detail-999")).toBeTruthy());
    expect(screen.getByText("zz-target")).toBeTruthy();
    await waitFor(() => expect(new URLSearchParams(screen.getByTestId("where").textContent ?? "").get("job")).toBeNull());
    const search = new URLSearchParams(screen.getByTestId("where").textContent ?? "");
    expect(search.get("path")).toBe("nightly");
    expect(search.get("agency")).toBe("ag-a");
  });

  it("expands a job at the root without inventing a folder", async () => {
    renderAt("/jobs?job=u-root");
    await waitFor(() => expect(document.getElementById("job-detail-1")).toBeTruthy());
    // The address follows as a navigation (a transition), a moment after the row.
    await waitFor(() => expect(new URLSearchParams(screen.getByTestId("where").textContent ?? "").get("job")).toBeNull());
    expect(new URLSearchParams(screen.getByTestId("where").textContent ?? "").get("path")).toBeNull();
  });

  it("says so when the job is not in this agency's list", async () => {
    renderAt("/jobs?job=u-elsewhere");
    expect(await screen.findByText("That job is not in this agency's list.")).toBeTruthy();
    await waitFor(() => expect(new URLSearchParams(screen.getByTestId("where").textContent ?? "").get("job")).toBeNull());
  });
});

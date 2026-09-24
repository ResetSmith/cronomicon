// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// FX-2 — the pause/resume pair in the expanded row. What is pinned here is the
// asymmetry: `isScheduled` is a fair precondition for Pause and was never one for
// Resume. Pause state is a paused_jobs row that survives the schedule being
// removed, so a Resume button gated on the schedule leaves the job stuck paused
// with no way back short of curl.

const posts: { path: string; jobId: unknown }[] = [];

const JOBS = {
  items: [
    // The trap: paused, and its schedule is gone.
    { id: 1, name: "stuck-job", type: "bash", scope: "Prod", source: "git", status: "paused", schedule: null },
    // A scheduled job that is running normally — Pause is meaningful here.
    { id: 2, name: "scheduled-job", type: "bash", scope: "Prod", source: "git", status: "idle", schedule: "0 2 * * *" },
    // Manual-only and not paused: pausing suppresses nothing, so no Pause.
    { id: 3, name: "manual-job", type: "bash", scope: "Prod", source: "git", status: "idle", schedule: "manual" },
  ],
  totalItems: 3,
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
        if (path === "/jobs/{jobId}") return { data: undefined };
        if (path === "/runs") return { data: { items: [] } };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { params?: { path?: { jobId?: number } } }) => {
        posts.push({ path, jobId: opts?.params?.path?.jobId });
        return { data: {} };
      }),
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
      // RB-3: Run/Kill/Pause/Resume now gate on the server-computed capability
      // rather than a client-side role list. These cases are about an OPERATOR,
      // who holds the verb — the gating itself is covered separately.
      triggerJobs: true,
      killJobs: true,
    })),
  };
});

import { Jobs } from "./Jobs";
import { AuthProvider } from "../auth";

beforeEach(() => {
  posts.length = 0;
});
afterEach(cleanup);

// The action strip needs canRun, which comes from /me — hence the real
// AuthProvider over the mocked client rather than a stubbed context.
const renderJobs = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Jobs />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  fireEvent.change(q.getByPlaceholderText("Search jobs…"), { target: { value: "job" } });
  await waitFor(() => expect(q.getByText("stuck-job")).toBeTruthy());
  return q;
};

const expandRow = (q: ReturnType<typeof within>, name: string) => {
  fireEvent.click(q.getByText(name).closest("tr")!);
};

describe("Jobs — pause/resume gating (FX-2)", () => {
  it("offers Resume on a paused job whose schedule has been removed", async () => {
    const q = await renderJobs();
    expandRow(q, "stuck-job");

    const resume = await waitFor(() => q.getByRole("button", { name: "Resume" }));
    fireEvent.click(resume);

    await waitFor(() => expect(posts).toEqual([{ path: "/jobs/{jobId}/resume", jobId: 1 }]));
  });

  it("still offers Pause on a scheduled job, and only Pause", async () => {
    const q = await renderJobs();
    expandRow(q, "scheduled-job");

    await waitFor(() => expect(q.getByRole("button", { name: "Pause" })).toBeTruthy());
    expect(q.queryByRole("button", { name: "Resume" })).toBeNull();
  });

  // The precondition Resume lost is the one Pause keeps: there is nothing to
  // suppress on a manual-only job that is not paused.
  it("offers neither on an unpaused manual-only job", async () => {
    const q = await renderJobs();
    expandRow(q, "manual-job");

    await waitFor(() => expect(q.getByRole("button", { name: /▶ Run/ })).toBeTruthy());
    expect(q.queryByRole("button", { name: "Pause" })).toBeNull();
    expect(q.queryByRole("button", { name: "Resume" })).toBeNull();
  });
});

// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The row action strip while a run is active: Run and Stop are INDEPENDENT
// gates, not a swap. Overlapping runs are legal under the Allow policy (the
// default), so an active run must not hide the Run button — the old ternary
// made the UI stricter than the job's own concurrency policy. Stop, on the
// other hand, only makes sense while an instance is actually active.

const JOBS = {
  items: [
    { id: 7, name: "busy-job", type: "bash", scope: "Prod", source: "git", status: "running", schedule: null },
    { id: 8, name: "idle-job", type: "bash", scope: "Prod", source: "git", status: "success", schedule: null },
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
  await waitFor(() => expect(q.getByText("busy-job")).toBeTruthy());
  return q;
};

const expandRow = async (q: ReturnType<typeof within>, name: string) => {
  fireEvent.click(q.getByText(name).closest("tr")!);
  await waitFor(() => expect(q.getByRole("button", { name: /▶ Run/ })).toBeTruthy());
};

describe("Jobs — Run stays available while a run is active", () => {
  it("shows BOTH Run and Stop on a running job's action strip", async () => {
    const q = await renderJobs();
    await expandRow(q, "busy-job");

    expect(q.getByRole("button", { name: /▶ Run/ })).toBeTruthy();
    expect(q.getByRole("button", { name: "Stop run…" })).toBeTruthy();
  });

  it("shows Run but NOT Stop on an idle job", async () => {
    const q = await renderJobs();
    await expandRow(q, "idle-job");

    expect(q.getByRole("button", { name: /▶ Run/ })).toBeTruthy();
    expect(q.queryByRole("button", { name: "Stop run…" })).toBeNull();
  });

  it("opens the Run dialog from a running job's Run button", async () => {
    const q = await renderJobs();
    await expandRow(q, "busy-job");

    fireEvent.click(q.getByRole("button", { name: /▶ Run/ }));
    // The Run dialog names the job it is about to launch.
    await waitFor(() => expect(q.getByText(/Run busy-job/)).toBeTruthy());
  });
});

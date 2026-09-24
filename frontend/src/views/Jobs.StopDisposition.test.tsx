// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RX-4 (Phase A) — the Stop dialog's disposition.
//
// What is pinned here is the WIRE, not the styling: which body reaches
// POST /jobs/{jobId}/kill for each choice. The server treats a missing body as
// the unclassified `killed`, so "no body" and `{"outcome":"killed"}` are the
// same request — but a body that says something ELSE is a different act, and a
// dialog that silently dropped it would turn "record this as done" into "record
// this as stopped" with no visible failure.

const posts: { path: string; jobId: unknown; body: unknown }[] = [];

const JOBS = {
  items: [{ id: 7, name: "wedged-job", type: "bash", scope: "Prod", source: "git", status: "running", schedule: null }],
  totalItems: 1,
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
      POST: vi.fn(async (path: string, opts: { params?: { path?: { jobId?: number } }; body?: unknown }) => {
        posts.push({ path, jobId: opts?.params?.path?.jobId, body: opts?.body });
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

const renderJobs = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Jobs />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("wedged-job")).toBeTruthy());
  return q;
};

// The dialog renders in a portal-less Modal inside the same tree, but the row
// must be expanded first to reach the action strip.
const openStopDialog = async (q: ReturnType<typeof within>) => {
  fireEvent.click(q.getByText("wedged-job").closest("tr")!);
  const stop = await waitFor(() => q.getByRole("button", { name: "Stop run…" }));
  fireEvent.click(stop);
  await waitFor(() => expect(q.getByText(/RECORD THIS RUN AS/)).toBeTruthy());
};

describe("Jobs — stop dispositions (RX-4)", () => {
  it("defaults to the unclassified stop, preserving pre-disposition behaviour", async () => {
    const q = await renderJobs();
    await openStopDialog(q);

    fireEvent.click(q.getByRole("button", { name: "Stop latest" }));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0].path).toBe("/jobs/{jobId}/kill");
    expect(posts[0].jobId).toBe(7);
    // An operator who just wants the run to end must not have to engage with the
    // choice at all — and what they send is exactly what a pre-RX client sent.
    expect(posts[0].body).toEqual({ outcome: "killed" });
  });

  it("sends the chosen disposition when the operator records an outcome", async () => {
    const q = await renderJobs();
    await openStopDialog(q);

    // "The work was actually done; the process is just wedged."
    fireEvent.click(q.getByRole("button", { name: "Success" }));
    fireEvent.click(q.getByRole("button", { name: "Stop latest" }));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0].body).toEqual({ outcome: "success" });
  });

  it("offers every disposition in the shared vocabulary", async () => {
    const q = await renderJobs();
    await openStopDialog(q);

    for (const label of ["Stopped", "Failed", "Success", "Warning"]) {
      expect(q.getByRole("button", { name: label })).toBeTruthy();
    }
  });

  it("sends no disposition for Stop all queued", async () => {
    const q = await renderJobs();
    await openStopDialog(q);

    fireEvent.click(q.getByRole("button", { name: "Success" }));
    fireEvent.click(q.getByRole("button", { name: "Stop all queued" }));

    await waitFor(() => expect(posts.length).toBeGreaterThan(0));
    // Clearing a stacked queue stops runs that never started, so there is no
    // work whose outcome anyone could assert. Carrying the dialog's selection
    // into the loop would record a success for a run that never executed.
    for (const p of posts) {
      expect(p.path).toBe("/jobs/{jobId}/kill");
      expect(p.body).toBeUndefined();
    }
  });
});

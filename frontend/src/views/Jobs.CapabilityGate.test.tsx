// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// RB-3 — Run/Kill/Pause/Resume gate on the SERVER-computed `triggerJobs`
// capability, not on a client-side role list.
//
// The behavior is identical today (admin|approver|operator hold the verb, viewer
// does not), so this file is not about who sees the button. It pins the SOURCE of
// the answer, which is what Phase 1 changes: once roles are DB rows (RB-6/RB-7) a
// custom role carrying triggerJobs is not in any hardcoded set, and a client-side
// list would hide the button from someone the API would happily serve. The
// capability flag is derived from whatever the matrix actually says.
//
// Deliberately decoupled from /me roles: the mock below reports a VIEWER identity
// while the capability says the verb is held. A component still reading roles
// fails here; one reading capabilities passes. That divergence is the whole test.

const JOBS = {
  items: [
    { id: 1, name: "gated-job", type: "bash", scope: "Prod", source: "git", status: "idle", schedule: "0 2 * * *" },
  ],
  totalItems: 1,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

let capTriggerJobs = true;

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
        // A viewer identity on purpose — see the note above.
        if (path === "/me") return { data: { email: "v@ex.com", roles: ["viewer"] } };
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
      triggerJobs: capTriggerJobs,
      killJobs: capTriggerJobs,
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
  await waitFor(() => expect(q.getByText("gated-job")).toBeTruthy());
  fireEvent.click(q.getByText("gated-job").closest("tr")!);
  return q;
};

describe("Jobs — Run/Pause gate reads the capability, not the role (RB-3)", () => {
  it("shows Run when the capability grants it, even though /me says viewer", async () => {
    capTriggerJobs = true;
    const q = await renderJobs();
    // Positive control: the expanded panel rendered.
    await waitFor(() => expect(q.getByRole("button", { name: /▶ Run/ })).toBeTruthy());
  });

  it("hides Run when the capability withholds it", async () => {
    capTriggerJobs = false;
    const q = await renderJobs();
    // Positive control: the row expanded, so a missing button is a gate and not
    // an unrendered panel.
    await waitFor(() => expect(q.getByText("gated-job")).toBeTruthy());
    expect(q.queryByRole("button", { name: /▶ Run/ })).toBeNull();
  });
});

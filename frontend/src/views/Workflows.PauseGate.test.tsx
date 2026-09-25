// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// FX-8 — Workflow Pause/Resume was the only action in the row with no gate at
// all, while Run (canRun) and Edit/Delete (compose + source) both had one. Same
// file, second half of the finding: one flag rendered under three different
// words, so an operator scanning for the state the button names never found it.

let roles: string[] = ["operator"];
const patches: { workflowId: unknown; body: unknown }[] = [];

// Mirrors the backend permission matrix: triggerJobs/killJobs are held by
// admin|approver|operator and not by viewer (auth.BuiltinRoles).
const holdsTriggerVerb = () =>
  roles.some((r) => ["admin", "approver", "operator"].includes(r.toLowerCase()));

const WORKFLOWS = [
  { id: 1, name: "paused-wf", source: "cronomicon", status: "success", disabled: true, steps: [] },
  { id: 2, name: "live-wf", source: "git", status: "success", disabled: false, steps: [] },
];

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/workflows") {
          await laterTick();
          return { data: WORKFLOWS };
        }
        if (path === "/me") return { data: { email: "op@ex.com", roles } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      PATCH: vi.fn(async (_path: string, opts: { params?: { path?: { workflowId?: number } }; body?: unknown }) => {
        patches.push({ workflowId: opts?.params?.path?.workflowId, body: opts?.body });
        return { data: {} };
      }),
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
      // RB-3: the Pause/Run gate reads the server-computed capability rather than
      // a client-side role list. Derive it from the same mutable `roles` the /me
      // mock serves, so this mock models what the backend actually computes and
      // each case keeps setting one variable.
      triggerJobs: holdsTriggerVerb(),
      killJobs: holdsTriggerVerb(),
    })),
  };
});

import { Workflows } from "./Workflows";
import { AuthProvider } from "../auth";

beforeEach(() => {
  roles = ["operator"];
  patches.length = 0;
});
afterEach(cleanup);

const renderWorkflows = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Workflows />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  fireEvent.change(q.getByPlaceholderText("Search workflows…"), { target: { value: "wf" } });
  await waitFor(() => expect(q.getByText("live-wf")).toBeTruthy());
  return q;
};

const expandRow = (q: ReturnType<typeof within>, name: string) => {
  fireEvent.click(q.getByText(name).closest("tr")!);
};

describe("Workflows — pause/resume gate (FX-8)", () => {
  it("offers Pause to an operator and sends the toggle", async () => {
    const q = await renderWorkflows();
    expandRow(q, "live-wf");

    const pause = await waitFor(() => q.getByRole("button", { name: "Pause" }));
    fireEvent.click(pause);

    await waitFor(() => expect(patches).toEqual([{ workflowId: 2, body: { disabled: true } }]));
  });

  // Not a security boundary — PATCH /workflows/{id} enforces no role today — but
  // the same UX gate its sibling actions carry: a Viewer is not offered controls
  // the role matrix says are not theirs.
  it("hides it from a viewer, the way Run is hidden", async () => {
    roles = ["viewer"];
    const q = await renderWorkflows();
    expandRow(q, "live-wf");

    // Positive control: the panel is open (the "Recent runs" section is
    // unconditional in it).
    await waitFor(() => expect(q.getByText("Recent runs")).toBeTruthy());
    expect(q.queryByRole("button", { name: "Pause" })).toBeNull();
    expect(q.queryByRole("button", { name: /▶ Run/ })).toBeNull();
  });

  // A git-source workflow CAN be paused — PATCH, unlike DELETE, has no source
  // guard — so the button must not inherit Edit/Delete's cronomicon-only gate.
  it("keeps it on a git-source workflow", async () => {
    const q = await renderWorkflows();
    expandRow(q, "live-wf"); // source: git

    await waitFor(() => expect(q.getByRole("button", { name: "Pause" })).toBeTruthy());
  });

  it("says 'Paused' everywhere the disabled flag surfaces", async () => {
    const q = await renderWorkflows();

    // The name-column badge used to read "disabled" while the Result column read
    // "Paused" and the button read "Resume" — one flag, three words.
    const row = q.getByText("paused-wf").closest("tr")!;
    expect(within(row).getAllByText("Paused").length).toBe(2);
    expect(within(row).queryByText("disabled")).toBeNull();

    expandRow(q, "paused-wf");
    await waitFor(() => expect(q.getByRole("button", { name: "Resume" })).toBeTruthy());
  });
});

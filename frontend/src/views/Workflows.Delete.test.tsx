// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The expanded-panel Delete action, mirroring Jobs.Delete.test.tsx: the gate
// (compose capability AND an cronomicon source), the confirm as the commit point,
// and the 409 the server uses to protect git-synced rows being rendered as a
// sentence rather than a status code.

let composeOn = true;
let deleteStatus = 200;
const deletes: { path: string; workflowId: unknown }[] = [];

const WORKFLOWS = [
  { id: 1, name: "cronomicon-wf", source: "cronomicon", status: "success", steps: [] },
  { id: 2, name: "git-wf", source: "git", status: "success", steps: [] },
];

// The list fetch resolves a macrotask late while fetchCapabilities resolves on a
// microtask, and the capability effect is declared first. By the time a row is on
// screen canCompose has been applied — which is what makes asserting the ABSENCE
// of the Delete button sound rather than a race.
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
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      PATCH: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async (path: string, opts: { params?: { path?: { workflowId?: number } } }) => {
        deletes.push({ path, workflowId: opts?.params?.path?.workflowId });
        return deleteStatus === 200
          ? { data: {}, response: { ok: true, status: 200 } }
          : { error: { message: "workflow is git-managed" }, response: { ok: false, status: deleteStatus } };
      }),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: composeOn,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: false,
    })),
  };
});

import { Workflows } from "./Workflows";

beforeEach(() => {
  composeOn = true;
  deleteStatus = 200;
  deletes.length = 0;
});
afterEach(cleanup);

const renderWorkflows = async () => {
  const { container } = render(
    <MemoryRouter>
      <Workflows />
    </MemoryRouter>,
  );
  const q = within(container);
  // Search mode gives the flat results table; the folder browser is irrelevant here.
  fireEvent.change(q.getByPlaceholderText("Search workflows…"), { target: { value: "wf" } });
  await waitFor(() => expect(q.getByText("cronomicon-wf")).toBeTruthy());
  return q;
};

const expandRow = (q: ReturnType<typeof within>, name: string) => {
  const row = q.getByText(name).closest("tr");
  fireEvent.click(row!);
};

const deleteBtn = (q: ReturnType<typeof within>) => q.queryByRole("button", { name: "Delete" });
// The expanded panel is what carries Delete; the "Recent runs" section (the
// embedded per-workflow runs table) is always in it, so it is the marker that
// the panel has actually rendered.
const panelOpen = (q: ReturnType<typeof within>) => q.getByText("Recent runs");

describe("Workflows — expanded-row Delete", () => {
  it("offers Delete on an cronomicon row when the caller may compose", async () => {
    const q = await renderWorkflows();
    expandRow(q, "cronomicon-wf");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());
  });

  it("hides Delete on a git row even with the compose capability", async () => {
    const q = await renderWorkflows();
    expandRow(q, "git-wf");
    await waitFor(() => expect(panelOpen(q)).toBeTruthy());
    expect(deleteBtn(q)).toBeNull();
    // Same gate as Edit, the shipped precedent for cronomicon-only actions. Scoped
    // to this row: Edit lives in the always-visible actions cell, so the OTHER
    // (cronomicon) row legitimately still shows one.
    expect(within(q.getByText("git-wf").closest("tr")!).queryByRole("link", { name: "Edit" })).toBeNull();
  });

  it("hides Delete when the compose capability is off", async () => {
    composeOn = false;
    const q = await renderWorkflows();
    expandRow(q, "cronomicon-wf");
    await waitFor(() => expect(panelOpen(q)).toBeTruthy());
    expect(deleteBtn(q)).toBeNull();
  });

  // I-1 (VF-11) — the toolbar's "+ Create" was the one create affordance on this
  // page that was NOT capability-gated, while the empty state's "Create a
  // workflow" was. A Viewer saw a primary button that led straight to the
  // editor's "requires the Compose capability" notice.
  it("gates the toolbar + Create on the same capability as every other create action", async () => {
    const on = await renderWorkflows();
    await waitFor(() => expect(on.getByRole("link", { name: "+ Create" })).toBeTruthy());
    cleanup();

    composeOn = false;
    const off = await renderWorkflows();
    await waitFor(() => expect(off.queryByText("git-wf")).toBeTruthy());
    expect(off.queryByRole("link", { name: "+ Create" })).toBeNull();
  });

  it("confirms before deleting, then DELETEs the expanded workflow's id", async () => {
    const q = await renderWorkflows();
    expandRow(q, "cronomicon-wf");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());

    fireEvent.click(deleteBtn(q)!);
    expect(deletes).toHaveLength(0);
    expect(q.getByText(/This cannot be undone/)).toBeTruthy();

    fireEvent.click(q.getByRole("button", { name: "Delete Workflow" }));

    await waitFor(() => expect(deletes).toEqual([{ path: "/workflows/{workflowId}", workflowId: 1 }]));
    await waitFor(() => expect(q.getByText('Workflow "cronomicon-wf" deleted.')).toBeTruthy());
  });

  it("cancelling the confirm sends nothing", async () => {
    const q = await renderWorkflows();
    expandRow(q, "cronomicon-wf");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());

    fireEvent.click(deleteBtn(q)!);
    fireEvent.click(q.getByRole("button", { name: "Cancel" }));

    expect(deletes).toHaveLength(0);
    expect(q.queryByText(/This cannot be undone/)).toBeNull();
  });

  it("turns a 409 into the cronomicon-only explanation", async () => {
    deleteStatus = 409;
    const q = await renderWorkflows();
    expandRow(q, "cronomicon-wf");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());

    fireEvent.click(deleteBtn(q)!);
    fireEvent.click(q.getByRole("button", { name: "Delete Workflow" }));

    await waitFor(() =>
      expect(q.getByText(/Only cronomicon-source workflows can be deleted in-app\./)).toBeTruthy(),
    );
    expect(q.queryByText(/workflow is git-managed/)).toBeNull();
    expect(q.queryByText('Workflow "cronomicon-wf" deleted.')).toBeNull();
  });
});

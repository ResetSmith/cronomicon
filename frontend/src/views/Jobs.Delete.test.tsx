// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// The expanded-row Delete action. What is worth pinning is the GATE (an operator
// must never be offered a delete the server will refuse) and the two outcomes:
// the DELETE goes to the right id, and the 409 the server uses to protect
// git-synced rows is translated into a sentence rather than a status code.

// `compose` is the capability under test; `publishSchedule` is a second flag set
// in the SAME .then() as compose, so the "+ Publish to GitLab" button doubles as
// a positive control that the capability fetch has landed before a test asserts
// the ABSENCE of Delete.
let composeOn = true;
// What DELETE /jobs/{jobId} answers with. 409 is the server's git-source guard.
let deleteStatus = 200;
const deletes: { path: string; jobId: unknown }[] = [];

const JOBS = {
  items: [
    { id: 1, name: "cronomicon-job", type: "bash", scope: "Prod", source: "cronomicon", status: "success" },
    { id: 2, name: "git-job", type: "bash", scope: "Prod", source: "git", status: "success" },
  ],
  totalItems: 2,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

// The list fetch resolves a macrotask late while fetchCapabilities resolves on a
// microtask. That ordering is what makes the negative tests sound: by the time a
// row is on screen, canCompose has already been applied.
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
        // JobDetail's own re-fetch: undefined ⇒ it falls back to the list row.
        if (path === "/jobs/{jobId}") return { data: undefined };
        if (path === "/runs") return { data: { items: [] } };
        if (path === "/job-reference-bindings/{jobId}") return { data: { bindings: [] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async (path: string, opts: { params?: { path?: { jobId?: number } } }) => {
        deletes.push({ path, jobId: opts?.params?.path?.jobId });
        return deleteStatus === 200
          ? { data: {}, response: { ok: true, status: 200 } }
          : { error: { message: "job is git-managed" }, response: { ok: false, status: deleteStatus } };
      }),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: composeOn,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: true,
    })),
  };
});

import { Jobs } from "./Jobs";

beforeEach(() => {
  composeOn = true;
  deleteStatus = 200;
  deletes.length = 0;
});
afterEach(cleanup);

// Queries are scoped to this render's container rather than document.body, which
// keeps a second render in the same file from colliding.
const renderJobs = async () => {
  const { container } = render(
    <MemoryRouter>
      <Jobs />
    </MemoryRouter>,
  );
  const q = within(container);
  // Type into the search box to leave browse mode: the flat results table is the
  // same renderJobRow, minus the folder tree that has nothing to do with delete.
  fireEvent.change(q.getByPlaceholderText("Search jobs…"), { target: { value: "job" } });
  await waitFor(() => expect(q.getByText("cronomicon-job")).toBeTruthy());
  return q;
};

const expandRow = (q: ReturnType<typeof within>, name: string) => {
  const row = q.getByText(name).closest("tr");
  fireEvent.click(row!);
};

const deleteBtn = (q: ReturnType<typeof within>) => q.queryByRole("button", { name: "Delete" });

describe("Jobs — expanded-row Delete", () => {
  it("offers Delete on an cronomicon row when the caller may compose", async () => {
    const q = await renderJobs();
    expandRow(q, "cronomicon-job");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());
  });

  // The server refuses a git-source delete with a 409; offering the button would
  // be offering a dead end, so the row action is gated on source too.
  it("hides Delete on a git row even with the compose capability", async () => {
    const q = await renderJobs();
    expandRow(q, "git-job");
    // The whole action strip is gated: canRun is false here (no session), so a
    // git row expands to detail with no actions at all.
    await waitFor(() => expect(q.getByText("Executor")).toBeTruthy());
    expect(deleteBtn(q)).toBeNull();
    expect(q.queryByRole("link", { name: "Edit" })).toBeNull();
  });

  it("hides Delete when the compose capability is off", async () => {
    composeOn = false;
    const q = await renderJobs();
    // Positive control: publishSchedule rides the same capability .then(), so its
    // button proves the fetch landed and compose was genuinely off.
    await waitFor(() => expect(q.getByRole("link", { name: /Publish to GitLab/ })).toBeTruthy());
    expandRow(q, "cronomicon-job");
    await waitFor(() => expect(q.getByText("Executor")).toBeTruthy());
    expect(deleteBtn(q)).toBeNull();
  });

  // I-1 (VF-11) — the toolbar's "+ Create" was the one create affordance on this
  // page that was NOT capability-gated, while the empty state's "Create a job"
  // was. A Viewer saw a primary button that led straight to JobComposer's
  // "requires the Compose capability" notice.
  it("gates the toolbar + Create on the same capability as every other create action", async () => {
    const on = await renderJobs();
    await waitFor(() => expect(on.getByRole("link", { name: "+ Create" })).toBeTruthy());
    cleanup();

    composeOn = false;
    const off = await renderJobs();
    // Positive control, as above: Publish to GitLab rides the same capability
    // fetch, so its presence proves compose was genuinely off rather than unread.
    await waitFor(() => expect(off.getByRole("link", { name: /Publish to GitLab/ })).toBeTruthy());
    expect(off.queryByRole("link", { name: "+ Create" })).toBeNull();
  });

  it("confirms before deleting, then DELETEs the expanded job's id", async () => {
    const q = await renderJobs();
    expandRow(q, "cronomicon-job");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());

    fireEvent.click(deleteBtn(q)!);
    // Nothing is sent on the click alone — the confirm is the commit point.
    expect(deletes).toHaveLength(0);
    expect(q.getByText(/This cannot be undone/)).toBeTruthy();

    fireEvent.click(q.getByRole("button", { name: "Delete Job" }));

    await waitFor(() => expect(deletes).toEqual([{ path: "/jobs/{jobId}", jobId: 1 }]));
    await waitFor(() => expect(q.getByText('Job "cronomicon-job" deleted.')).toBeTruthy());
  });

  it("cancelling the confirm sends nothing", async () => {
    const q = await renderJobs();
    expandRow(q, "cronomicon-job");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());

    fireEvent.click(deleteBtn(q)!);
    fireEvent.click(q.getByRole("button", { name: "Cancel" }));

    expect(deletes).toHaveLength(0);
    expect(q.queryByText(/This cannot be undone/)).toBeNull();
  });

  // The 409 is the one failure an operator can act on, so it must read as a
  // sentence about sources — not "Delete failed (409)".
  it("turns a 409 into the cronomicon-only explanation", async () => {
    deleteStatus = 409;
    const q = await renderJobs();
    expandRow(q, "cronomicon-job");
    await waitFor(() => expect(deleteBtn(q)).toBeTruthy());

    fireEvent.click(deleteBtn(q)!);
    fireEvent.click(q.getByRole("button", { name: "Delete Job" }));

    await waitFor(() =>
      expect(q.getByText(/Only cronomicon-source jobs can be deleted in-app\./)).toBeTruthy(),
    );
    // The server's raw message must not be what the operator reads instead.
    expect(q.queryByText(/job is git-managed/)).toBeNull();
    // A failed delete is not announced as a success.
    expect(q.queryByText('Job "cronomicon-job" deleted.')).toBeNull();
  });
});

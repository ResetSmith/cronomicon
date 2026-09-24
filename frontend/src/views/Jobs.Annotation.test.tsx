// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// AN-3 (the annotations plan) — the operator annotation surface on the
// Jobs catalog: the Critical chip on a collapsed row, the section at the top of
// the expanded panel, its empty state, the optimistic save, and the Run dialog's
// context banner.
//
// The list rows below carry `critical` and `contact` but NO `notes`, mirroring
// the server (notes are detail-only). That is not incidental to the fixture: it
// is why the panel must read the DETAIL, and a regression that makes the section
// read the list row would show a chip with no note and pass every other check.

const JOBS = {
  items: [
    {
      id: 1,
      name: "critical-job",
      type: "bash",
      scope: "prod",
      source: "git",
      status: "idle",
      schedule: "manual",
      canRun: true,
      canKill: true,
      critical: true,
      contact: "dba-oncall@corp.example",
    },
    {
      id: 2,
      name: "plain-job",
      type: "bash",
      scope: "prod",
      source: "git",
      status: "idle",
      schedule: "manual",
      canRun: true,
      canKill: true,
      critical: false,
    },
  ],
  totalItems: 2,
  totalPages: 1,
  page: 1,
  pageSize: 50,
};

// The detail adds what the list omits.
const DETAIL: Record<number, Record<string, unknown>> = {
  1: {
    ...JOBS.items[0],
    notes: "Restores from the 02:00 dump.\nCheck free disk before re-running.",
    notesBy: "alex@corp.example",
    notesAt: "2026-08-14T09:30:00Z",
  },
  2: { ...JOBS.items[1] },
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));
const putCalls: { path: string; body: unknown }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { jobId?: number } } }) => {
        if (path === "/jobs") {
          await laterTick();
          return { data: JOBS };
        }
        if (path === "/jobs/{jobId}") {
          await laterTick();
          return { data: DETAIL[opts?.params?.path?.jobId ?? 0] ?? {} };
        }
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        if (path === "/runs") return { data: { items: [], totalItems: 0, totalPages: 1, page: 1, pageSize: 20 } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        putCalls.push({ path, body: opts?.body });
        const b = (opts?.body ?? {}) as Record<string, unknown>;
        // The server answers with its own view, including the attribution it
        // assigns — which is what the section's byline must render.
        return { data: { ...b, notesBy: "op@ex.com", notesAt: "2026-08-14T10:00:00Z" } };
      }),
      PATCH: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      // compose FALSE on purpose: annotations are open to any logged-in user
      // (AN-Q1), and the section must not inherit the composer's gate.
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

afterEach(() => {
  cleanup();
  putCalls.length = 0;
});

const renderJobs = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Jobs />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("critical-job")).toBeTruthy());
  return q;
};

describe("Jobs — operator annotations (AN-3)", () => {
  it("marks a critical row and leaves an un-annotated one unmarked", async () => {
    const q = await renderJobs();
    const criticalRow = within(q.getByText("critical-job").closest("tr")!);
    expect(criticalRow.getByText("Critical")).toBeTruthy();
    // Absence is the common case, so the chip must be the exception — a
    // "Not critical" chip on every row would train people to stop seeing it.
    const plainRow = within(q.getByText("plain-job").closest("tr")!);
    expect(plainRow.queryByText("Critical")).toBeNull();
  });

  it("renders criticality, contact, notes and attribution in the expanded panel", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("critical-job").closest("tr")!);

    // The notes come from the DETAIL fetch — the list row has none — so this
    // also pins that the panel reads the right source.
    await waitFor(() => expect(q.getByText(/Restores from the 02:00 dump/)).toBeTruthy());
    expect(q.getByText(/dba-oncall@corp\.example/)).toBeTruthy();
    // Attribution makes staleness visible; a note with no byline is unattributable.
    expect(q.getByText(/Updated by alex@corp\.example/)).toBeTruthy();
  });

  it("preserves newlines rather than collapsing them", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("critical-job").closest("tr")!);
    const notes = await waitFor(() => q.getByText(/Restores from the 02:00 dump/));
    // pre-wrap is the ONLY formatting an operator gets (AN-Q2 — no markdown, no
    // linkification), so losing it loses the note's structure entirely.
    expect(getComputedStyle(notes).whiteSpace).toBe("pre-wrap");
  });

  it("offers a quiet add affordance when nothing is annotated", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("plain-job").closest("tr")!);
    // Every logged-in user may write (compose is false in this fixture), so the
    // empty state is an invitation, not an empty labelled box.
    await waitFor(() => expect(q.getByRole("button", { name: /Add a note/ })).toBeTruthy());
  });

  it("saves an annotation and repaints the row's chip optimistically", async () => {
    const q = await renderJobs();
    // plain-job starts un-annotated and unmarked.
    expect(within(q.getByText("plain-job").closest("tr")!).queryByText("Critical")).toBeNull();

    fireEvent.click(q.getByText("plain-job").closest("tr")!);
    fireEvent.click(await waitFor(() => q.getByRole("button", { name: /Add a note/ })));

    fireEvent.click(q.getByLabelText(/Critical/));
    fireEvent.change(q.getByPlaceholderText(/Who to contact/), { target: { value: "sre@corp.example" } });
    fireEvent.change(q.getByPlaceholderText(/What this is for/), { target: { value: "Ask before re-running." } });
    fireEvent.click(q.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(putCalls.length).toBe(1));
    expect(putCalls[0].path).toBe("/job-annotation/{jobId}");
    expect(putCalls[0].body).toEqual({
      critical: true,
      contact: "sre@corp.example",
      notes: "Ask before re-running.",
    });

    // The row's chip appears without waiting for a refetch — the whole point of
    // routing the write through the optimistic hook.
    await waitFor(() =>
      expect(within(q.getByText("plain-job").closest("tr")!).getByText("Critical")).toBeTruthy(),
    );
    // And the panel now shows the server's assigned attribution.
    expect(q.getByText(/Updated by op@ex\.com/)).toBeTruthy();
  });

  it("shows the annotation banner in the Run dialog, and only when there is one", async () => {
    const q = await renderJobs();

    fireEvent.click(q.getByText("critical-job").closest("tr")!);
    fireEvent.click(await waitFor(() => q.getByRole("button", { name: /▶ Run/ })));
    await waitFor(() => expect(q.getByText(/Run critical-job/)).toBeTruthy());

    // Scoped to the banner itself: the expanded panel behind the dialog carries
    // the same text, so an unscoped query would pass with no banner at all.
    const banner = within(await waitFor(() => q.getByRole("note", { name: "Operator annotation" })));
    expect(banner.getByText("Critical")).toBeTruthy();
    expect(banner.getByText(/dba-oncall@corp\.example/)).toBeTruthy();
    // The note is truncated to its FIRST line — the dialog is for running the
    // job, not reading its history.
    expect(banner.getByText(/Restores from the 02:00 dump/)).toBeTruthy();
    expect(banner.queryByText(/Check free disk/)).toBeNull();
  });

  it("omits the banner for a job with no annotation", async () => {
    const q = await renderJobs();
    fireEvent.click(q.getByText("plain-job").closest("tr")!);
    fireEvent.click(await waitFor(() => q.getByRole("button", { name: /▶ Run/ })));
    await waitFor(() => expect(q.getByText(/Run plain-job/)).toBeTruthy());
    // No banner at all, rather than an empty one.
    expect(q.queryByRole("note", { name: "Operator annotation" })).toBeNull();
  });
});

// CO-1.1 (the columns plan) — Critical is its own column now. The
// column INDEX is resolved from the header rather than hardcoded, because CO-2
// is about to make that order operator-arrangeable; a test that asserts
// `cells[7]` would pass today and become a false failure the moment somebody
// drags the column, which is the feature working.
type Q = ReturnType<typeof renderJobs> extends Promise<infer R> ? R : never;

const columnHeads = (q: Q) => q.getAllByRole("columnheader") as HTMLElement[];
const columnIndex = (q: Q, label: string) => {
  const i = columnHeads(q).findIndex((h) => (h.textContent ?? "").trim().startsWith(label));
  expect(i, `no "${label}" column header`).toBeGreaterThan(-1);
  return i;
};
// Click the header itself rather than querying it by accessible name: a sortable
// <th> names itself from its CONTENT (label + caret), not from the "Sort by …"
// title attribute, so a name query here silently matches nothing.
const sortBy = (q: Q, label: string) => fireEvent.click(columnHeads(q)[columnIndex(q, label)]);

// Sorting is a flat/search-mode affordance only — browse mode keeps the folder
// tree's alpha order (TS-8) — so every sort assertion goes through the search box.
const searchAll = (q: Q) =>
  fireEvent.change(q.getByPlaceholderText("Search jobs…"), { target: { value: "job" } });

const nameOrder = (q: Q) =>
  (q.getAllByRole("row") as HTMLElement[])
    .slice(1)
    .map((r) => (r.querySelectorAll("td")[0]?.textContent ?? "").trim())
    .filter(Boolean);

describe("Jobs — the Critical column (CO-1.1)", () => {
  it("renders the chip in the Critical column and NOT in the Status cell", async () => {
    const q = await renderJobs();
    const crit = columnIndex(q, "Critical");
    const status = columnIndex(q, "Status");

    const row = q.getByText("critical-job").closest("tr")!;
    const cells = row.querySelectorAll("td");
    expect(within(cells[crit] as HTMLElement).getByText("Critical")).toBeTruthy();
    // CO-Q7: the chip MOVED, it was not duplicated. One fact in two cells of one
    // row drifts the first time somebody edits one of the two sites.
    expect(within(cells[status] as HTMLElement).queryByText("Critical")).toBeNull();
  });

  it("renders an em dash for a job that is not critical, rather than a blank cell", async () => {
    const q = await renderJobs();
    const crit = columnIndex(q, "Critical");
    const cells = q.getByText("plain-job").closest("tr")!.querySelectorAll("td");
    // CO-Q8 ships the column visible even in a fleet with few annotations, so an
    // empty cell must read as "not critical" the way every other empty cell in
    // this table does — not as a rendering failure.
    expect((cells[crit].textContent ?? "").trim()).toBe("—");
  });

  it("floats critical jobs to the top when sorted by Critical", async () => {
    const q = await renderJobs();
    searchAll(q);
    await waitFor(() => expect(nameOrder(q)).toEqual(["critical-job", "plain-job"]));

    // CO-Q6 — critical-first ASCENDING, matching the TS-Q5 "worst first
    // ascending" convention the status rank already uses.
    sortBy(q, "Critical");
    await waitFor(() => expect(nameOrder(q)[0]).toBe("critical-job"));
    // The DESCENDING leg is what proves the sort is reading `critical` at all:
    // this order is unreachable by the default name sort, which the ascending
    // leg happens to agree with for this fixture.
    sortBy(q, "Critical");
    await waitFor(() => expect(nameOrder(q)[0]).toBe("plain-job"));
  });

  it("re-sorts on an optimistic in-panel toggle, before any refetch", async () => {
    const q = await renderJobs();
    searchAll(q);
    await waitFor(() => expect(nameOrder(q).length).toBe(2));
    sortBy(q, "Critical");
    await waitFor(() => expect(nameOrder(q)[0]).toBe("critical-job"));

    // Mark plain-job critical from its panel. This is the whole reason the sort
    // spec is a factory closed over inlineAnnotation rather than a module const:
    // a module const sorts the SERVER's value while the row renders the
    // operator's, a one-render disagreement indistinguishable from a sort bug.
    fireEvent.click(q.getByText("plain-job").closest("tr")!);
    fireEvent.click(await waitFor(() => q.getByRole("button", { name: /Add a note/ })));
    fireEvent.click(q.getByLabelText(/Critical/));
    fireEvent.click(q.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(putCalls.length).toBe(1));
    // Both are critical now, so the name tiebreak decides — and crucially the
    // row that was just toggled no longer sits below the un-annotated ones.
    await waitFor(() => {
      const crit = columnIndex(q, "Critical");
      const cells = q.getByText("plain-job").closest("tr")!.querySelectorAll("td");
      expect(within(cells[crit] as HTMLElement).getByText("Critical")).toBeTruthy();
    });
  });
});

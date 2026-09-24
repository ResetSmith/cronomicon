// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// AN-3, the Workflows half. Written alongside the Jobs half deliberately: the
// recurring defect in this area is a change made to one catalog and not the
// other (the backend row struct's FX-D1 comment lists three in a row).
//
// Two things differ from the Jobs twin and are what this file is really for:
//
//  1. The expanded panel here renders from the LIST row, which carries no
//     notes — so the annotation section fetches the workflow's detail itself.
//     A regression that drops that fetch shows the chip and the contact and
//     silently loses the note.
//  2. This list is filtered CLIENT-side, so notes and contact join the search
//     predicate (AN-Q5). The Jobs catalog is server-paged and stays name-only.

const WORKFLOWS = [
  { id: 1, name: "release-train", source: "amadeus", status: "success", disabled: false, steps: [], critical: true, contact: "#platform" },
  { id: 2, name: "nightly-tidy", source: "git", status: "success", disabled: false, steps: [] },
];

// The detail adds what the list omits.
const DETAIL: Record<number, Record<string, unknown>> = {
  1: {
    ...WORKFLOWS[0],
    notes: "Freezes during month-end close.\nAsk before re-running.",
    notesBy: "sam@corp.example",
    notesAt: "2026-08-14T09:00:00Z",
  },
  2: { ...WORKFLOWS[1] },
};

const laterTick = () => new Promise((resolve) => setTimeout(resolve, 0));
const putCalls: { path: string; body: unknown }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { path?: { workflowId?: number } } }) => {
        if (path === "/workflows") {
          await laterTick();
          return { data: WORKFLOWS };
        }
        if (path === "/workflows/{workflowId}") {
          await laterTick();
          return { data: DETAIL[opts?.params?.path?.workflowId ?? 0] ?? {} };
        }
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        putCalls.push({ path, body: opts?.body });
        const b = (opts?.body ?? {}) as Record<string, unknown>;
        return { data: { ...b, notesBy: "op@ex.com", notesAt: "2026-08-14T10:00:00Z" } };
      }),
      PATCH: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      // compose FALSE: annotations are open to any logged-in user (AN-Q1).
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

import { Workflows } from "./Workflows";
import { AuthProvider } from "../auth";

afterEach(() => {
  cleanup();
  putCalls.length = 0;
});

const renderWorkflows = async () => {
  const { container } = render(
    <MemoryRouter>
      <AuthProvider>
        <Workflows />
      </AuthProvider>
    </MemoryRouter>,
  );
  const q = within(container);
  await waitFor(() => expect(q.getByText("release-train")).toBeTruthy());
  return q;
};

describe("Workflows — operator annotations (AN-3)", () => {
  it("marks a critical row and leaves an un-annotated one unmarked", async () => {
    const q = await renderWorkflows();
    expect(within(q.getByText("release-train").closest("tr")!).getByText("Critical")).toBeTruthy();
    expect(within(q.getByText("nightly-tidy").closest("tr")!).queryByText("Critical")).toBeNull();
  });

  it("reads notes from the workflow DETAIL, which the list row does not carry", async () => {
    const q = await renderWorkflows();
    fireEvent.click(q.getByText("release-train").closest("tr")!);
    // The fixture's list row has no `notes`, so this text can only have come
    // from the detail fetch the panel issues for itself.
    await waitFor(() => expect(q.getByText(/Freezes during month-end close/)).toBeTruthy());
    expect(q.getByText(/#platform/)).toBeTruthy();
    expect(q.getByText(/Updated by sam@corp\.example/)).toBeTruthy();
  });

  it("offers a quiet add affordance when nothing is annotated", async () => {
    const q = await renderWorkflows();
    fireEvent.click(q.getByText("nightly-tidy").closest("tr")!);
    await waitFor(() => expect(q.getByRole("button", { name: /Add a note/ })).toBeTruthy());
  });

  it("saves through the workflow route and repaints the chip optimistically", async () => {
    const q = await renderWorkflows();
    expect(within(q.getByText("nightly-tidy").closest("tr")!).queryByText("Critical")).toBeNull();

    fireEvent.click(q.getByText("nightly-tidy").closest("tr")!);
    fireEvent.click(await waitFor(() => q.getByRole("button", { name: /Add a note/ })));
    fireEvent.click(q.getByLabelText(/Critical/));
    fireEvent.change(q.getByPlaceholderText(/Who to contact/), { target: { value: "#sre" } });
    fireEvent.click(q.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(putCalls.length).toBe(1));
    expect(putCalls[0].path).toBe("/workflow-annotation/{workflowId}");
    expect(putCalls[0].body).toEqual({ critical: true, contact: "#sre", notes: "" });

    await waitFor(() =>
      expect(within(q.getByText("nightly-tidy").closest("tr")!).getByText("Critical")).toBeTruthy(),
    );
  });

  it("searches notes and contact, not just the name (AN-Q5)", async () => {
    const q = await renderWorkflows();
    const search = q.getByPlaceholderText(/Search/i);

    // Contact matches a row whose NAME does not.
    fireEvent.change(search, { target: { value: "#platform" } });
    await waitFor(() => expect(q.getByText("release-train")).toBeTruthy());
    expect(q.queryByText("nightly-tidy")).toBeNull();

    // A term that matches nothing anywhere still filters everything out — the
    // positive control that the box is doing the filtering.
    fireEvent.change(search, { target: { value: "no-such-term" } });
    await waitFor(() => expect(q.queryByText("release-train")).toBeNull());
  });
});

// CO-1.2 — the Workflows half of the Critical column. Written alongside the Jobs
// half for the reason this file's header already gives: the recurring defect in
// this area is a change made to one catalog and not the other.
type Q = ReturnType<typeof renderWorkflows> extends Promise<infer R> ? R : never;

const columnHeads = (q: Q) => q.getAllByRole("columnheader") as HTMLElement[];
const columnIndex = (q: Q, label: string) => {
  const i = columnHeads(q).findIndex((h) => (h.textContent ?? "").trim().startsWith(label));
  expect(i, `no "${label}" column header`).toBeGreaterThan(-1);
  return i;
};
// Click the header element, not a name query: a sortable <th> names itself from
// its content, never from the "Sort by …" title.
const sortBy = (q: Q, label: string) => fireEvent.click(columnHeads(q)[columnIndex(q, label)]);

// Sort is flat/search-mode only (TS-8); an empty box is browse mode.
const searchAll = (q: Q) =>
  fireEvent.change(q.getByPlaceholderText(/Search/i), { target: { value: "-" } });

const nameOrder = (q: Q) =>
  (q.getAllByRole("row") as HTMLElement[])
    .slice(1)
    .map((r) => (r.querySelectorAll("td")[0]?.textContent ?? "").trim())
    .filter(Boolean);

describe("Workflows — the Critical column (CO-1.2)", () => {
  it("renders the chip in the Critical column and NOT in the Result cell", async () => {
    const q = await renderWorkflows();
    const crit = columnIndex(q, "Critical");
    const result = columnIndex(q, "Result");

    const cells = q.getByText("release-train").closest("tr")!.querySelectorAll("td");
    expect(within(cells[crit] as HTMLElement).getByText("Critical")).toBeTruthy();
    // CO-Q7 — moved, not duplicated. Workflows has no Status column, so Result
    // is where the AN-3 adjacency argument lands.
    expect(within(cells[result] as HTMLElement).queryByText("Critical")).toBeNull();
  });

  it("renders an em dash for a workflow that is not critical", async () => {
    const q = await renderWorkflows();
    const crit = columnIndex(q, "Critical");
    const cells = q.getByText("nightly-tidy").closest("tr")!.querySelectorAll("td");
    expect((cells[crit].textContent ?? "").trim()).toBe("—");
  });

  it("floats critical workflows to the top when sorted by Critical", async () => {
    const q = await renderWorkflows();
    searchAll(q);
    await waitFor(() => expect(nameOrder(q).length).toBe(2));

    sortBy(q, "Critical");
    await waitFor(() => expect(nameOrder(q)[0]).toBe("release-train"));
    // The descending leg is the one the default name sort could never produce.
    sortBy(q, "Critical");
    await waitFor(() => expect(nameOrder(q)[0]).toBe("nightly-tidy"));
  });

  it("re-sorts on an optimistic in-panel toggle, before any refetch", async () => {
    const q = await renderWorkflows();
    searchAll(q);
    await waitFor(() => expect(nameOrder(q).length).toBe(2));
    sortBy(q, "Critical");
    await waitFor(() => expect(nameOrder(q)[0]).toBe("release-train"));

    fireEvent.click(q.getByText("nightly-tidy").closest("tr")!);
    fireEvent.click(await waitFor(() => q.getByRole("button", { name: /Add a note/ })));
    fireEvent.click(q.getByLabelText(/Critical/));
    fireEvent.click(q.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(putCalls.length).toBe(1));
    // The sort spec must read the OVERRIDE, not the server's stale row — which
    // is why wfSortCols is a factory closed over inlineAnnotation.
    await waitFor(() => {
      const crit = columnIndex(q, "Critical");
      const cells = q.getByText("nightly-tidy").closest("tr")!.querySelectorAll("td");
      expect(within(cells[crit] as HTMLElement).getByText("Critical")).toBeTruthy();
    });
  });
});

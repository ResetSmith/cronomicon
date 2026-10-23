// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// 2.5.2 OP4 — the run's acknowledgement and notes: the thread newest first
// with each author's label, about five in view and paged ten at a time,
// acknowledging (with the draft as its first note), adding a note, taking an
// acknowledgement back, and a draft that survives the thread being read again.

type Entry = { id: number; kind: string; body: string; author: string; relation: string; at: string };
let ENTRIES: Entry[] = [];
const { GET, POST } = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET, POST } as unknown as typeof actual.api };
});

import { RunNotesCell, RunNotesPanel, relationLabel } from "./RunNotes";

function state() {
  let acknowledged: { by: string; relation: string; at: string } | null = null;
  for (const e of ENTRIES) {
    if (e.kind === "acknowledged") acknowledged = { by: e.author, relation: e.relation, at: e.at };
    if (e.kind === "unacknowledged") acknowledged = null;
  }
  return { acknowledged, notes: ENTRIES };
}

beforeEach(() => {
  ENTRIES = [];
  GET.mockReset();
  POST.mockReset();
  GET.mockImplementation(async () => ({ data: state() }));
  POST.mockImplementation(async (_p: string, opts: { body: { kind: string; body: string } }) => {
    ENTRIES = [...ENTRIES, { id: ENTRIES.length + 1, kind: opts.body.kind, body: opts.body.body, author: "ops@example.gov", relation: "outside", at: "2026-10-22T07:05:00Z" }];
    return { data: state() };
  });
});
afterEach(cleanup);

describe("History's Notes column (2.5.2 OP-18)", () => {
  const cell = (notes: { acknowledged: boolean; count: number } | undefined, status: string) => {
    const { container } = render(<RunNotesCell notes={notes} status={status} />);
    const text = container.textContent ?? "";
    cleanup();
    return text;
  };
  it("a tick and the count on an acknowledged run", () => {
    expect(cell({ acknowledged: true, count: 3 }, "danger")).toBe("3Acknowledged");
  });
  it("the count on a run with notes nobody acknowledged", () => {
    expect(cell({ acknowledged: false, count: 1 }, "danger")).toBe("1 note");
  });
  it("Not acknowledged on a failed, stopped or lost run nobody has taken up", () => {
    expect(cell(undefined, "danger")).toBe("Not acknowledged");
    expect(cell(undefined, "killed")).toBe("Not acknowledged");
  });
  it("nothing on a run that needs nothing", () => {
    expect(cell(undefined, "success")).toBe("");
    expect(cell(undefined, "running")).toBe("");
  });
});

describe("the run notes panel (2.5.2 OP4)", () => {
  it("labels each author by how they stood to the run's agency (OP-14)", () => {
    expect(relationLabel("member", "Payroll", "Global")).toBe("Payroll");
    expect(relationLabel("global_admin", "Payroll", "Global")).toBe("Global administrator");
    expect(relationLabel("outside", "Payroll", "Central")).toBe("Central");
  });

  it("shows the thread newest first, the state, and the labels", async () => {
    ENTRIES = [
      { id: 1, kind: "acknowledged", body: "Looking at it.", author: "j.ortiz@example.gov", relation: "outside", at: "2026-10-22T02:11:00Z" },
      { id: 2, kind: "note", body: "The bank added a header row.", author: "a.novak@example.gov", relation: "member", at: "2026-10-22T06:15:00Z" },
    ];
    render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck />);
    const items = await screen.findAllByTestId("run-note");
    expect(items).toHaveLength(2);
    expect(items[0].textContent).toContain("a.novak@example.gov · Payroll");
    expect(items[1].textContent).toContain("j.ortiz@example.gov · Global");
    expect(items[1].textContent).toContain("acknowledged");
    expect(screen.getByTestId("run-ack-state").textContent).toContain("Acknowledged by j.ortiz@example.gov · Global");
    expect(screen.getByRole("button", { name: "Remove acknowledgement" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Acknowledge" })).toBeNull();
  });

  it("acknowledges with the draft as its first note, and clears the draft", async () => {
    render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck />);
    await screen.findByText("No notes on this run yet.");
    const box = screen.getByLabelText("Add a note") as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "  Looking at it.  " } });
    fireEvent.click(screen.getByRole("button", { name: "Acknowledge" }));
    await waitFor(() => expect(POST).toHaveBeenCalled());
    const [path, opts] = POST.mock.calls[0] as [string, { params: { path: { traceId: string } }; body: { kind: string; body: string } }];
    expect(path).toBe("/runs/{traceId}/notes");
    expect(opts.params.path.traceId).toBe("t-1");
    expect(opts.body).toEqual({ kind: "acknowledged", body: "Looking at it." });
    await waitFor(() => expect(screen.getByTestId("run-ack-state")).toBeTruthy());
    expect(box.value).toBe("");
  });

  it("adds a note only when there is text", async () => {
    render(<RunNotesPanel kind="workflow" traceId="wt-1" agencyId="Payroll" offerAck={false} />);
    await screen.findByText("No notes on this run yet.");
    const add = screen.getByRole("button", { name: "Add note" }) as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Add a note"), { target: { value: "Called on-call." } });
    expect(add.disabled).toBe(false);
    fireEvent.click(add);
    await waitFor(() => expect(POST).toHaveBeenCalled());
    expect(POST.mock.calls[0][0]).toBe("/workflow-runs/{traceId}/notes");
    expect((POST.mock.calls[0][1] as { body: { kind: string } }).body.kind).toBe("note");
    expect(await screen.findByText("Called on-call.")).toBeTruthy();
  });

  it("offers no Acknowledge where it means nothing (a run that succeeded)", async () => {
    render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck={false} />);
    await screen.findByText("No notes on this run yet.");
    expect(screen.queryByRole("button", { name: "Acknowledge" })).toBeNull();
    expect(screen.getByRole("button", { name: "Add note" })).toBeTruthy();
  });

  it("takes an acknowledgement back, and a draft survives the thread being read again", async () => {
    ENTRIES = [{ id: 1, kind: "acknowledged", body: "", author: "j.ortiz@example.gov", relation: "outside", at: "2026-10-22T02:11:00Z" }];
    render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck />);
    await screen.findByTestId("run-ack-state");
    fireEvent.change(screen.getByLabelText("Add a note"), { target: { value: "half-written" } });
    fireEvent.click(screen.getByRole("button", { name: "Remove acknowledgement" }));
    await waitFor(() => expect(screen.queryByTestId("run-ack-state")).toBeNull());
    expect((POST.mock.calls[0][1] as { body: { kind: string } }).body.kind).toBe("unacknowledged");
    expect((screen.getByLabelText("Add a note") as HTMLTextAreaElement).value).toBe("half-written");
    const newest = screen.getAllByTestId("run-note")[0];
    expect(within(newest).getByText("acknowledgement removed")).toBeTruthy();
  });

  it("keeps about five entries in view and scrolls the rest", async () => {
    ENTRIES = notes(7);
    render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck={false} />);
    expect(await screen.findAllByTestId("run-note")).toHaveLength(7);
    const box = screen.getByTestId("run-notes-list");
    expect(box.style.overflowY).toBe("auto");
    expect(box.style.maxHeight).toBe("280px");
    expect(screen.queryByRole("button", { name: "Next →" })).toBeNull();
  });

  it("pages ten at a time past ten, newest on the first page", async () => {
    ENTRIES = notes(10);
    const { unmount } = render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck={false} />);
    expect(await screen.findAllByTestId("run-note")).toHaveLength(10);
    expect(screen.queryByRole("button", { name: "Next →" })).toBeNull();
    unmount();

    ENTRIES = notes(23);
    render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck={false} />);
    let items = await screen.findAllByTestId("run-note");
    expect(items).toHaveLength(10);
    expect(items[0].textContent).toContain("Note 23");
    expect(screen.getByText("1–10 of 23 notes")).toBeTruthy();
    expect(screen.queryByRole("group", { name: "Rows per page" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Next →" }));
    fireEvent.click(screen.getByRole("button", { name: "Next →" }));
    items = screen.getAllByTestId("run-note");
    expect(items).toHaveLength(3);
    expect(items[0].textContent).toContain("Note 3");
    expect(items[2].textContent).toContain("Note 1");
    expect(screen.getByText("21–23 of 23 notes")).toBeTruthy();
  });

  it("returns to the first page after adding a note, so the new note shows on top", async () => {
    ENTRIES = notes(12);
    render(<RunNotesPanel kind="run" traceId="t-1" agencyId="Payroll" offerAck={false} />);
    await screen.findAllByTestId("run-note");
    fireEvent.click(screen.getByRole("button", { name: "Next →" }));
    expect(screen.getAllByTestId("run-note")).toHaveLength(2);
    fireEvent.change(screen.getByLabelText("Add a note"), { target: { value: "Rerun at 08:00." } });
    fireEvent.click(screen.getByRole("button", { name: "Add note" }));
    await waitFor(() => expect(screen.getAllByTestId("run-note")[0].textContent).toContain("Rerun at 08:00."));
    expect(screen.getAllByTestId("run-note")).toHaveLength(10);
    expect(screen.getByText("1–10 of 13 notes")).toBeTruthy();
  });
});

/** n plain notes, oldest first as the server sends them: "Note 1" … "Note n". */
function notes(n: number): Entry[] {
  return Array.from({ length: n }, (_, i) => ({
    id: i + 1,
    kind: "note",
    body: `Note ${i + 1}`,
    author: "a.novak@example.gov",
    relation: "member",
    at: `2026-10-22T06:${String(i).padStart(2, "0")}:00Z`,
  }));
}

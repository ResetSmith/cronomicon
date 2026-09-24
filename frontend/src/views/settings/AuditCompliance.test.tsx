// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";

// The Data Retention card (LU-15). These knobs were stored-but-inert until the
// nightly sweeper was wired to read them (LU-2), which changes what the UI owes
// the operator: every value posted from here now deletes real rows and real log
// files on the next sweep. So the behaviours worth pinning are the destructive
// ones — that a full blob round-trips rather than a partial one (a dropped key
// would read as 0 = keep forever, silently disabling retention for that table),
// that a negative window can never be posted (its delete cutoff would land in
// the future and empty the table), and that a rejected save says so instead of
// flashing "Saved" over a value the server refused.

const DAYS = { runs: 30, activity: 45, workflowRuns: 60, changeLog: 400, schedulePushes: 365, logFiles: 14 };

let getData: unknown = { retentionDays: DAYS };
const puts: { path: string; body: unknown }[] = [];
let putError: unknown = null;

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async () => ({ data: getData })),
      PUT: vi.fn(async (path: string, opts: { body?: unknown }) => {
        puts.push({ path, body: opts.body });
        return putError ? { error: putError } : { data: getData };
      }),
    } as unknown as typeof actual.api,
  };
});

import { AuditComplianceSection } from "./AuditCompliance";

beforeEach(() => {
  puts.length = 0;
  putError = null;
  getData = { retentionDays: DAYS };
});
afterEach(cleanup);

describe("Data Retention card", () => {
  it("shows the stored window for every knob, not a default", async () => {
    // Six independent knobs is the point of LU-2 — before it, two env vars
    // covered five tables. If the card ever collapses them the operator loses
    // the ability to keep the change log longer than run history.
    const { container } = render(<AuditComplianceSection />);
    const q = within(container);
    await waitFor(() => expect((q.getByLabelText("Run History") as HTMLInputElement).value).toBe("30"));
    expect((q.getByLabelText("Activity Log") as HTMLInputElement).value).toBe("45");
    expect((q.getByLabelText("Workflow Runs") as HTMLInputElement).value).toBe("60");
    expect((q.getByLabelText("Change Log") as HTMLInputElement).value).toBe("400");
    expect((q.getByLabelText("Schedule Pushes") as HTMLInputElement).value).toBe("365");
    expect((q.getByLabelText("Run Log Files") as HTMLInputElement).value).toBe("14");
  });

  it("PUTs the whole retentionDays object, not just the edited knob", async () => {
    // A partial body would arrive at the server with the untouched keys absent,
    // and Go would unmarshal those as 0 — turning every other table's retention
    // off as a side effect of editing one field.
    const { container } = render(<AuditComplianceSection />);
    const q = within(container);
    await waitFor(() => expect(q.getByLabelText("Run History")).toBeTruthy());

    fireEvent.change(q.getByLabelText("Run History"), { target: { value: "7" } });
    fireEvent.click(q.getByText("Save Changes"));

    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0].path).toBe("/settings/audit-compliance");
    expect(puts[0].body).toEqual({ retentionDays: { ...DAYS, runs: 7 } });
  });

  it("clamps a negative window to 0 rather than posting it", async () => {
    // A negative day count is silently absorbed by the sweeper as "keep
    // forever", the opposite of what typing -1 asks for — so the server rejects
    // it rather than quietly disabling retention. This keeps the form from
    // getting there, and keeps 0 as the one explicit way to say "never delete".
    const { container } = render(<AuditComplianceSection />);
    const q = within(container);
    await waitFor(() => expect(q.getByLabelText("Run Log Files")).toBeTruthy());

    fireEvent.change(q.getByLabelText("Run Log Files"), { target: { value: "-5" } });
    fireEvent.click(q.getByText("Save Changes"));

    await waitFor(() => expect(puts).toHaveLength(1));
    expect((puts[0].body as { retentionDays: { logFiles: number } }).retentionDays.logFiles).toBe(0);
  });

  it("labels 0 as keep-forever rather than leaving a bare 0", async () => {
    // 0 is not "no retention", it is "never delete" — the opposite reading. The
    // unit column carries that meaning, so it has to change with the value.
    getData = { retentionDays: { ...DAYS, changeLog: 0 } };
    const { container } = render(<AuditComplianceSection />);
    const q = within(container);
    await waitFor(() => expect((q.getByLabelText("Change Log") as HTMLInputElement).value).toBe("0"));
    expect(q.getAllByText("∞")).toHaveLength(1);
  });

  it("surfaces a rejected save instead of reporting success", async () => {
    putError = { message: "retentionDays.runs must be at most 10000 days (use 0 to keep forever)" };
    const { container } = render(<AuditComplianceSection />);
    const q = within(container);
    await waitFor(() => expect(q.getByLabelText("Run History")).toBeTruthy());

    fireEvent.click(q.getByText("Save Changes"));

    await waitFor(() => expect(q.getByText(/Save failed/)).toBeTruthy());
    expect(q.getByText(/must be at most 10000 days/)).toBeTruthy();
    expect(q.queryByText("✓ Saved")).toBeNull();
  });
});

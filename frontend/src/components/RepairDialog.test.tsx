// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// A repair (2.4.0 I4, IN-7; the owner's answer to question 48): a write that
// changes an object's agency is a global administrator's, with a reason the
// server writes to the audit log. The dialog asks for the reason and keeps its
// confirm off until there is one within the 500-character limit; a restore
// that the server answers 409 repair_required asks for it and is sent again.

const posts: { path: string; body: unknown }[] = [];
let restoreAnswers: unknown[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async () => ({
        data: {
          items: [
            { revisionNo: 2, action: "updated", actor: "a@x", createdAt: "2026-10-15T00:00:00Z", digest: "d2", snapshot: { steps: ["tax-job"] } },
            { revisionNo: 1, action: "created", actor: "a@x", createdAt: "2026-10-14T00:00:00Z", digest: "d1", snapshot: { steps: ["fin-job"] } },
          ],
        },
      })),
      POST: vi.fn(async (path: string, init?: { body?: unknown }) => {
        posts.push({ path, body: init?.body });
        return restoreAnswers.shift() ?? { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});

import { RepairDialog } from "./RepairDialog";
import { RevisionHistory } from "../views/RevisionHistory";

beforeEach(() => {
  posts.length = 0;
  restoreAnswers = [];
  vi.spyOn(window, "confirm").mockReturnValue(true);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("RepairDialog", () => {
  it("keeps its confirm off until there is a reason within the limit, and sends it trimmed", () => {
    const onConfirm = vi.fn();
    render(<RepairDialog title="Move it?" message="It moves." confirmLabel="Move" onConfirm={onConfirm} onCancel={vi.fn()} />);
    const move = screen.getByRole("button", { name: "Move" }) as HTMLButtonElement;
    expect(move.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "   " } });
    expect(move.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "x".repeat(501) } });
    expect(move.disabled).toBe(true);
    expect(screen.getByText("501 / 500")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "  wrong agency  " } });
    expect(move.disabled).toBe(false);
    fireEvent.click(move);
    expect(onConfirm).toHaveBeenCalledWith({ repair: true, reason: "wrong agency" });
  });
});

describe("RevisionHistory — a restore that changes a workflow's agency", () => {
  it("asks for the reason on repair_required and restores again with it", async () => {
    restoreAnswers = [
      { error: { code: "repair_required", message: "this changes the agency of this workflow, which is a repair" } },
      { data: {} },
    ];
    render(<RevisionHistory kind="workflow" name="nightly" onClose={vi.fn()} />);
    fireEvent.click(await screen.findByText("#1"));
    fireEvent.click(screen.getByRole("button", { name: "Restore this revision" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0].body).toBeUndefined();
    expect(await screen.findByText(/Restore revision 1 for another agency\?/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "the Finance jobs were right" } });
    fireEvent.click(screen.getByRole("button", { name: "Restore" }));
    await waitFor(() => expect(posts).toHaveLength(2));
    expect(posts[1]).toEqual({
      path: "/definitions/{kind}/{name}/revisions/{no}/restore",
      body: { repair: true, reason: "the Finance jobs were right" },
    });
    await waitFor(() => expect(screen.queryByText(/for another agency\?/)).toBeNull());
  });

  it("says why a restore was refused", async () => {
    restoreAnswers = [{ error: { code: "forbidden", message: "only a global administrator can repair this" } }];
    render(<RevisionHistory kind="workflow" name="nightly" onClose={vi.fn()} />);
    fireEvent.click(await screen.findByText("#1"));
    fireEvent.click(screen.getByRole("button", { name: "Restore this revision" }));
    expect(await screen.findByText("Restore failed: only a global administrator can repair this")).toBeTruthy();
  });
});

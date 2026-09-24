// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { RecycleBinSection } from "./RecycleBin";

// RH-F. The two things this screen must get right are the ones an operator
// cannot undo by trying again: purge asks first, and the countdown says how long
// they actually have.

// vi.mock's factory is hoisted above the module body, so anything it closes over
// must be hoisted with it.
const { restore, purge, soon } = vi.hoisted(() => ({
  restore: vi.fn(async () => ({})),
  purge: vi.fn(async () => ({})),
  soon: new Date(Date.now() + 3 * 86_400_000).toISOString(),
}));

vi.mock("../../api/client", () => ({
  csrfHeader: { "X-CSRF-Token": "t" },
  errMsg: (e: unknown) => String(e),
  api: {
    GET: vi.fn(async () => ({
      data: {
        retentionDays: 30,
        items: [
          { kind: "job", name: "billing", deletedAt: "2026-08-11T09:00:00Z", deletedBy: "ada@example.com", purgeAt: soon },
          { kind: "workflow", name: "release", deletedAt: "2026-08-10T09:00:00Z", deletedBy: "ada@example.com", purgeAt: null },
        ],
      },
    })),
    POST: restore,
    DELETE: purge,
  },
}));

beforeEach(() => {
  restore.mockClear();
  purge.mockClear();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("RecycleBin (RH-F)", () => {
  it("lists binned definitions of every kind with who deleted them", async () => {
    render(<RecycleBinSection />);
    await waitFor(() => expect(screen.getByText("billing")).toBeTruthy());
    expect(screen.getByText("release")).toBeTruthy();
    expect(screen.getByText("Job")).toBeTruthy();
    expect(screen.getByText("Workflow")).toBeTruthy();
    expect(screen.getAllByText("ada@example.com")).toHaveLength(2);
  });

  it("shows how long is left, not just that something is binned", async () => {
    render(<RecycleBinSection />);
    await waitFor(() => expect(screen.getByText("billing")).toBeTruthy());
    expect(screen.getByText("in 3 days")).toBeTruthy();
    // A null purgeAt is "keep forever", which must read as never rather than blank.
    expect(screen.getByText("never")).toBeTruthy();
    expect(screen.getByText(/30 days/)).toBeTruthy();
  });

  it("restores without a confirmation — it is reversible", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    render(<RecycleBinSection />);
    await waitFor(() => expect(screen.getByText("billing")).toBeTruthy());
    fireEvent.click(screen.getAllByRole("button", { name: "Restore" })[0]);
    await waitFor(() => expect(restore).toHaveBeenCalledTimes(1));
    expect(confirm).not.toHaveBeenCalled();
  });

  it("asks before purging, and does nothing if the operator declines", async () => {
    const confirmSpy = vi.fn(() => false);
    vi.stubGlobal("confirm", confirmSpy);
    render(<RecycleBinSection />);
    await waitFor(() => expect(screen.getByText("billing")).toBeTruthy());
    fireEvent.click(screen.getAllByRole("button", { name: "Delete forever" })[0]);
    expect(confirmSpy).toHaveBeenCalled();
    expect(purge).not.toHaveBeenCalled();
  });

  it("purges once confirmed", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    render(<RecycleBinSection />);
    await waitFor(() => expect(screen.getByText("billing")).toBeTruthy());
    fireEvent.click(screen.getAllByRole("button", { name: "Delete forever" })[0]);
    await waitFor(() => expect(purge).toHaveBeenCalledTimes(1));
  });

  it("explains that a binned definition still holds its name", async () => {
    // The prose is split by <strong>, so assert on the container's text rather
    // than a single node — and on the consequence, which is the part an operator
    // hits when their "create a replacement" 409s.
    const { container } = render(<RecycleBinSection />);
    await waitFor(() => expect(screen.getByText("billing")).toBeTruthy());
    const text = container.textContent ?? "";
    expect(text).toMatch(/keeps its\s*name/i);
    expect(text).toMatch(/replacement cannot be created/i);
  });
});

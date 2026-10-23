// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.0 C6 (QT-1) — an agency's own concurrency cap, on its row. A global
// administrator sets it, Global's included; anyone else sees it and the reason.

let CAP: number | null = null;
const puts: Array<{ path: string; body: unknown }> = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/agencies") return { data: [{ id: "ag-tax", name: "Tax", maxConcurrent: CAP }] };
        if (path === "/agency-matrix") return { data: { rows: [] } };
        if (path === "/agencies/{agencyId}") return { data: { members: [], onlineRunners: 0, queuedRuns: 0 } };
        if (path === "/agencies/{agencyId}/vault-prefixes") return { data: { agencyId: "ag-tax", prefixes: [] } };
        if (path === "/agencies/{agencyId}/groups") return { data: { agencyId: "ag-tax", groups: [] } };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        puts.push({ path, body: opts?.body });
        CAP = (opts?.body as { maxConcurrent: number | null }).maxConcurrent;
        return { data: { id: "ag-tax", name: "Tax", maxConcurrent: CAP } };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { AgenciesTab } from "./AgenciesTab";

beforeEach(() => {
  puts.length = 0;
  CAP = null;
});
afterEach(cleanup);

const openAgency = async (globalAdmin: boolean) => {
  render(
    <MemoryRouter>
      <AgenciesTab canEdit globalAdmin={globalAdmin} />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("Tax")).toBeTruthy());
  fireEvent.click(screen.getByText("Tax").closest("tr")!);
  await waitFor(() => expect(screen.getByText("Concurrency cap")).toBeTruthy());
};

describe("an agency's concurrency cap (QT-1)", () => {
  it("says when the agency has none of its own", async () => {
    await openAgency(true);
    expect(screen.getByText(/None of its own: the installation's cap alone/)).toBeTruthy();
  });

  it("is set by a global administrator, and shown as set", async () => {
    await openAgency(true);
    fireEvent.click(screen.getByRole("button", { name: "Edit cap" }));
    fireEvent.change(screen.getByLabelText("Concurrency cap"), { target: { value: "3" } });
    fireEvent.click(screen.getByRole("button", { name: "Save cap" }));
    await waitFor(() => expect(puts).toEqual([{ path: "/agencies/{agencyId}/concurrency", body: { maxConcurrent: 3 } }]));
    await waitFor(() => expect(screen.getByText("3 runs at a time")).toBeTruthy());
  });

  it("is removed by saving it empty", async () => {
    CAP = 2;
    await openAgency(true);
    fireEvent.click(screen.getByRole("button", { name: "Edit cap" }));
    fireEvent.change(screen.getByLabelText("Concurrency cap"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save cap" }));
    await waitFor(() => expect(puts).toEqual([{ path: "/agencies/{agencyId}/concurrency", body: { maxConcurrent: null } }]));
  });

  it("refuses a value that is not a whole number of runs, before asking the server", async () => {
    await openAgency(true);
    fireEvent.click(screen.getByRole("button", { name: "Edit cap" }));
    for (const v of ["0", "-1", "2.5", "x"]) {
      fireEvent.change(screen.getByLabelText("Concurrency cap"), { target: { value: v } });
      fireEvent.click(screen.getByRole("button", { name: "Save cap" }));
      expect(screen.getByRole("alert").textContent).toMatch(/whole number of runs/);
    }
    expect(puts).toEqual([]);
  });

  it("is shown, not edited, to anyone else, with the reason", async () => {
    CAP = 1;
    await openAgency(false);
    expect(screen.getByText("1 run at a time")).toBeTruthy();
    const edit = screen.getByRole("button", { name: "Edit cap" }) as HTMLButtonElement;
    expect(edit.disabled).toBe(true);
    expect(edit.title).toMatch(/Only a global administrator .* concurrency cap/);
  });
});

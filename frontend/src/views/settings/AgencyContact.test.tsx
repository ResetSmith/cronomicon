// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// 2.5.2 OP-28 — the agency's contact, kept on Settings → Alerts by the
// agency's administrators: who the operations team calls about the agency's
// runs when a job names no one.

let STORED = "";
const { GET, PUT } = vi.hoisted(() => ({ GET: vi.fn(), PUT: vi.fn() }));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET, PUT } as unknown as typeof actual.api };
});

import { AgencyContactCard } from "./AgencyContact";

beforeEach(() => {
  STORED = "Payroll on-call";
  GET.mockReset();
  PUT.mockReset();
  GET.mockImplementation(async () => ({ data: { agency: { id: "ag-p", name: "Payroll", contact: STORED } } }));
  PUT.mockImplementation(async (_p: string, opts: { body: { contact: string } }) => {
    STORED = opts.body.contact.trim();
    return { data: { id: "ag-p", contact: STORED } };
  });
});
afterEach(cleanup);

describe("the agency contact card (2.5.2 OP-28)", () => {
  it("shows the stored contact and saves a change to the agency", async () => {
    render(<AgencyContactCard agency="ag-p" agencyName="Payroll" why="" />);
    const input = (await screen.findByDisplayValue("Payroll on-call")) as HTMLInputElement;
    const save = screen.getByRole("button", { name: "Save Changes" }) as HTMLButtonElement;
    // Nothing to save until it changes.
    expect(save.disabled).toBe(true);
    fireEvent.change(input, { target: { value: "Payroll desk, ext. 4410" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(PUT).toHaveBeenCalled());
    const [path, opts] = PUT.mock.calls[0] as [string, { params: { path: { agencyId: string } }; body: { contact: string } }];
    expect(path).toBe("/agencies/{agencyId}/contact");
    expect(opts.params.path.agencyId).toBe("ag-p");
    expect(opts.body.contact).toBe("Payroll desk, ext. 4410");
  });

  it("may be cleared: an empty contact is none", async () => {
    render(<AgencyContactCard agency="ag-p" agencyName="Payroll" why="" />);
    fireEvent.change(await screen.findByDisplayValue("Payroll on-call"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Changes" }));
    await waitFor(() => expect(PUT).toHaveBeenCalled());
    expect((PUT.mock.calls[0][1] as { body: { contact: string } }).body.contact).toBe("");
  });

  it("is read-only, saying why, for someone who may not change it", async () => {
    render(<AgencyContactCard agency="ag-p" agencyName="Payroll" why="Only Payroll's administrators can change its contact." />);
    const input = (await screen.findByDisplayValue("Payroll on-call")) as HTMLInputElement;
    expect(input.disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save Changes" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("Only Payroll's administrators can change its contact.")).toBeTruthy();
  });
});

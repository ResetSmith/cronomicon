// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// Changing the agency of a secret, variable or SSH key (v2.3.0; a repair
// since 2.4.0 I4, IN-7; the owner's answer to question 48).
//
// Each belongs to exactly one agency. Since 2.4.0 an agency administrator does
// not move one: another agency gets one by making it again. A global
// administrator repairs a placement that was a mistake, with a reason the
// server writes to the audit log; the control is theirs alone, and anyone else
// reads that the agency does not change.

let CAPS: Record<string, boolean> = {};
const puts: { path: string; body: unknown }[] = [];
let putError: unknown = null;

const CATALOG = [
  { id: "global", name: "Global" },
  { id: "ag-fin", name: "Finance" },
  { id: "ag-tax", name: "Tax" },
];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({ manageEnvVars: true, configureApp: true, ...CAPS })),
    api: {
      GET: vi.fn(async (path: string) => (path === "/agencies" ? { data: CATALOG } : { data: [] })),
      PUT: vi.fn(async (path: string, init?: { body?: unknown }) => {
        puts.push({ path, body: init?.body });
        return putError ? { error: putError } : { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});

import { MoveAgency } from "./MoveAgency";

const moved = vi.fn();
const failed = vi.fn();
const show = (over: Partial<Parameters<typeof MoveAgency>[0]> = {}) =>
  render(<MoveAgency kind="secret" id="s1" name="DB_PASS" ownerName="Finance" onMoved={moved} onError={failed} {...over} />);
const select = () => screen.findByRole("combobox", { name: "Repair: move DB_PASS to another agency" }) as Promise<HTMLSelectElement>;
const options = (el: HTMLSelectElement) => Array.from(el.options).map((o) => o.textContent);
const repair = async (reason: string, confirm: string) => {
  fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: reason } });
  fireEvent.click(screen.getByRole("button", { name: confirm }));
};

beforeEach(() => {
  CAPS = { configureAppGlobal: true };
  puts.length = 0;
  putError = null;
  moved.mockClear();
  failed.mockClear();
});
afterEach(cleanup);

describe("MoveAgency — a repair", () => {
  it("offers a global administrator every other agency, Global included, and sends the repair with its reason", async () => {
    show();
    const el = await select();
    await waitFor(() => expect(options(el)).toEqual(["Repair: move to another agency…", "Global", "Tax"]));

    fireEvent.change(el, { target: { value: "ag-tax" } });
    expect(puts).toHaveLength(0);
    // The dialog says what stops working, and asks why.
    expect(await screen.findByText(/Finance's runs that bind this secret will stop resolving it/)).toBeTruthy();
    const confirm = screen.getByRole("button", { name: "Move to Tax" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true); // no reason yet
    await repair("  created in the wrong agency  ", "Move to Tax");
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toEqual({
      path: "/secret-agencies",
      body: { repair: true, reason: "created in the wrong agency", assignments: [{ id: "s1", agencyIds: ["ag-tax"] }] },
    });
    await waitFor(() => expect(moved).toHaveBeenCalledWith("DB_PASS now belongs to Tax"));
  });

  it("uses each kind's own route", async () => {
    show({ kind: "ssh-credential" });
    fireEvent.change(await select(), { target: { value: "ag-tax" } });
    await repair("wrong agency", "Move to Tax");
    await waitFor(() => expect(puts[0]?.path).toBe("/ssh-credential-agencies"));
    cleanup();
    puts.length = 0;
    show({ kind: "env-var" });
    fireEvent.change(await select(), { target: { value: "ag-tax" } });
    await repair("wrong agency", "Move to Tax");
    await waitFor(() => expect(puts[0]?.path).toBe("/env-var-agencies"));
  });

  it("says what Global means", async () => {
    show();
    fireEvent.change(await select(), { target: { value: "global" } });
    // §0a (IN-6): Global's is Global's runs' only (until 2.4.2 the dialog said every agency's).
    expect(await screen.findByText(/It becomes Global's: only Global's runs may use it, and only global administrators change it\./)).toBeTruthy();
  });

  it("is not offered to an agency's administrator, who reads that the agency does not change", async () => {
    CAPS = {};
    show();
    expect(await screen.findByText(/Its agency does not change: another agency gets one by a copy/)).toBeTruthy();
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("follows the row's own canRepair over the capability", async () => {
    CAPS = {};
    show({ canRepair: true });
    expect(await select()).toBeTruthy();
    cleanup();
    CAPS = { configureAppGlobal: true };
    show({ canRepair: false });
    expect(await screen.findByText(/Its agency does not change/)).toBeTruthy();
  });

  it("keeps the dialog open with the server's refusal, and moves nothing", async () => {
    putError = { code: "vault_path_not_allowed", message: "the path is outside the Vault paths assigned to Tax" };
    show();
    fireEvent.change(await select(), { target: { value: "ag-tax" } });
    await repair("wrong agency", "Move to Tax");
    await waitFor(() => expect(failed).toHaveBeenCalledWith("Repair failed: the path is outside the Vault paths assigned to Tax"));
    expect(screen.getByRole("alert").textContent).toMatch(/outside the Vault paths/);
    expect(moved).not.toHaveBeenCalled();
  });
});

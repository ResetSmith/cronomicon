// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { ServiceAccountsSection } from "./ServiceAccounts";

// ET-C — the shown-once contract, which is the only part of this screen that
// cannot be recovered if it is wrong. The plaintext exists solely in the mint
// response; if the panel outlives it, or the list ever renders it, the
// "cannot be revealed again" promise is false.

const listPayload = {
  items: [
    {
      id: "sa-1",
      name: "nagios",
      role: "operator",
      allScopes: false,
      agencyName: "Tax",
      createdBy: "admin@example.com",
      createdAt: "2026-08-11T00:00:00Z",
      lastUsedAt: "2026-08-11T06:00:00Z",
      status: "active",
    },
    {
      id: "sa-2",
      name: "retired-bot",
      role: "viewer",
      allScopes: true,
      createdBy: "admin@example.com",
      createdAt: "2026-01-01T00:00:00Z",
      status: "revoked",
    },
  ],
};

vi.mock("../../api/client", () => ({
  csrfHeader: { "X-CSRF-Token": "t" },
  errMsg: (e: unknown) => String(e),
  api: {
    GET: vi.fn(async (path: string) => {
      if (path === "/service-accounts") return { data: listPayload };
      if (path === "/agencies") return { data: [{ id: "ag-1", name: "Tax" }] };
      if (path === "/roles") return { data: [{ name: "admin" }, { name: "operator" }] };
      return { data: undefined };
    }),
    POST: vi.fn(async () => ({
      data: { id: "sa-9", name: "fresh", role: "operator", allScopes: true, status: "active", token: "crnsvc_SECRETVALUE" },
    })),
    DELETE: vi.fn(async () => ({})),
  },
}));

beforeEach(() => {
  vi.stubGlobal("confirm", () => true);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ServiceAccounts (ET-C)", () => {
  it("lists accounts with their status and never shows token material", async () => {
    render(<ServiceAccountsSection canGrantEverywhere />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());
    expect(screen.getByText("retired-bot")).toBeTruthy();
    // A revoked account keeps its row (it is an audit actor) but loses Revoke.
    expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(1);
    expect(document.body.textContent).not.toContain("crnsvc_");
  });

  it("shows the minted token once and hides it again when dismissed", async () => {
    render(<ServiceAccountsSection canGrantEverywhere />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());

    fireEvent.change(screen.getByPlaceholderText("nagios"), { target: { value: "fresh" } });
    fireEvent.click(screen.getByRole("button", { name: /Create service account/ }));

    await waitFor(() => expect(screen.getByTestId("minted-token").textContent).toBe("crnsvc_SECRETVALUE"));
    expect(screen.getByText(/shown once/i)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByTestId("minted-token")).toBeNull());
  });

  it("requires a name before the account can be created", async () => {
    render(<ServiceAccountsSection canGrantEverywhere />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());
    const create = screen.getByRole("button", { name: /Create service account/ }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    fireEvent.change(screen.getByPlaceholderText("nagios"), { target: { value: "x" } });
    expect((screen.getByRole("button", { name: /Create service account/ }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("says a job must be requestable — the gate operators otherwise trip over", async () => {
    render(<ServiceAccountsSection canGrantEverywhere />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());
    expect(screen.getByText(/requestable/i)).toBeTruthy();
  });
});

// GC-5 (v2.2.2, gate closing) — minting a service account IS granting a role, so
// it follows the access-grant rules: a delegate (manageRoles for their own
// agencies) mints for an agency and never for all of them. Until 2.2.2 a
// delegate could mint an all-scopes `admin` token here. The "All scopes" option
// is withheld from them — it is never theirs, so this is irrelevance, not a
// precondition (FX-7) — and the form no longer DEFAULTS to it.
describe("ServiceAccounts — the all-scopes option is a global administrator's (GC-5)", () => {
  const lastBody = async () => {
    const { api } = await import("../../api/client");
    const calls = (api.POST as unknown as ReturnType<typeof vi.fn>).mock.calls;
    return (calls[calls.length - 1][1] as { body: Record<string, unknown> }).body;
  };
  beforeEach(async () => {
    const { api } = await import("../../api/client");
    (api.POST as unknown as ReturnType<typeof vi.fn>).mockClear();
  });

  it("withholds All scopes from a delegate and makes them name an agency", async () => {
    render(<ServiceAccountsSection canGrantEverywhere={false} />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());

    const where = screen.getByLabelText("Where") as HTMLSelectElement;
    expect(Array.from(where.options).map((o) => o.textContent)).toEqual(["Choose an agency…", "Tax"]);
    expect(screen.queryByRole("option", { name: "All scopes (*)" })).toBeNull();

    // A name alone is not enough: with no agency chosen there is nothing this
    // caller may send, and the button says what is missing.
    fireEvent.change(screen.getByPlaceholderText("nagios"), { target: { value: "fresh" } });
    const create = screen.getByRole("button", { name: /Create service account/ }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    expect(create.title).toBe("Choose an agency first");

    fireEvent.change(where, { target: { value: "ag-1" } });
    expect((screen.getByRole("button", { name: /Create service account/ }) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: /Create service account/ }));
    await waitFor(() => expect(screen.getByTestId("minted-token")).toBeTruthy());

    const body = await lastBody();
    expect(body.agencyId).toBe("ag-1");
    expect("allScopes" in body).toBe(false);
  });

  it("offers All scopes to a global administrator, as the default", async () => {
    render(<ServiceAccountsSection canGrantEverywhere />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());
    const where = screen.getByLabelText("Where") as HTMLSelectElement;
    expect(Array.from(where.options).map((o) => o.textContent)).toEqual(["All scopes (*)", "Tax"]);

    fireEvent.change(screen.getByPlaceholderText("nagios"), { target: { value: "fresh" } });
    fireEvent.click(screen.getByRole("button", { name: /Create service account/ }));
    await waitFor(() => expect(screen.getByTestId("minted-token")).toBeTruthy());
    const body = await lastBody();
    expect(body.allScopes).toBe(true);
    expect("agencyId" in body).toBe(false);
  });
});

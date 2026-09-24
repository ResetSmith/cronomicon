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
      data: { id: "sa-9", name: "fresh", role: "operator", allScopes: true, status: "active", token: "amasvc_SECRETVALUE" },
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
    render(<ServiceAccountsSection />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());
    expect(screen.getByText("retired-bot")).toBeTruthy();
    // A revoked account keeps its row (it is an audit actor) but loses Revoke.
    expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(1);
    expect(document.body.textContent).not.toContain("amasvc_");
  });

  it("shows the minted token once and hides it again when dismissed", async () => {
    render(<ServiceAccountsSection />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());

    fireEvent.change(screen.getByPlaceholderText("nagios"), { target: { value: "fresh" } });
    fireEvent.click(screen.getByRole("button", { name: /Create service account/ }));

    await waitFor(() => expect(screen.getByTestId("minted-token").textContent).toBe("amasvc_SECRETVALUE"));
    expect(screen.getByText(/shown once/i)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByTestId("minted-token")).toBeNull());
  });

  it("requires a name before the account can be created", async () => {
    render(<ServiceAccountsSection />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());
    const create = screen.getByRole("button", { name: /Create service account/ }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    fireEvent.change(screen.getByPlaceholderText("nagios"), { target: { value: "x" } });
    expect((screen.getByRole("button", { name: /Create service account/ }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("says a job must be requestable — the gate operators otherwise trip over", async () => {
    render(<ServiceAccountsSection />);
    await waitFor(() => expect(screen.getByText("nagios")).toBeTruthy());
    expect(screen.getByText(/requestable/i)).toBeTruthy();
  });
});

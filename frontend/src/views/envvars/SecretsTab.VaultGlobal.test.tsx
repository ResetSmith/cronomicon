// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

// GC-8 (v2.2.2, gate closing) — there is ONE Vault connection for the
// installation, and a secret's Vault path is checked only for being non-empty.
// So a Vault-backed secret can name any path that connection can read — another
// agency's included — and Migrate to Vault can write to any path it can write.
// Until per-agency paths exist the interim is closed rather than open: naming a
// Vault path is a global administrator's (`manageEnvVarsGlobal`).
//
// A departmental manageEnvVars holder still manages stored secrets, and still
// SEES the three Vault controls — Vault is configured, so they are relevant —
// disabled with the reason (FX-7). Reading, revealing and deleting an existing
// Vault-backed secret are unchanged.

let caps: Record<string, boolean> = {};

const SECRETS = [
  { id: "s-stored", key: "DB_PASS", scope: "Prod", source: "stored", description: "", tags: [] },
  { id: "s-vault", key: "API_TOKEN", scope: "Prod", source: "vault", vaultPath: "secret/data/app#TOKEN", description: "", tags: [] },
];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    fetchCapabilities: vi.fn(async () => ({
      vault: true, apprise: false, compose: false, composeUnbound: false, manageRoles: false,
      configureApp: false, manageEnvVars: true, publishSchedule: false, triggerJobs: false,
      killJobs: false, unrestricted: true,
      ...caps,
    })),
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/env-secrets") return { data: SECRETS };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { SecretsTab } from "./SecretsTab";

const WHY =
  "A Vault-backed secret names a path on the installation's one Vault connection — only a global administrator can create, edit or migrate one.";

beforeEach(() => {
  caps = {};
  localStorage.clear();
});
afterEach(cleanup);

const open = async () => {
  render(<SecretsTab scopeNames={["All", "Prod"]} canEdit />);
  await waitFor(() => expect(screen.getByText("DB_PASS")).toBeTruthy());
  // Let the capabilities read land before judging what is disabled.
  await new Promise((r) => setTimeout(r, 20));
};
const rowOf = (key: string) => within(screen.getByText(key).closest("tr") as HTMLElement);
const editOf = (key: string) => rowOf(key).getByRole("button", { name: "Edit" }) as HTMLButtonElement;

describe("Secrets — Vault controls for a departmental manager (GC-8)", () => {
  beforeEach(() => {
    caps = { manageEnvVarsGlobal: false };
  });

  it("disables Edit on a Vault-backed row with the reason, and leaves a stored row's Edit and both Deletes live", async () => {
    await open();
    expect(editOf("API_TOKEN").disabled).toBe(true);
    expect(editOf("API_TOKEN").title).toBe(WHY);
    expect(editOf("DB_PASS").disabled).toBe(false);
    expect(editOf("DB_PASS").title).toBe("");
    for (const key of ["API_TOKEN", "DB_PASS"]) {
      expect((rowOf(key).getByRole("button", { name: "Delete" }) as HTMLButtonElement).disabled, key).toBe(false);
    }
  });

  it("disables Migrate to Vault with the reason, and says it beside the button", async () => {
    await open();
    fireEvent.click(screen.getByText("DB_PASS"));
    const migrate = (await waitFor(() => screen.getByRole("button", { name: "Migrate to Vault" }))) as HTMLButtonElement;
    expect(migrate.disabled).toBe(true);
    expect(migrate.title).toBe(WHY);
    expect(screen.getByText(WHY)).toBeTruthy();
    fireEvent.click(migrate);
    expect(screen.queryByText("Migrate secret to Vault?")).toBeNull();
  });

  it("shows the Vault source option disabled with the reason when adding a secret, and it cannot be chosen", async () => {
    await open();
    fireEvent.click(screen.getByRole("button", { name: "+ Add Secret" }));
    const opt = await waitFor(() => screen.getByText("Vault reference"));
    expect(opt.getAttribute("aria-disabled")).toBe("true");
    expect(opt.getAttribute("title")).toBe(WHY);
    expect(screen.getByRole("note").textContent).toBe(WHY);

    fireEvent.click(opt);
    // Still the stored form: no Vault path field appeared.
    expect(screen.queryByText("Vault Path *")).toBeNull();
    expect(screen.getByText("Secret Value *")).toBeTruthy();
  });
});

describe("Secrets — Vault controls for a global administrator (GC-8)", () => {
  beforeEach(() => {
    caps = { manageEnvVarsGlobal: true };
  });

  it("enables Edit on a Vault-backed row and Migrate to Vault", async () => {
    await open();
    expect(editOf("API_TOKEN").disabled).toBe(false);
    expect(editOf("API_TOKEN").title).toBe("");

    fireEvent.click(screen.getByText("DB_PASS"));
    const migrate = (await waitFor(() => screen.getByRole("button", { name: "Migrate to Vault" }))) as HTMLButtonElement;
    expect(migrate.disabled).toBe(false);
    expect(screen.queryByText(WHY)).toBeNull();
    fireEvent.click(migrate);
    await waitFor(() => expect(screen.getByText("Migrate secret to Vault?")).toBeTruthy());
  });

  it("lets the Vault source be chosen when adding a secret", async () => {
    await open();
    fireEvent.click(screen.getByRole("button", { name: "+ Add Secret" }));
    const opt = await waitFor(() => screen.getByText("Vault reference"));
    expect(opt.getAttribute("aria-disabled")).toBeNull();
    expect(screen.queryByRole("note")).toBeNull();
    fireEvent.click(opt);
    await waitFor(() => expect(screen.getByText("Vault Path *")).toBeTruthy());
  });
});

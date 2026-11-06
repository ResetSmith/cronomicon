// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// Vault-backed secrets, by owner (GC-8 in v2.2.2; LR-80 in v2.3.0).
//
// There is ONE Vault connection for the installation. In 2.2.2 naming a path on
// it was a global administrator's alone, because nothing divided it by agency.
// Since 2.3.0 a global administrator assigns each agency its path prefixes, and:
//
//   - a secret that is GLOBAL's may name any path, and is still a global
//     administrator's: for anyone else its three Vault controls are disabled
//     with the reason (FX-7);
//   - an agency's own secret is its administrators', inside that agency's
//     prefixes — the form shows them and says so before the request is sent;
//   - an agency with no prefixes can hold no Vault-backed row, and says why.
//
// Reading, revealing and deleting an existing Vault-backed secret are unchanged.

let caps: Record<string, boolean> = {};

const SECRETS = [
  { id: "s-stored", key: "DB_PASS", scope: "Prod", source: "stored", description: "", tags: [], ownerAgency: "Global" },
  { id: "s-vault", key: "API_TOKEN", scope: "Prod", source: "vault", vaultPath: "secret/data/app#TOKEN", description: "", tags: [], ownerAgency: "Global" },
  { id: "s-tax-stored", key: "TAX_PASS", scope: "Prod", source: "stored", description: "", tags: [], ownerAgency: "Tax" },
  { id: "s-tax-vault", key: "TAX_TOKEN", scope: "Prod", source: "vault", vaultPath: "secret/data/tax/app#TOKEN", description: "", tags: [], ownerAgency: "Tax" },
];
const CATALOG = [
  { id: "global", name: "Global" },
  { id: "ag-tax", name: "Tax" },
  { id: "ag-hr", name: "HR" },
];
// Tax has been assigned a prefix; HR has not.
const PREFIXES: Record<string, string[]> = { "ag-tax": ["secret/data/tax"], "ag-hr": [] };
let ACCESS: unknown = null;
const posts: { path: string; body: unknown }[] = [];

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
      GET: vi.fn(async (path: string, init?: { params?: { path?: { agencyId?: string } } }) => {
        if (path === "/env-secrets") return { data: SECRETS };
        if (path === "/agencies") return { data: CATALOG };
        if (path === "/agencies/{agencyId}/vault-prefixes") {
          const id = init?.params?.path?.agencyId ?? "";
          return { data: { agencyId: id, prefixes: PREFIXES[id] ?? [] } };
        }
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, init?: { body?: unknown; params?: { path?: { agencyId?: string } } }) => {
        posts.push({ path, body: init?.body });
        // The server's judgement (2.5.5 SM-10), as the write makes it.
        if (path === "/agencies/{agencyId}/vault/validate-ref") {
          const id = init?.params?.path?.agencyId ?? "";
          const ref = ((init?.body as { ref?: string }) ?? {}).ref ?? "";
          const p = ref.split("#")[0];
          const inside = (PREFIXES[id] ?? []).some((x) => p === x || p.startsWith(x + "/"));
          return { data: inside ? { ok: true } : { ok: false, code: "vault_path_not_allowed", reason: `that reference is outside the paths assigned to this secret's agency (${(PREFIXES[id] ?? []).join(", ")})` } };
        }
        return { data: {} };
      }),
      PUT: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

vi.mock("../../api/access", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/access")>();
  return { ...actual, fetchMyAccess: vi.fn(async () => ACCESS) };
});

import { SecretsTab } from "./SecretsTab";

const WHY =
  "A Vault-backed secret that is Global's may name any path on the installation's Vault — only a global administrator can edit one.";

const grant = (agencyId: string, agencyName: string) => ({
  role: "admin", allScopes: false, agencyId, agencyName, scopes: [], permissions: ["manageEnvVars"], groups: [], origin: "group",
});

beforeEach(() => {
  caps = {};
  ACCESS = null;
  posts.length = 0;
  localStorage.clear();
});
afterEach(cleanup);

const open = async () => {
  render(
    <MemoryRouter>
      <SecretsTab scopeNames={["All", "Prod"]} canEdit />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("DB_PASS")).toBeTruthy());
  // Let the capabilities read land before judging what is disabled.
  await new Promise((r) => setTimeout(r, 20));
};
const rowOf = (key: string) => within(screen.getByText(key).closest("tr") as HTMLElement);
const editOf = (key: string) => rowOf(key).getByRole("button", { name: "Edit" }) as HTMLButtonElement;

describe("Secrets — Global's Vault-backed rows are a global administrator's (GC-8)", () => {
  beforeEach(() => {
    caps = { manageEnvVarsGlobal: false };
    ACCESS = { grants: [grant("ag-tax", "Tax"), grant("ag-hr", "HR")] };
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

  it("offers no Migrate to Vault: the app does not write to a store (2.5.5 SM-8)", async () => {
    await open();
    fireEvent.click(screen.getByText("DB_PASS"));
    await waitFor(() => screen.getByRole("button", { name: "Make unrevealable…" }));
    expect(screen.queryByRole("button", { name: "Migrate to Vault" })).toBeNull();
  });
});

describe("Secrets — an agency names Vault paths inside its own prefixes (LR-80)", () => {
  beforeEach(() => {
    caps = { manageEnvVarsGlobal: false };
    ACCESS = { grants: [grant("ag-tax", "Tax"), grant("ag-hr", "HR")] };
  });
  const addFor = async (agencyId: string) => {
    await open();
    fireEvent.click(screen.getByRole("button", { name: "+ Add Secret" }));
    const picker = (await waitFor(() => screen.getByRole("combobox", { name: "Agency" }))) as HTMLSelectElement;
    await waitFor(() => expect(picker.options.length).toBe(3)); // the prompt, HR, Tax — never Global
    fireEvent.change(picker, { target: { value: agencyId } });
    return screen.getByText("External (secret store)");
  };

  it("leaves an agency's own Vault-backed row editable", async () => {
    await open();
    expect(editOf("TAX_TOKEN").disabled).toBe(false);
    expect(editOf("TAX_TOKEN").title).toBe("");
  });

  it("shows the agency's prefixes, refuses a path outside them before sending, and sends one inside", async () => {
    const opt = await addFor("ag-tax");
    await waitFor(() => expect(opt.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(opt);
    const path = (await waitFor(() => screen.getByPlaceholderText("secret/data/myapp#KEY_NAME"))) as HTMLInputElement;
    expect(await screen.findByText(/Tax may name paths inside:/)).toBeTruthy();
    expect(screen.getByText("secret/data/tax")).toBeTruthy();

    fireEvent.change(screen.getByPlaceholderText("MY_SECRET_NAME"), { target: { value: "NEW_TOKEN" } });
    // A longer name is another path, not a path inside this one.
    fireEvent.change(path, { target: { value: "secret/data/tax-audit/db#x" } });
    expect((await screen.findByRole("alert")).textContent).toMatch(/outside the paths assigned to this secret's agency \(secret\/data\/tax\)/);
    fireEvent.click(screen.getByRole("button", { name: "Save Secret" }));
    await new Promise((r) => setTimeout(r, 20));
    expect(posts.filter((p) => p.path === "/env-secrets")).toHaveLength(0);

    fireEvent.change(path, { target: { value: "secret/data/tax/db#x" } });
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Save Secret" }));
    await waitFor(() => expect(posts.some((p) => p.path === "/env-secrets")).toBe(true));
    expect(posts.find((p) => p.path === "/env-secrets")!.body).toMatchObject({
      source: "vault", vaultPath: "secret/data/tax/db#x", agencyIds: ["ag-tax"],
    });
  });

  it("disables the Vault source for an agency with no prefixes, with the reason", async () => {
    const opt = await addFor("ag-hr");
    await waitFor(() => expect(opt.getAttribute("aria-disabled")).toBe("true"));
    expect(opt.getAttribute("title")).toMatch(/HR has no Vault paths assigned.*A global administrator assigns them on Scopes → Agencies/);
    expect(screen.getByRole("note").textContent).toMatch(/HR has no Vault paths assigned/);
    fireEvent.click(opt);
    // Still the stored form: no Vault path field appeared.
    expect(screen.queryByText("Vault path *")).toBeNull();
    expect(screen.getByText("Secret Value *")).toBeTruthy();
  });
});

describe("Secrets — Vault controls for a global administrator (GC-8)", () => {
  beforeEach(() => {
    caps = { manageEnvVarsGlobal: true };
  });

  it("enables Edit on a Vault-backed row", async () => {
    await open();
    expect(editOf("API_TOKEN").disabled).toBe(false);
    expect(editOf("API_TOKEN").title).toBe("");
  });

  it("lets the Vault source be chosen when adding a secret", async () => {
    await open();
    fireEvent.click(screen.getByRole("button", { name: "+ Add Secret" }));
    const opt = await waitFor(() => screen.getByText("External (secret store)"));
    expect(opt.getAttribute("aria-disabled")).toBeNull();
    expect(screen.queryByRole("note")).toBeNull();
    fireEvent.click(opt);
    await waitFor(() => expect(screen.getByText("Vault path *")).toBeTruthy());
  });
});

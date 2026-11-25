import { beforeEach, describe, expect, it, vi } from "vitest";

const post = vi.fn();
vi.mock("./client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./client")>();
  return { ...actual, api: { ...actual.api, POST: (...a: unknown[]) => post(...a) } as unknown as typeof actual.api };
});

import { VAULT_REF_HINT, checkStoreRef, refHintOf, vaultPathWhy } from "./vaultPaths";

const AWS_HINT = { label: "Secret name or ARN", placeholder: "myapp/db#password", format: "…" };

describe("vaultPaths (2.5.5 SM-10: the server judges a reference)", () => {
  beforeEach(() => post.mockReset());

  it("says why an owner can name nothing, and nothing about one reference", () => {
    const st = (prefixes: string[], ownVault: boolean) => ({ kind: "prefixes" as const, prefixes, ownVault, provider: "hashicorp", refHint: VAULT_REF_HINT });
    expect(vaultPathWhy({ kind: "unlimited" }, "Tax")).toBe("");
    expect(vaultPathWhy({ kind: "unknown" }, "Tax")).toBe("");
    expect(vaultPathWhy(st([], false), "Tax")).toMatch(/Tax has no Vault paths assigned/);
    expect(vaultPathWhy(st([], true), "Tax")).toBe("");
    expect(vaultPathWhy(st(["secret/data/tax"], false), "Tax")).toBe("");
  });

  it("labels the field from the owner's provider, else Vault's", () => {
    expect(refHintOf({ kind: "unlimited" })).toBe(VAULT_REF_HINT);
    expect(refHintOf({ kind: "prefixes", prefixes: [], ownVault: true, provider: "aws", refHint: AWS_HINT })).toBe(AWS_HINT);
  });

  it("asks the server, and passes on its reason", async () => {
    post.mockResolvedValueOnce({ data: { ok: false, code: "vault_path_not_allowed", reason: "outside (cronomicon/tax)" } });
    expect(await checkStoreRef("ag-tax", " cronomicon/fin/db ", "cronomicon/old")).toBe("outside (cronomicon/tax)");
    const [path, init] = post.mock.calls[0] as [string, { params: { path: { agencyId: string } }; body: unknown }];
    expect(path).toBe("/agencies/{agencyId}/vault/validate-ref");
    expect(init.params.path.agencyId).toBe("ag-tax");
    expect(init.body).toEqual({ ref: "cronomicon/fin/db", current: "cronomicon/old", label: "secret" });

    post.mockResolvedValueOnce({ data: { ok: true } });
    expect(await checkStoreRef("ag-tax", "cronomicon/tax/db")).toBe("");
  });

  it("asks nothing for Global, an empty reference, or no owner, and a failed request is not a refusal", async () => {
    expect(await checkStoreRef("global", "x")).toBe("");
    expect(await checkStoreRef("ag-tax", "  ")).toBe("");
    expect(await checkStoreRef("", "x")).toBe("");
    expect(post).not.toHaveBeenCalled();
    post.mockResolvedValueOnce({ error: { code: "x" } });
    expect(await checkStoreRef("ag-tax", "x")).toBe("");
  });
});

import { describe, expect, it } from "vitest";
import { vaultPathAllowed, vaultPathWhy, vaultRefPath } from "./vaultPaths";

// The form's hint must agree with the server's rule (internal/vaultpath), or it
// would refuse what the server accepts — or, worse, wave through what it won't.
describe("vault path hint (LR-80)", () => {
  const prefixes = ["secret/data/tax", "kv/fin"];

  it("matches whole segments, as written", () => {
    for (const [ref, want] of [
      ["secret/data/tax/db#password", true],
      ["secret/data/tax#token", true],
      ["kv/fin/app", true],
      ["secret/data/tax-audit/db#password", false], // a longer name is another path
      ["secret/data/ta", false],
      ["secret/data", false], // above the prefix
      ["Secret/data/tax/db", false], // case-sensitive, as Vault is
      ["/secret/data/tax/db", false], // judged as written
      ["secret/data/tax/db/", false],
      ["secret/data/tax/../fin/db", false],
      ["secret/data/tax//db", false],
      ["secret/data/tax/./db", false],
      ["secret/data/tax/%2e%2e/db", false],
      ["", false],
    ] as [string, boolean][]) {
      expect(vaultPathAllowed(ref, prefixes), ref).toBe(want);
    }
    expect(vaultPathAllowed("secret/data/tax/db", [])).toBe(false);
  });

  it("takes the path from before the last #", () => {
    expect(vaultRefPath("secret/data/tax/db#password")).toBe("secret/data/tax/db");
    expect(vaultRefPath("secret/data/tax/db")).toBe("secret/data/tax/db");
  });

  it("explains an agency with no prefixes, and a path outside them", () => {
    expect(vaultPathWhy({ kind: "unlimited" }, "Tax", "anything")).toBe("");
    expect(vaultPathWhy({ kind: "unknown" }, "Tax", "anything")).toBe("");
    expect(vaultPathWhy({ kind: "prefixes", prefixes: [] }, "Tax")).toMatch(/Tax has no Vault paths assigned/);
    expect(vaultPathWhy({ kind: "prefixes", prefixes }, "Tax", "secret/data/hr/db#x")).toMatch(
      /outside the Vault paths assigned to Tax: secret\/data\/tax, kv\/fin\./,
    );
    expect(vaultPathWhy({ kind: "prefixes", prefixes }, "Tax", "secret/data/tax/db#x")).toBe("");
    expect(vaultPathWhy({ kind: "prefixes", prefixes }, "Tax", "")).toBe("");
  });
});

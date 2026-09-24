import { describe, it, expect } from "vitest";
import { scriptDisplayPath, scriptFolder, scriptSearchText, resolveCatalogItems } from "./scripts";
import type { components } from "../api/schema";

type Script = components["schemas"]["Script"];

// Minimal Script shapes — only the fields the picker helpers read. Cast keeps the
// fixtures terse without re-declaring the whole generated type.
const s = (p: Partial<Script>): Script => p as Script;

describe("scriptDisplayPath", () => {
  it("strips the leading scripts/ root", () => {
    expect(scriptDisplayPath(s({ sourcePath: "scripts/db/backup.yaml", name: "backup-db" }))).toBe("db/backup.yaml");
  });
  it("strips stray leading/trailing slashes", () => {
    expect(scriptDisplayPath(s({ sourcePath: "scripts//ops/site.yml/", name: "x" }))).toBe("ops/site.yml");
  });
  it("falls back to name when the path is empty or just scripts/", () => {
    expect(scriptDisplayPath(s({ sourcePath: "scripts/", name: "compose-authored" }))).toBe("compose-authored");
    expect(scriptDisplayPath(s({ sourcePath: "", name: "no-file" }))).toBe("no-file");
    expect(scriptDisplayPath(s({ name: "no-path" }))).toBe("no-path");
  });
  it("leaves a path without the scripts/ prefix intact", () => {
    expect(scriptDisplayPath(s({ sourcePath: "top.sh", name: "top.sh" }))).toBe("top.sh");
  });
});

describe("scriptFolder", () => {
  it("returns the containing folder of the display path", () => {
    expect(scriptFolder(s({ sourcePath: "scripts/db/backup.yaml", name: "backup-db" }))).toBe("db");
    expect(scriptFolder(s({ sourcePath: "scripts/a/b/c.sh", name: "c" }))).toBe("a/b");
  });
  it("is empty string for a root-level script", () => {
    expect(scriptFolder(s({ sourcePath: "scripts/top.sh", name: "top.sh" }))).toBe("");
    expect(scriptFolder(s({ sourcePath: "", name: "compose" }))).toBe("");
  });
});

describe("scriptSearchText", () => {
  it("includes name, folder path, description and tags", () => {
    const text = scriptSearchText(
      s({ sourcePath: "scripts/db/backup.yaml", name: "backup-db", description: "nightly dump", tags: ["prod", "cron"] }),
    );
    expect(text).toContain("backup-db");
    expect(text).toContain("db/backup.yaml");
    expect(text).toContain("nightly dump");
    expect(text).toContain("prod");
    expect(text).toContain("cron");
  });
  it("is whitespace-tolerant when fields are missing", () => {
    expect(scriptSearchText(s({ name: "x" })).trim()).toBe("x x");
  });
});

describe("resolveCatalogItems (cap decision)", () => {
  const full = [s({ name: "a" }), s({ name: "b" })];
  const server = [s({ name: "c" })];
  it("returns the full set when not capped", () => {
    expect(resolveCatalogItems(false, "q", server, full)).toBe(full);
    expect(resolveCatalogItems(false, "", server, full)).toBe(full);
  });
  it("returns the full set when capped but the query is blank", () => {
    expect(resolveCatalogItems(true, "   ", server, full)).toBe(full);
    expect(resolveCatalogItems(true, "", null, full)).toBe(full);
  });
  it("returns the server set when capped with a query", () => {
    expect(resolveCatalogItems(true, "c", server, full)).toBe(server);
  });
  it("returns [] when capped+query but server results haven't arrived", () => {
    expect(resolveCatalogItems(true, "c", null, full)).toEqual([]);
  });
  it("routes to the server set when capped with a tag filter but no query (FU-2 Phase 2)", () => {
    expect(resolveCatalogItems(true, "", server, full, true)).toBe(server);
    // No tag filter and blank query ⇒ still the full client set.
    expect(resolveCatalogItems(true, "", server, full, false)).toBe(full);
  });
});

import { describe, it, expect } from "vitest";
import { buildTree, childrenOf, crumbsFor, parentOf, immediateCount, folderAt } from "./tree";

// Items mirror the Script shape the Scripts view feeds in: a display path (folder
// location) and a name (identity). For raw scripts they coincide; the wrapper case
// (name != path) and a Compose-authored flat item are covered too.
interface Item {
  path: string;
  name: string;
}
const items: Item[] = [
  { path: "ops-playbooks/site.yml", name: "ops-playbooks/site.yml" },
  { path: "ops-playbooks/deploy.yml", name: "ops-playbooks/deploy.yml" },
  { path: "ops-playbooks/roles/common/tasks/main.yml", name: "ops-playbooks/roles/common/tasks/main.yml" },
  { path: "db/backup.yaml", name: "backup-db" }, // wrapper: location != identity
  { path: "top.sh", name: "top.sh" }, // root-level leaf
];
const tree = () => buildTree(items, (i) => i.path, (i) => i.name);

describe("buildTree / childrenOf", () => {
  it("groups root into folders-first then leaves, alpha-sorted", () => {
    const { folders, leaves } = childrenOf(tree(), "");
    expect(folders.map((f) => f.label)).toEqual(["db", "ops-playbooks"]);
    expect(leaves.map((l) => l.label)).toEqual(["top.sh"]);
  });

  it("descends into a sub-folder and lists its files + sub-folders", () => {
    const { folders, leaves } = childrenOf(tree(), "ops-playbooks");
    expect(folders.map((f) => f.label)).toEqual(["roles"]);
    expect(leaves.map((l) => l.label)).toEqual(["deploy.yml", "site.yml"]);
  });

  it("walks arbitrarily deep nesting", () => {
    const { leaves } = childrenOf(tree(), "ops-playbooks/roles/common/tasks");
    expect(leaves.map((l) => l.label)).toEqual(["main.yml"]);
  });

  it("keeps a leaf's identity (name) distinct from its folder location (wrapper)", () => {
    const { leaves } = childrenOf(tree(), "db");
    expect(leaves).toHaveLength(1);
    expect(leaves[0].label).toBe("backup.yaml");
    expect(leaves[0].name).toBe("backup-db");
  });

  it("returns empty for an unknown path", () => {
    expect(childrenOf(tree(), "does/not/exist")).toEqual({ folders: [], leaves: [] });
    expect(folderAt(tree(), "nope")).toBeNull();
  });

  it("immediateCount counts direct children only", () => {
    const ops = folderAt(tree(), "ops-playbooks")!;
    expect(immediateCount(ops)).toBe(3); // roles/ + site.yml + deploy.yml
  });

  it("ignores empty / slash-only paths without throwing", () => {
    const t = buildTree([{ path: "", name: "x" }, { path: "/", name: "y" }], (i) => i.path, (i) => i.name);
    expect(childrenOf(t, "")).toEqual({ folders: [], leaves: [] });
  });
});

// TS-24: browse mode shares the flat table's column sort. The catalogs feed the
// already-sorted rows in and ask childrenOf to leave the leaves alone; folders
// stay alpha-first either way, since they carry none of the row columns.
describe("childrenOf leafOrder", () => {
  const unsorted: Item[] = [
    { path: "ops-playbooks/site.yml", name: "site" },
    { path: "ops-playbooks/deploy.yml", name: "deploy" },
    { path: "ops-playbooks/apply.yml", name: "apply" },
  ];
  const t = () => buildTree(unsorted, (i) => i.path, (i) => i.name);

  it('alpha-sorts leaves by default (leafOrder unset === "label")', () => {
    expect(childrenOf(t(), "ops-playbooks").leaves.map((l) => l.label)).toEqual([
      "apply.yml",
      "deploy.yml",
      "site.yml",
    ]);
    expect(childrenOf(t(), "ops-playbooks", { leafOrder: "label" }).leaves.map((l) => l.label)).toEqual([
      "apply.yml",
      "deploy.yml",
      "site.yml",
    ]);
  });

  it('leafOrder "input" keeps the caller\'s order verbatim', () => {
    expect(childrenOf(t(), "ops-playbooks", { leafOrder: "input" }).leaves.map((l) => l.label)).toEqual([
      "site.yml",
      "deploy.yml",
      "apply.yml",
    ]);
  });

  it("keeps folders alpha-first regardless of leafOrder", () => {
    const mixed = buildTree(
      [
        { path: "zeta/one.yml", name: "one" },
        { path: "root-b.yml", name: "b" },
        { path: "alpha/two.yml", name: "two" },
        { path: "root-a.yml", name: "a" },
      ],
      (i) => i.path,
      (i) => i.name,
    );
    const { folders, leaves } = childrenOf(mixed, "", { leafOrder: "input" });
    expect(folders.map((f) => f.label)).toEqual(["alpha", "zeta"]);
    // Leaves follow insertion order, NOT alpha — root-b was handed in first.
    expect(leaves.map((l) => l.label)).toEqual(["root-b.yml", "root-a.yml"]);
  });
});

describe("crumbsFor / parentOf", () => {
  it("expands a path into cumulative breadcrumb segments", () => {
    expect(crumbsFor("ops-playbooks/roles/common")).toEqual([
      { label: "ops-playbooks", path: "ops-playbooks" },
      { label: "roles", path: "ops-playbooks/roles" },
      { label: "common", path: "ops-playbooks/roles/common" },
    ]);
    expect(crumbsFor("")).toEqual([]);
  });

  it("parentOf returns the containing folder, '' at top level", () => {
    expect(parentOf("ops-playbooks/site.yml")).toBe("ops-playbooks");
    expect(parentOf("ops-playbooks/roles/common/tasks/main.yml")).toBe("ops-playbooks/roles/common/tasks");
    expect(parentOf("top.sh")).toBe("");
  });
});

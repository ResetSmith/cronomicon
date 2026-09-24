// tree.ts — turn a flat catalog of slash-pathed items into a navigable folder
// tree (scripts-browsing.md FB4). Pure and dependency-free so it is unit-testable
// and reusable across the Scripts/Jobs/Workflows/Schedules catalogs.
//
// Identity vs. location (FB1): an item's folder LOCATION comes from a display path
// (getPath), while its IDENTITY for navigation/detail comes from getName. For raw
// scripts the two coincide (name == repo-relative path); for Cronomicon wrappers the
// file lives where source_path says while its name is metadata.name — so the tree
// always groups by path and keys actions by name.

export interface TreeLeaf<T> {
  kind: "leaf";
  label: string; // last path segment, e.g. "site.yml"
  name: string; // identity (DB name), e.g. "ops-playbooks/site.yml" or "backup-db"
  path: string; // full display path, e.g. "ops-playbooks/site.yml"
  item: T;
}

export interface TreeFolder<T> {
  kind: "folder";
  label: string; // this folder's own segment, "" for the root
  path: string; // full folder path from the root, "" for the root
  folders: Map<string, TreeFolder<T>>;
  leaves: TreeLeaf<T>[];
}

function newFolder<T>(label: string, path: string): TreeFolder<T> {
  return { kind: "folder", label, path, folders: new Map(), leaves: [] };
}

const segments = (path: string): string[] => path.split("/").filter(Boolean);

// buildTree groups items into nested folders by their display path. An item whose
// path has no segments (empty) is skipped. Order of insertion does not matter;
// childrenOf sorts on read.
export function buildTree<T>(items: T[], getPath: (t: T) => string, getName: (t: T) => string): TreeFolder<T> {
  const root = newFolder<T>("", "");
  for (const item of items) {
    const path = (getPath(item) || "").replace(/^\/+|\/+$/g, "");
    const segs = segments(path);
    if (segs.length === 0) continue;
    const label = segs[segs.length - 1];
    let cur = root;
    let acc = "";
    for (const seg of segs.slice(0, -1)) {
      acc = acc ? `${acc}/${seg}` : seg;
      let next = cur.folders.get(seg);
      if (!next) {
        next = newFolder<T>(seg, acc);
        cur.folders.set(seg, next);
      }
      cur = next;
    }
    cur.leaves.push({ kind: "leaf", label, name: getName(item), path, item });
  }
  return root;
}

// folderAt resolves a folder by its path ("" => root); null when the path does
// not exist in the tree.
export function folderAt<T>(root: TreeFolder<T>, path: string): TreeFolder<T> | null {
  let cur: TreeFolder<T> | undefined = root;
  for (const seg of segments(path)) {
    cur = cur.folders.get(seg);
    if (!cur) return null;
  }
  return cur ?? null;
}

const byLabel = (a: { label: string }, b: { label: string }) =>
  a.label.localeCompare(b.label, undefined, { sensitivity: "base" });

// childrenOf returns the immediate sub-folders and leaves of `path`, folders first
// then leaves, each case-insensitively alpha-sorted. Unknown path => empty.
//
// `leafOrder: "input"` keeps the leaves in the order they were inserted (i.e. the
// order of the `items` array handed to buildTree) instead of alpha-sorting them,
// so a caller that has already ordered its rows — a column sort, say — can have
// that order survive into the browse view. Folders are ALWAYS alpha-first: they
// carry none of the row columns, so a column sort over them means nothing.
export function childrenOf<T>(
  root: TreeFolder<T>,
  path: string,
  opts?: { leafOrder?: "label" | "input" },
): { folders: TreeFolder<T>[]; leaves: TreeLeaf<T>[] } {
  const f = folderAt(root, path);
  if (!f) return { folders: [], leaves: [] };
  return {
    folders: [...f.folders.values()].sort(byLabel),
    leaves: opts?.leafOrder === "input" ? [...f.leaves] : [...f.leaves].sort(byLabel),
  };
}

// immediateCount is the number of entries shown directly inside a folder (the
// "N items" affordance) — sub-folders plus leaves, not deep descendants.
export function immediateCount<T>(f: TreeFolder<T>): number {
  return f.folders.size + f.leaves.length;
}

// crumbsFor expands a folder path into breadcrumb segments, each with the full
// path to navigate to. The root crumb is the caller's concern.
export function crumbsFor(path: string): { label: string; path: string }[] {
  const out: { label: string; path: string }[] = [];
  let acc = "";
  for (const seg of segments(path)) {
    acc = acc ? `${acc}/${seg}` : seg;
    out.push({ label: seg, path: acc });
  }
  return out;
}

// parentOf returns the containing folder path of a display path ("" when it is a
// top-level entry). Used to open the folder that holds a deep-linked leaf.
export function parentOf(path: string): string {
  const segs = segments(path);
  segs.pop();
  return segs.join("/");
}

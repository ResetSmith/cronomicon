import { Fragment, useRef, type KeyboardEvent, type ReactNode } from "react";
import { c } from "../theme";
import { buildTree, childrenOf, crumbsFor, immediateCount, parentOf, type TreeFolder, type TreeLeaf } from "../utils/tree";
import { clampPage, sliceForPage } from "../utils/pager";
import { HoverTr, Pager, usePager } from "./ui";

// FolderBrowser renders a flat catalog as a navigable folder tree (one level at a
// time, Finder/Explorer style — scripts-browsing.md FB4, breadcrumb-only per
// Q-FB4). It owns the breadcrumb + folder rows; leaf rows are delegated to a
// render prop so each catalog (Scripts first, later Jobs/Workflows/Schedules)
// keeps its own columns and expand-to-detail behaviour. Generic over the item
// type T; the tree is grouped by getPath and keyed for actions by getName.
export function FolderBrowser<T>({
  items,
  getPath,
  getName,
  path,
  onNavigate,
  rootLabel,
  header,
  colCount,
  renderLeaf,
  hideBreadcrumbAtRoot = false,
  paginate = false,
  preserveLeafOrder = false,
}: {
  items: T[];
  getPath: (t: T) => string;
  getName: (t: T) => string;
  path: string; // current folder, "" = root
  onNavigate: (p: string) => void;
  rootLabel: string; // breadcrumb root crumb, e.g. "Scripts"
  header: ReactNode; // <tr> of <th> — so leaf rows line up with the catalog columns
  colCount: number; // total columns (folder rows colSpan the rest)
  renderLeaf: (leaf: TreeLeaf<T>) => ReactNode; // returns the leaf <tr>(s)
  // At the root the breadcrumb is a single, redundant crumb (just `rootLabel`,
  // e.g. "Scripts"). When this is set, suppress the breadcrumb at root; it still
  // renders inside subfolders so the click-to-navigate-up affordance is kept.
  // Jobs/Scripts opt in; Workflows/Schedules keep the always-on breadcrumb.
  hideBreadcrumbAtRoot?: boolean;
  // Opt in to an always-on pager over the current folder level (folders + leaves)
  // so a deep/wide folder doesn't dump every row at once — the same control the
  // catalogs use in search mode. Off by default, so callers that want the full
  // level (e.g. Workflows) are unchanged.
  paginate?: boolean;
  // Render leaves in the order `items` arrives in rather than alpha by folder
  // label (TS-24). Catalogs that share their column header with the flat table
  // set this and hand in their already-sorted rows, so the active column sort
  // governs browse mode too; folders stay alpha-first either way. Off by
  // default, so a caller that wants the plain tree order is unchanged.
  preserveLeafOrder?: boolean;
}) {
  const root = buildTree(items, getPath, getName);
  const { folders, leaves } = childrenOf(root, path, preserveLeafOrder ? { leafOrder: "input" } : undefined);
  const crumbs = crumbsFor(path);
  const atRoot = crumbs.length === 0;

  // The current level, folders-first then leaves — the same order childrenOf
  // already renders. When `paginate` is set we page this combined list; both
  // entry kinds carry a `kind` discriminant, so one slice covers a page that
  // straddles the folder/leaf boundary. The hook runs unconditionally (hooks
  // rule); it only drives the slice and the control when paginating.
  const level: (TreeFolder<T> | TreeLeaf<T>)[] = [...folders, ...leaves];
  const total = level.length;
  const pager = usePager();
  // Reset to the first page the instant the folder changes — synchronously, during
  // render, so a newly-opened level never flashes its old (clamped) page for a
  // frame the way a post-render effect would. Between navigations clampPage reels a
  // shrinking level (a tighter filter, a deleted row) back into range instead of
  // stranding the user past the last page. This is React's "adjust state when a
  // prop changes" pattern: compare against a ref, correct this render, then sync
  // the stored page so the next render agrees.
  const prevPathRef = useRef(path);
  const pathChanged = prevPathRef.current !== path;
  if (pathChanged) prevPathRef.current = path;
  const page = pathChanged ? 0 : clampPage(pager.page, total, pager.pageSize);
  if (pathChanged && pager.page !== 0) pager.setPage(0);
  const rendered = paginate ? sliceForPage(level, page, pager.pageSize) : level;
  // Suppress the lone redundant root crumb for opted-in catalogs; subfolders
  // always keep the breadcrumb so the navigate-up affordance survives.
  const showBreadcrumb = !(hideBreadcrumbAtRoot && atRoot);

  // Keyboard: Backspace/Escape goes up one folder (no dead-ends). Bubbles from a
  // focused folder row; ignored at the root.
  const onContainerKey = (e: KeyboardEvent<HTMLDivElement>) => {
    if ((e.key === "Backspace" || e.key === "Escape") && path) {
      e.preventDefault();
      onNavigate(parentOf(path));
    }
  };

  const crumb = (label: string, target: string, isLast: boolean) =>
    isLast ? (
      <span key={target} style={{ color: c.text, fontWeight: 600 }}>{label}</span>
    ) : (
      <span key={target}>
        <button
          onClick={() => onNavigate(target)}
          style={{ background: "none", border: "none", padding: 0, color: c.primary, cursor: "pointer", font: "inherit" }}
        >
          {label}
        </button>
        <span style={{ color: c.textMuted, margin: "0 6px" }}>/</span>
      </span>
    );

  // The level table, extracted so a paginating caller can wrap just the table in a
  // horizontal-scroll container (keeping the pager anchored) while non-paginating
  // callers (e.g. Workflows) still render the bare table, layout unchanged.
  const tableEl = (
    <table style={{ width: "100%", borderCollapse: "collapse", fontSize: c.fontSm }}>
      <thead>{header}</thead>
      <tbody>
        {rendered.map((entry) =>
          entry.kind === "folder" ? (
            <HoverTr
              key={`dir:${entry.path}`}
              onClick={() => onNavigate(entry.path)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  onNavigate(entry.path);
                }
              }}
              tabIndex={0}
              role="button"
              ariaLabel={`Open folder ${entry.label}`}
              hoverTint={c.panelHover}
              style={{ borderBottom: `1px solid ${c.border}`, cursor: "pointer" }}
            >
              <td style={cellStyle}>
                <span style={{ display: "inline-flex", alignItems: "center", gap: 8, fontWeight: 600 }}>
                  <span aria-hidden>📁</span>
                  {entry.label}
                </span>
              </td>
              <td style={{ ...cellStyle, textAlign: "right", color: c.textSec }} colSpan={Math.max(1, colCount - 1)}>
                {immediateCount(entry)} item{immediateCount(entry) === 1 ? "" : "s"} <span aria-hidden>›</span>
              </td>
            </HoverTr>
          ) : (
            <Fragment key={`leaf:${entry.name}`}>{renderLeaf(entry)}</Fragment>
          ),
        )}
      </tbody>
    </table>
  );

  return (
    <div onKeyDown={onContainerKey}>
      {showBreadcrumb && (
        // paddingLeft matches the table cell's horizontal padding so the
        // breadcrumb lines up with the first column instead of sitting flush
        // against the card's left edge.
        <nav aria-label="Folder path" style={{ display: "flex", alignItems: "center", flexWrap: "wrap", fontSize: c.fontSm, marginBottom: 10, paddingLeft: 16 }}>
          {crumb(rootLabel, "", atRoot)}
          {crumbs.map((cr, i) => crumb(cr.label, cr.path, i === crumbs.length - 1))}
        </nav>
      )}

      {folders.length === 0 && leaves.length === 0 ? (
        <div style={{ color: c.textMuted, fontSize: c.fontSm, padding: "12px 4px" }}>This folder is empty.</div>
      ) : paginate ? (
        // Leaf rows can be far wider than the card (e.g. the Jobs catalog's 11
        // columns), so the TABLE gets its own horizontal scroll while the <Pager>
        // below stays anchored at the card edge instead of scrolling off with it.
        <div style={{ overflowX: "auto" }}>{tableEl}</div>
      ) : (
        tableEl
      )}

      {paginate && total > 0 && <Pager pager={pager} page={page} total={total} noun="items" />}
    </div>
  );
}

const cellStyle: React.CSSProperties = { padding: "10px 16px", verticalAlign: "top" };

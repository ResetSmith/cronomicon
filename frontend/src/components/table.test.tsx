// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { act, cleanup, fireEvent, render, renderHook, within } from "@testing-library/react";

// CO-2.4 (the columns plan) — the column model.
//
// The CO-Q10 merge gets the most coverage here for a specific reason: every
// other rule in this band fails loudly in review, and that one fails SILENTLY,
// months later, and only for users who happen to have a stored order. A column
// added in a later release that never appears for existing users is the exact
// bug these four cases exist to prevent.

import { ColumnsMenu, TableHead, mergeOrder, minWidthOf, renderCells, useTableColumns, type TableColumn } from "./table";

type Row = { id: number; name: string; host: string };

const ROW: Row = { id: 1, name: "nightly-db-backup", host: "db-01" };

// A miniature of a real adopter: a pinned opener, three loose middle columns,
// and a pinned trailing actions cell.
const COLS = (): TableColumn<Row>[] => [
  // `expand` deliberately declares NO menuLabel, so the key fallback is covered;
  // `actions` declares one, which is what a real adopter does.
  { key: "expand", label: "", width: 44, fixed: true, pin: "first", cell: () => "▶" },
  { key: "name", label: "Name", sortKey: "name", width: 180, cell: (r) => r.name },
  { key: "host", label: "Host", width: 130, cell: (r) => r.host },
  { key: "status", label: "Status", width: 100, fixed: true, cell: () => "Idle" },
  { key: "actions", label: "", menuLabel: "Actions", width: 120, fixed: true, pin: "last", cell: () => "⋯" },
];

const keysOf = (cols: TableColumn<Row>[]) => cols.map((col) => col.key);
const store = (v: unknown) => localStorage.setItem("cols:t", JSON.stringify(v));

beforeEach(() => localStorage.clear());
afterEach(cleanup);

describe("mergeOrder — forward compatibility (CO-Q10)", () => {
  const DEFAULTS = ["a", "b", "c", "d"];

  it("honours a stored order", () => {
    expect(mergeOrder(DEFAULTS, ["d", "c", "b", "a"])).toEqual(["d", "c", "b", "a"]);
  });

  it("drops a stored key that no longer names a column", () => {
    // A column removed in a later release must not leave a ghost slot behind.
    expect(mergeOrder(DEFAULTS, ["a", "removed-last-release", "b", "c", "d"])).toEqual(["a", "b", "c", "d"]);
  });

  it("re-inserts a NEW default key at its default index, not at the end", () => {
    // "c" is new: the stored order predates it. It must land where its author
    // put it. Appending it instead would be the same bug in a quieter costume —
    // the column appears, but in the wrong place, for exactly the users who
    // already had a preference.
    expect(mergeOrder(DEFAULTS, ["a", "b", "d"])).toEqual(["a", "b", "c", "d"]);
  });

  it("re-inserts a new key even when the stored order is a permutation", () => {
    expect(mergeOrder(DEFAULTS, ["d", "b", "a"])).toEqual(["d", "b", "c", "a"]);
  });

  it("falls back to defaults on an empty, missing or corrupt stored order", () => {
    expect(mergeOrder(DEFAULTS, undefined)).toEqual(DEFAULTS);
    expect(mergeOrder(DEFAULTS, [])).toEqual(DEFAULTS);
  });

  it("de-duplicates a repeated stored key", () => {
    // A corrupt value that names one column twice would otherwise render that
    // column twice and throw off every count derived from the list.
    expect(mergeOrder(DEFAULTS, ["a", "a", "b"])).toEqual(["a", "b", "c", "d"]);
  });
});

describe("useTableColumns", () => {
  const setup = () => renderHook(() => useTableColumns("t", COLS()));

  it("defaults to the declared order with nothing hidden", () => {
    const { result } = setup();
    expect(keysOf(result.current.visible)).toEqual(["expand", "name", "host", "status", "actions"]);
    expect(result.current.hidden.size).toBe(0);
  });

  it("survives a corrupt stored value rather than taking the table down", () => {
    localStorage.setItem("cols:t", "{not json");
    expect(keysOf(setup().result.current.visible)).toEqual(["expand", "name", "host", "status", "actions"]);
    store({ order: "not-an-array", hidden: 7 });
    expect(keysOf(setup().result.current.visible)).toEqual(["expand", "name", "host", "status", "actions"]);
  });

  it("moves a loose column and persists the new order", () => {
    const { result } = setup();
    act(() => result.current.move("host", -1));
    expect(keysOf(result.current.visible)).toEqual(["expand", "host", "name", "status", "actions"]);
    expect(JSON.parse(localStorage.getItem("cols:t")!).order).toEqual([
      "expand", "host", "name", "status", "actions",
    ]);
  });

  it("refuses to move a pinned column (CO-Q4)", () => {
    const { result } = setup();
    act(() => result.current.move("expand", 1));
    act(() => result.current.move("actions", -1));
    expect(keysOf(result.current.visible)).toEqual(["expand", "name", "host", "status", "actions"]);
  });

  it("refuses to move a loose column PAST a pin", () => {
    const { result } = setup();
    // name is already adjacent to the pinned opener; moving it left would put a
    // column before a column pinned "first", which is not a preference the model
    // may express.
    act(() => result.current.move("name", -1));
    expect(keysOf(result.current.visible)[0]).toBe("expand");
    act(() => result.current.move("status", 1));
    expect(keysOf(result.current.visible).at(-1)).toBe("actions");
  });

  it("hides and un-hides a loose column", () => {
    const { result } = setup();
    act(() => result.current.setHidden("host", true));
    expect(keysOf(result.current.visible)).toEqual(["expand", "name", "status", "actions"]);
    // `all` still lists it — that is what the CO-3 menu needs in order to offer
    // it back. A hidden column with no menu row could never be recovered.
    expect(keysOf(result.current.all)).toContain("host");
    act(() => result.current.setHidden("host", false));
    expect(keysOf(result.current.visible)).toContain("host");
  });

  it("refuses to hide a pinned column (CO-Q4)", () => {
    const { result } = setup();
    act(() => result.current.setHidden("expand", true));
    act(() => result.current.setHidden("actions", true));
    expect(keysOf(result.current.visible)).toHaveLength(5);
  });

  it("ignores a stored hide on a column that has since been pinned", () => {
    // Otherwise the preference is unreachable: no menu row can clear it,
    // because a pinned row renders without controls.
    store({ hidden: ["expand"] });
    expect(keysOf(setup().result.current.visible)).toContain("expand");
  });

  it("re-applies pins over a stored order that predates them", () => {
    store({ order: ["name", "actions", "expand", "host", "status"] });
    const keys = keysOf(setup().result.current.visible);
    expect(keys[0]).toBe("expand");
    expect(keys.at(-1)).toBe("actions");
  });

  it("reset clears the stored preference", () => {
    const { result } = setup();
    act(() => result.current.setHidden("host", true));
    act(() => result.current.reset());
    expect(keysOf(result.current.visible)).toHaveLength(5);
    expect(localStorage.getItem("cols:t")).toBeNull();
  });

  it("minWidth sums the VISIBLE widths only", () => {
    const { result } = setup();
    expect(result.current.minWidth).toBe(44 + 180 + 130 + 100 + 120);
    act(() => result.current.setHidden("host", true));
    // Hiding a column must free its width, or the table refuses to compress into
    // the space that hiding it was supposed to open up.
    expect(result.current.minWidth).toBe(44 + 180 + 100 + 120);
  });
});

describe("renderCells / TableHead", () => {
  const sort = {
    sortDir: "asc" as const,
    toggle: () => {},
    isActive: (k: string) => k === "name",
    ariaSort: (k: string) => (k === "name" ? ("ascending" as const) : undefined),
  };
  const cw = { widths: {}, setWidth: () => {} };

  const renderTable = (cols: TableColumn<Row>[], sortEnabled = true) =>
    render(
      <table>
        <thead>
          <TableHead columns={cols} sort={sort} cw={cw} sortEnabled={sortEnabled} />
        </thead>
        <tbody>
          <tr>{renderCells(cols, ROW)}</tr>
        </tbody>
      </table>,
    );

  it("renders one <td> per visible column, in order", () => {
    const { container } = renderTable(COLS());
    const cells = container.querySelectorAll("tbody td");
    expect(cells).toHaveLength(5);
    expect(cells[1].textContent).toBe("nightly-db-backup");
    expect(cells[2].textContent).toBe("db-01");
  });

  it("leaves NO <td> in the DOM for a hidden column (CO-Q9)", () => {
    // Not `display: none`: the rendered count, the colSpan arithmetic and what a
    // screen reader announces must all agree, and three sources of truth for
    // "how many columns" is how CO-2.0's bugs got in.
    const visible = COLS().filter((col) => col.key !== "host");
    const { container } = renderTable(visible);
    expect(container.querySelectorAll("tbody td")).toHaveLength(4);
    expect(container.textContent).not.toContain("db-01");
    expect(container.querySelector('[style*="display: none"]')).toBeNull();
  });

  it("gives a sortable head a click target and aria-sort; a plain one neither", () => {
    const { container } = renderTable(COLS());
    const q = within(container);
    const heads = q.getAllByRole("columnheader");
    const name = heads.find((h) => h.textContent?.startsWith("Name"))!;
    expect(name.getAttribute("aria-sort")).toBe("ascending");
    expect(name.getAttribute("title")).toBe("Sort by Name");
    // Host declares no sortKey, so it must not look or behave clickable.
    const host = heads.find((h) => h.textContent?.startsWith("Host"))!;
    expect(host.getAttribute("aria-sort")).toBeNull();
    expect(host.getAttribute("title")).toBeNull();
  });

  it("drops every sort affordance when sortEnabled is false (browse mode, TS-8)", () => {
    // Browse mode shares this header but sorts nothing — a column sort is
    // meaningless against the folder tree's folders-first grouping.
    const { container } = renderTable(COLS(), false);
    const name = within(container).getAllByRole("columnheader").find((h) => h.textContent?.startsWith("Name"))!;
    expect(name.getAttribute("aria-sort")).toBeNull();
    expect(name.getAttribute("title")).toBeNull();
    // ...and no caret, so the header does not advertise a sort it will not do.
    expect(name.textContent).toBe("Name");
  });

  it("applies a per-row style over every cell (the expanded-row border case)", () => {
    const cols = COLS();
    const { container } = render(
      <table>
        <tbody>
          <tr>{renderCells(cols, ROW, { rowStyle: { borderBottom: "none" } })}</tr>
        </tbody>
      </table>,
    );
    for (const td of container.querySelectorAll("td")) {
      // The LONGHAND, not `style.borderBottom`: jsdom expands the shorthand and
      // then re-serialises it as its width component ("medium"), so the shorthand
      // read is not the value that was set.
      expect((td as HTMLElement).style.borderBottomStyle).toBe("none");
    }
  });
});

describe("minWidthOf", () => {
  it("sums declared widths and tolerates a column with none", () => {
    expect(minWidthOf(COLS())).toBe(574);
    expect(minWidthOf([{ key: "x", label: "X", cell: () => null }])).toBe(0);
  });
});

// CO-3 — the Columns menu. It is the ONLY writer of the order/visibility
// preference the model shipped inert in CO-2, so these tests are also the first
// end-to-end exercise of that storage.
describe("ColumnsMenu (CO-3)", () => {
  // A harness rather than a bare render: the menu drives a real useTableColumns,
  // and asserting on the TABLE it rearranges is the point — a menu that updates
  // only itself would pass every assertion made against the popover alone.
  function Harness() {
    const cols = useTableColumns("t", COLS());
    return (
      <div>
        <ColumnsMenu cols={cols} cw={{ widths: {}, setWidth: () => {}, reset: () => {} }} />
        <table>
          <tbody>
            <tr>{renderCells(cols.visible, ROW)}</tr>
          </tbody>
        </table>
      </div>
    );
  }
  const openMenu = () => {
    const r = render(<Harness />);
    fireEvent.click(r.getByRole("button", { name: /Columns/ }));
    return r;
  };
  const bodyKeys = (c: HTMLElement) =>
    Array.from(c.querySelectorAll("tbody td")).map((td) => (td.textContent ?? "").trim());

  it("opens, lists every column including hidden ones, and closes on Escape", () => {
    const r = openMenu();
    const dialog = r.getByRole("dialog", { name: /arrange columns/i });
    expect(within(dialog).getByText("Name")).toBeTruthy();
    // The unlabeled trailing cell is named by menuLabel, not left blank — a menu
    // row reading "" names nothing.
    expect(within(dialog).getByText("Actions")).toBeTruthy();
    // ...and an unlabeled column with no menuLabel falls back to its key rather
    // than rendering an unclickable blank row.
    expect(within(dialog).getByText("expand")).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(r.queryByRole("dialog")).toBeNull();
  });

  it("hides a column, and the TABLE loses that cell", () => {
    const r = openMenu();
    expect(bodyKeys(r.container)).toContain("db-01");
    fireEvent.click(r.getByLabelText("Show Host"));
    expect(bodyKeys(r.container)).not.toContain("db-01");
    // CO-Q9 — gone from the DOM, not display:none.
    expect(r.container.querySelectorAll("tbody td")).toHaveLength(4);
  });

  it("counts hidden columns on the TRIGGER", () => {
    // A preference silently in force on a shared workstation is otherwise
    // indistinguishable from missing data.
    const r = openMenu();
    fireEvent.click(r.getByLabelText("Show Host"));
    expect(r.getByRole("button", { name: /Columns · 1 hidden/ })).toBeTruthy();
    fireEvent.click(r.getByLabelText("Show Status"));
    expect(r.getByRole("button", { name: /Columns · 2 hidden/ })).toBeTruthy();
  });

  it("reorders via the ▲/▼ buttons — the keyboard path", () => {
    const r = openMenu();
    expect(bodyKeys(r.container).slice(0, 3)).toEqual(["▶", "nightly-db-backup", "db-01"]);
    fireEvent.click(r.getByRole("button", { name: "Move Host up" }));
    expect(bodyKeys(r.container).slice(0, 3)).toEqual(["▶", "db-01", "nightly-db-backup"]);
  });

  it("disables an arrow whose move a pin would refuse (FX-7)", () => {
    const r = openMenu();
    // `name` is the first LOOSE column, sitting directly under the pinned
    // opener. It is not at index 0, so a position-only rule left this arrow
    // live — and clicking it did nothing, because move() refuses to step into
    // a pin. A control that cannot act is disabled, not silently inert.
    const up = r.getByRole("button", { name: "Move Name up" }) as HTMLButtonElement;
    expect(up.disabled).toBe(true);
    // Its neighbour below is genuinely movable, so the pair is not just uniformly off.
    expect((r.getByRole("button", { name: "Move Name down" }) as HTMLButtonElement).disabled).toBe(false);
    // Same at the trailing edge: `status` is the last LOOSE column, sitting
    // above the pinned actions cell.
    expect((r.getByRole("button", { name: "Move Status down" }) as HTMLButtonElement).disabled).toBe(true);
    expect((r.getByRole("button", { name: "Move Status up" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("keeps the arrows and the model's refusals in agreement", () => {
    // The regression that motivated canMove(): the arrow said yes where move()
    // said no. Asserting they agree column-by-column is what stops the two
    // rules drifting apart again.
    // A fresh mount per case, rather than moving a column and putting it back:
    // each assertion then judges the DEFAULT layout, and one wrong restore
    // cannot cascade into the cases after it.
    for (const label of ["up", "down"] as const) {
      for (const col of ["Name", "Host", "Status"]) {
        cleanup();
        localStorage.clear();
        const r = openMenu();
        const btn = r.getByRole("button", { name: `Move ${col} ${label}` }) as HTMLButtonElement;
        // Read BEFORE the click. React updates this same node in place, so a
        // move that lands the column against a pin flips its own arrow to
        // disabled — reading afterwards would assert the new position's rule
        // against the old position's outcome.
        const wasDisabled = btn.disabled;
        const before = bodyKeys(r.container).join("|");
        fireEvent.click(btn);
        const moved = bodyKeys(r.container).join("|") !== before;
        expect({ col, label, moved }).toEqual({ col, label, moved: !wasDisabled });
      }
    }
  });

  it("renders a pinned column greyed, named, and with no controls (CO-Q4)", () => {
    const r = openMenu();
    const dialog = r.getByRole("dialog");
    expect(within(dialog).getByText("Always last")).toBeTruthy();
    // The pin is VISIBLE rather than mysterious: no move buttons, and the
    // checkbox is disabled rather than absent.
    expect(within(dialog).queryByRole("button", { name: /Move Actions/ })).toBeNull();
    expect((within(dialog).getByLabelText("Show Actions") as HTMLInputElement).disabled).toBe(true);
  });

  it("Reset to defaults clears BOTH the order and the widths", () => {
    let widthsCleared = false;
    function ResetHarness() {
      const cols = useTableColumns("t", COLS());
      return (
        <div>
          <ColumnsMenu cols={cols} cw={{ widths: {}, setWidth: () => {}, reset: () => { widthsCleared = true; } }} />
          <table><tbody><tr>{renderCells(cols.visible, ROW)}</tr></tbody></table>
        </div>
      );
    }
    const r = render(<ResetHarness />);
    fireEvent.click(r.getByRole("button", { name: /Columns/ }));
    fireEvent.click(r.getByLabelText("Show Host"));
    expect(localStorage.getItem("cols:t")).toBeTruthy();

    fireEvent.click(r.getByRole("button", { name: "Reset to defaults" }));
    expect(localStorage.getItem("cols:t")).toBeNull();
    expect(r.container.querySelectorAll("tbody td")).toHaveLength(5);
    // One mental model ("put the table back"), so one button clears both keys.
    expect(widthsCleared).toBe(true);
  });

  it("persists what it changed, so the next mount reads it back", () => {
    const r = openMenu();
    fireEvent.click(r.getByLabelText("Show Host"));
    fireEvent.click(r.getByRole("button", { name: "Move Status up" }));
    cleanup();

    const again = render(<Harness />);
    expect(bodyKeys(again.container)).not.toContain("db-01");
    // Status stepped over the HIDDEN host column, so the table's visible order
    // is unchanged by that move even though the menu's list did change. That is
    // correct and deliberate: the menu lists every column, hidden ones included,
    // so a step is always visible WHERE THE GESTURE HAPPENED. Making a step skip
    // hidden columns instead would mean the same click moves one position or
    // three depending on what is hidden.
    expect(bodyKeys(again.container)).toEqual(["▶", "nightly-db-backup", "Idle", "⋯"]);

    // A second step clears the hidden column and now does move the table.
    fireEvent.click(again.getByRole("button", { name: /Columns/ }));
    fireEvent.click(again.getByRole("button", { name: "Move Status up" }));
    expect(bodyKeys(again.container)).toEqual(["▶", "Idle", "nightly-db-backup", "⋯"]);
  });
});

describe("useTableColumns.moveTo (the drag target)", () => {
  const setup = () => renderHook(() => useTableColumns("t", COLS()));

  it("moves to an absolute index", () => {
    const { result } = setup();
    act(() => result.current.moveTo("status", 1));
    expect(keysOf(result.current.visible)).toEqual(["expand", "status", "name", "host", "actions"]);
  });

  it("CLAMPS into the loose band rather than refusing an overshoot", () => {
    // A drag that overshoots the pins should land at the boundary: a drop that
    // silently does nothing reads as a broken drag.
    const { result } = setup();
    act(() => result.current.moveTo("status", -5));
    expect(keysOf(result.current.visible)[1]).toBe("status");
    act(() => result.current.moveTo("name", 99));
    // Last LOOSE slot — never past the pinned actions column.
    expect(keysOf(result.current.visible).at(-1)).toBe("actions");
    expect(keysOf(result.current.visible).at(-2)).toBe("name");
  });

  it("refuses to move a pinned column", () => {
    const { result } = setup();
    act(() => result.current.moveTo("actions", 0));
    expect(keysOf(result.current.visible).at(-1)).toBe("actions");
  });
});

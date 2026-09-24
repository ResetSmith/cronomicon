// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SearchSelect, type SearchSelectProps } from "./SearchSelect";

afterEach(cleanup);

type Item = { id: string; folder?: string };
const ITEMS: Item[] = [{ id: "alpha" }, { id: "beta" }, { id: "gamma" }];

function renderSS(props: Partial<SearchSelectProps<Item>> = {}) {
  const onChange = props.onChange ?? vi.fn();
  const merged: SearchSelectProps<Item> = {
    value: "",
    onChange,
    items: ITEMS,
    getKey: (i) => i.id,
    getSearchText: (i) => i.id,
    renderOption: (i) => i.id,
    ...props,
  };
  const utils = render(<SearchSelect<Item> {...merged} />);
  return { ...utils, onChange, combobox: screen.getByRole("combobox") };
}
const optionText = () => screen.queryAllByRole("option").map((o) => o.textContent);

describe("SearchSelect — open/close + filtering", () => {
  it("is closed until focused, then lists every option", () => {
    const { combobox } = renderSS();
    expect(screen.queryByRole("listbox")).toBeNull();
    fireEvent.focus(combobox);
    expect(optionText()).toEqual(["alpha", "beta", "gamma"]);
  });

  it("type-to-filter narrows the list (client-side)", () => {
    const { combobox } = renderSS();
    fireEvent.focus(combobox);
    fireEvent.change(combobox, { target: { value: "be" } });
    expect(optionText()).toEqual(["beta"]);
  });

  it("Escape closes the panel", () => {
    const { combobox } = renderSS();
    fireEvent.focus(combobox);
    expect(screen.getByRole("listbox")).toBeTruthy();
    fireEvent.keyDown(combobox, { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
  });
});

describe("SearchSelect — selection", () => {
  it("ArrowDown then Enter selects the active option", () => {
    const { combobox, onChange } = renderSS();
    fireEvent.focus(combobox); // active seeds to 0 (alpha)
    fireEvent.keyDown(combobox, { key: "ArrowDown" }); // → beta
    fireEvent.keyDown(combobox, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("beta", { id: "beta" });
  });

  it("clicking an option selects it", () => {
    const { combobox, onChange } = renderSS();
    fireEvent.focus(combobox);
    fireEvent.click(screen.getByText("gamma"));
    expect(onChange).toHaveBeenCalledWith("gamma", { id: "gamma" });
  });

  it("opens with the current selection highlighted (immediate Enter confirms it)", () => {
    const { combobox, onChange } = renderSS({ value: "gamma", selectedItem: { id: "gamma" } });
    fireEvent.focus(combobox);
    fireEvent.keyDown(combobox, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("gamma", { id: "gamma" });
  });
});

describe("SearchSelect — grouping", () => {
  const GROUPED: Item[] = [
    { id: "a", folder: "db" },
    { id: "b", folder: "ops" },
    { id: "c", folder: "db" },
  ];
  it("renders non-selectable folder headers and arrow-nav skips them", () => {
    const { combobox, onChange } = renderSS({ items: GROUPED, groupBy: (i) => i.folder ?? "" });
    fireEvent.focus(combobox);
    // headers are presentation-only and alpha-sorted
    expect(screen.getAllByRole("presentation").map((h) => h.textContent)).toEqual(["db", "ops"]);
    // first navigable entry is an option (a), not the "db" header → Enter picks "a"
    fireEvent.keyDown(combobox, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("a", { id: "a", folder: "db" });
  });
});

describe("SearchSelect — current selection always rendered (§2.5)", () => {
  it("prepends the selected item even when it is not in the loaded list", () => {
    const { combobox } = renderSS({
      value: "alpha",
      selectedItem: { id: "alpha" },
      items: [{ id: "beta" }, { id: "gamma" }], // alpha is off-page
    });
    fireEvent.focus(combobox);
    expect(optionText()).toContain("alpha");
  });

  it("shows a 'missing' marker for a dangling value", () => {
    renderSS({ value: "ghost", selectedItem: null, missing: true });
    expect(screen.getByText("missing")).toBeTruthy();
    expect((screen.getByRole("combobox") as HTMLInputElement).value).toBe("ghost");
  });
});

describe("SearchSelect — creatable", () => {
  it("offers a create row for a novel value and commits it on Enter", () => {
    const onChange = vi.fn();
    const { combobox } = renderSS({ creatable: true, onChange });
    fireEvent.focus(combobox);
    fireEvent.change(combobox, { target: { value: "zeta" } });
    expect(screen.getByText('Use "zeta"')).toBeTruthy();
    fireEvent.keyDown(combobox, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("zeta", null);
  });

  it("suppresses the create row when the typed value exactly matches an option", () => {
    const { combobox } = renderSS({ creatable: true });
    fireEvent.focus(combobox);
    fireEvent.change(combobox, { target: { value: "beta" } });
    expect(screen.queryByText('Use "beta"')).toBeNull();
    expect(optionText()).toEqual(["beta"]);
  });
});

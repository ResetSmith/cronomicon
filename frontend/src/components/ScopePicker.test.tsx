// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ScopePicker } from "./ScopePicker";

afterEach(cleanup);

// "envonly" stands for a scope that exists only as an env-var scope name — the
// suggestion list is a union, and every entry renders the same way.
const SUGGESTIONS = ["Dev", "Prod", "envonly"];

function renderSP(props: Partial<React.ComponentProps<typeof ScopePicker>> = {}) {
  const onChange = props.onChange ?? vi.fn();
  render(<ScopePicker value="" onChange={onChange} suggestions={SUGGESTIONS} {...props} />);
  return { onChange, combobox: screen.getByRole("combobox") };
}
const options = () => screen.getAllByRole("option");
const optionFor = (scope: string) => options().find((o) => o.textContent?.startsWith(scope))!;

describe("ScopePicker — option rows", () => {
  // Until 2.1.0 a row carried the scope's "supported run types" after a dash.
  // The feature is gone; an option is the scope name and nothing else.
  it("renders every option as the bare scope name", () => {
    const { combobox } = renderSP();
    fireEvent.focus(combobox);
    expect(optionFor("Prod").textContent).toBe("Prod");
    expect(optionFor("Dev").textContent).toBe("Dev");
    expect(optionFor("envonly").textContent).toBe("envonly");
  });
});

describe("ScopePicker — selection & creation", () => {
  it("commits a chosen suggestion as its scope string", () => {
    const { combobox, onChange } = renderSP();
    fireEvent.focus(combobox);
    fireEvent.click(optionFor("Prod"));
    expect(onChange).toHaveBeenCalledWith("Prod");
  });

  it("offers a create row for a novel scope and commits the typed value (JC23)", () => {
    const onChange = vi.fn();
    const { combobox } = renderSP({ onChange });
    fireEvent.focus(combobox);
    fireEvent.change(combobox, { target: { value: "Staging" } });
    expect(screen.getByText('Use "Staging"')).toBeTruthy();
    fireEvent.keyDown(combobox, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("Staging");
  });

  it("suppresses the create row when the typed value matches a known scope", () => {
    const { combobox } = renderSP();
    fireEvent.focus(combobox);
    fireEvent.change(combobox, { target: { value: "Prod" } });
    expect(screen.queryByText('Use "Prod"')).toBeNull();
  });
});

describe("ScopePicker — custom current value", () => {
  it("renders a custom value that isn't a known suggestion (closed label + pinned row)", () => {
    const { combobox } = renderSP({ value: "weird-custom", suggestions: ["Prod"] });
    expect((combobox as HTMLInputElement).value).toBe("weird-custom");
    fireEvent.focus(combobox);
    expect(options().some((o) => o.textContent?.includes("weird-custom"))).toBe(true);
  });
});

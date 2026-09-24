// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ScopePicker } from "./ScopePicker";
import type { components } from "../api/schema";

type Scope = components["schemas"]["Scope"];

afterEach(cleanup);

const SUGGESTIONS = ["Dev", "Prod", "envonly"];
// Prod has capability types; Dev has empty types; "envonly" is in the suggestion union
// (e.g. an env-var-only scope) but absent from scopes[] → renders bare.
const SCOPES = [
  { scope: "Prod", capability: { types: ["bash", "ansible"] } },
  { scope: "Dev", capability: { types: [] } },
] as unknown as Scope[];

function renderSP(props: Partial<React.ComponentProps<typeof ScopePicker>> = {}) {
  const onChange = props.onChange ?? vi.fn();
  render(<ScopePicker value="" onChange={onChange} suggestions={SUGGESTIONS} scopes={SCOPES} {...props} />);
  return { onChange, combobox: screen.getByRole("combobox") };
}
const options = () => screen.getAllByRole("option");
const optionFor = (scope: string) => options().find((o) => o.textContent?.startsWith(scope))!;

describe("ScopePicker — capability labels", () => {
  it("labels a scope with its capability types and renders typeless/env-var-only scopes bare", () => {
    const { combobox } = renderSP();
    fireEvent.focus(combobox);
    expect(optionFor("Prod").textContent).toContain("bash, ansible");
    expect(optionFor("Dev").textContent).toBe("Dev"); // empty types → no dash
    expect(optionFor("envonly").textContent).toBe("envonly"); // absent from scopes[] → bare
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
    const { combobox } = renderSP({ value: "weird-custom", suggestions: ["Prod"], scopes: [] });
    expect((combobox as HTMLInputElement).value).toBe("weird-custom");
    fireEvent.focus(combobox);
    expect(options().some((o) => o.textContent?.includes("weird-custom"))).toBe(true);
  });
});

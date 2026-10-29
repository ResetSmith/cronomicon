// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";

let ctx: { agencyName: string; onOverview: boolean } | null = null;
vi.mock("../../agency", () => ({ useAgencyOptional: () => ctx }));

import { agencyWideLabel, pickedAgencyName, useAgencyWide } from "./scopeOption";

const label = (owner?: string, picked?: string) => renderHook(() => useAgencyWide(owner, picked)).result.current;

// "All" makes an agency-wide row: every scope of ONE agency. The option names
// that agency so it is not read as "every agency" (2.5.4).
describe("useAgencyWide", () => {
  it("names the agency in use for a new row", () => {
    ctx = { agencyName: "Operations", onOverview: false };
    const w = label(undefined);
    expect(w.optionLabel("All")).toBe("Agency-wide (every scope in Operations)");
    expect(w.optionLabel("prod")).toBe("prod");
  });

  it("names the row's owner when editing", () => {
    ctx = { agencyName: "Operations", onOverview: false };
    expect(label("Reporting").optionLabel("All")).toBe("Agency-wide (every scope in Reporting)");
  });

  it("names no agency on the overview or outside the switcher", () => {
    ctx = { agencyName: "All agencies", onOverview: true };
    expect(label(undefined).optionLabel("All")).toBe("Agency-wide (every scope)");
    ctx = null;
    expect(label(undefined).optionLabel("All")).toBe("Agency-wide (every scope)");
  });

  it("names the agency picked on the form outside the switcher", () => {
    ctx = null;
    expect(label(undefined, "Reporting").optionLabel("All")).toBe("Agency-wide (every scope in Reporting)");
    // The row's owner still wins on an edit.
    expect(label("Operations", "Reporting").optionLabel("All")).toBe("Agency-wide (every scope in Operations)");
  });

  it("ignores the picker inside the switcher, the overview included", () => {
    ctx = { agencyName: "Operations", onOverview: false };
    expect(label(undefined, "Reporting").optionLabel("All")).toBe("Agency-wide (every scope in Operations)");
    // On the overview the picker's agency is the overview itself ("All agencies").
    ctx = { agencyName: "All agencies", onOverview: true };
    expect(label(undefined, "All agencies").optionLabel("All")).toBe("Agency-wide (every scope)");
  });

  it("hints at the reach, and at what narrows it", () => {
    ctx = { agencyName: "Operations", onOverview: false };
    const hint = label(undefined).hint("secret");
    expect(hint).toContain("Every run in Operations that references this secret receives it");
    expect(hint).toContain("unless a row in the run's own scope has the same key");
    ctx = { agencyName: "All agencies", onOverview: true };
    expect(label(undefined).hint("variable")).toContain("Every run in the agency that references this variable");
  });
});

describe("agencyWideLabel", () => {
  it("names the row's agency in its details", () => {
    expect(agencyWideLabel("Operations")).toBe("Agency-wide (every scope in Operations)");
    expect(agencyWideLabel(undefined)).toBe("Agency-wide (every scope)");
  });
});

describe("pickedAgencyName", () => {
  const agencies = [
    { id: "ag-ops", name: "Operations" },
    { id: "ag-rep", name: "Reporting" },
  ];
  it("names the selected agency", () => {
    expect(pickedAgencyName({ agencies, selected: "ag-rep" })).toBe("Reporting");
  });
  it("is empty when nothing is chosen yet", () => {
    expect(pickedAgencyName({ agencies, selected: "" })).toBe("");
  });
});

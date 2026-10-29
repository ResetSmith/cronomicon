// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";

let ctx: { agencyName: string; onOverview: boolean } | null = null;
vi.mock("../../agency", () => ({ useAgencyOptional: () => ctx }));

import { useScopeOptionLabel } from "./scopeOption";

// "All" makes a global row: every scope of ONE agency. The option names that
// agency so it is not read as "every agency".
describe("useScopeOptionLabel", () => {
  it("names the agency in use for a new row", () => {
    ctx = { agencyName: "Operations", onOverview: false };
    const { result } = renderHook(() => useScopeOptionLabel(undefined));
    expect(result.current("All")).toBe("All scopes in Operations (global)");
    expect(result.current("prod")).toBe("prod");
  });

  it("names the row's owner when editing", () => {
    ctx = { agencyName: "Operations", onOverview: false };
    const { result } = renderHook(() => useScopeOptionLabel("Reporting"));
    expect(result.current("All")).toBe("All scopes in Reporting (global)");
  });

  it("names no agency on the overview or outside the switcher", () => {
    ctx = { agencyName: "All agencies", onOverview: true };
    expect(renderHook(() => useScopeOptionLabel(undefined)).result.current("All")).toBe("All scopes (global)");
    ctx = null;
    expect(renderHook(() => useScopeOptionLabel(undefined)).result.current("All")).toBe("All scopes (global)");
  });
});

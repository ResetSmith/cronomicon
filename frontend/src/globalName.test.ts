// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { globalAgencyName, setGlobalAgencyName } from "./globalName";
import { rowFlagWhy } from "./views/envvars/ui";

// 2.5.2 GR — the built-in agency's name is read, never written: once it is
// renamed, "Global" is an ordinary agency's name, and the built-in agency's
// rows say its new one.
describe("the built-in agency's name (2.5.2 GR)", () => {
  afterEach(() => setGlobalAgencyName(undefined));

  it("is Global until the provider says otherwise", () => {
    expect(globalAgencyName()).toBe("Global");
    setGlobalAgencyName("Central");
    expect(globalAgencyName()).toBe("Central");
    setGlobalAgencyName("  ");
    expect(globalAgencyName()).toBe("Global");
  });

  it("is what a row's owner is compared with", () => {
    setGlobalAgencyName("Central");
    expect(rowFlagWhy(false, "Central", "secret")).toBe("This secret is Central's — only a global administrator (a role on every agency) can change it.");
    // An agency that took the old name is an agency like any other.
    expect(rowFlagWhy(false, "Global", "secret")).toBe("This secret belongs to Global — only that agency's administrators can change it.");
  });
});

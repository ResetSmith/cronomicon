// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { AgencyCell } from "./ui";

// The Agencies cell (2.5.1 SG-44): the owner chip left the name column, so a
// row whose owner is not among the agencies it lists — a row several agencies
// shared before 2.3.0, which is the built-in agency's and, since 2.5.3, used by
// no run (it runs nothing) — says whose it is here. Anywhere else the owner is
// one of the chips already.

afterEach(cleanup);

describe("AgencyCell", () => {
  it("names the owner of a shared row that does not list it", () => {
    render(<AgencyCell names={["FIN", "TAX"]} owner="Global" />);
    const owner = screen.getByText("Global (owner)");
    expect(owner.getAttribute("title")).toMatch(/Global runs nothing, so no run uses it\. They read it; a global administrator changes it, or moves it to one of them \(Move agency\)/);
    expect(screen.getByText("FIN")).toBeTruthy();
    expect(screen.getByText("TAX")).toBeTruthy();
  });

  it("does not repeat an owner that is one of the agencies listed", () => {
    render(<AgencyCell names={["FIN"]} owner="FIN" />);
    expect(screen.queryByText(/\(owner\)/)).toBeNull();
    expect(screen.getByText("FIN")).toBeTruthy();
  });

  it("says a row in no agency is damage, whoever owns it", () => {
    render(<AgencyCell names={[]} owner="Global" />);
    expect(screen.getByText("no agency")).toBeTruthy();
    expect(screen.queryByText(/\(owner\)/)).toBeNull();
  });
});

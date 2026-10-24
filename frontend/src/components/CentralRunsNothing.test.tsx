// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.3 CA-12: Central is the installation's administration and runs nothing.
// Its work pages say so in one empty state; while it still holds rows of a
// page's kind from before 2.5.3 (CA-17, the central_holds_work findings), the
// page lists them under a banner, for their way out.

type Ctx = { agency: string; agencyName: string; onOverview: boolean; choices: { id: string; name: string; kind: string }[] };
const { inUse } = vi.hoisted(() => ({ inUse: { current: null as null | Ctx } }));
vi.mock("../agency", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../agency")>();
  return { ...actual, useAgencyOptional: () => inUse.current };
});

let NOTICES: { kind: string; subject: string; resolvedAt?: string }[] = [];
const gets: string[] = [];
vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        gets.push(path);
        return path === "/notices" ? { data: NOTICES } : { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});

import { CentralGate } from "./CentralRunsNothing";

const CHOICES = [
  { id: "overview", name: "All agencies", kind: "overview" },
  { id: "global", name: "Central", kind: "agency" },
  { id: "ag-ops", name: "Operations", kind: "agency" },
];
const central = (choices = CHOICES): Ctx => ({ agency: "global", agencyName: "Central", onOverview: false, choices });

const show = (holds?: string[]) =>
  render(
    <MemoryRouter>
      <CentralGate holds={holds}>
        <div>the page</div>
      </CentralGate>
    </MemoryRouter>,
  );

beforeEach(() => {
  inUse.current = null;
  NOTICES = [];
  gets.length = 0;
});
afterEach(cleanup);

describe("CentralGate", () => {
  it("shows the page in an agency, and asks nothing", async () => {
    inUse.current = { agency: "ag-ops", agencyName: "Operations", onOverview: false, choices: CHOICES };
    show(["job"]);
    expect(screen.getByText("the page")).toBeTruthy();
    expect(gets).not.toContain("/notices");
  });

  it("is not Central on the All-agencies Dashboard", () => {
    inUse.current = { ...central(), onOverview: true };
    show(["job"]);
    expect(screen.getByText("the page")).toBeTruthy();
  });

  it("says Central runs nothing, with no page, when it holds none of the kind", async () => {
    inUse.current = central();
    show(["job"]);
    expect(await screen.findByText("Central runs nothing. Switch to an agency to work.")).toBeTruthy();
    expect(screen.queryByText("the page")).toBeNull();
  });

  it("says so on a page that lists nothing Central can hold", async () => {
    inUse.current = central();
    show();
    expect(await screen.findByText("Central runs nothing. Switch to an agency to work.")).toBeTruthy();
    expect(gets).not.toContain("/notices");
  });

  it("lists what Central still holds of the kind, under a banner that says why and where the way out is", async () => {
    inUse.current = central();
    NOTICES = [{ kind: "central_holds_work", subject: "central:job" }];
    show(["job"]);
    expect(await screen.findByText("the page")).toBeTruthy();
    expect(screen.getByTestId("central-leftovers").textContent).toMatch(/left from before 2\.5\.3 and no longer runs/);
    expect(screen.getByRole("link", { name: "Notices" }).getAttribute("href")).toBe("/notices");
  });

  it("does not count a resolved finding, nor one of another kind", async () => {
    inUse.current = central();
    NOTICES = [
      { kind: "central_holds_work", subject: "central:job", resolvedAt: "2026-10-24T00:00:00Z" },
      { kind: "central_holds_work", subject: "central:secret" },
      { kind: "orphaned", subject: "central:job" },
    ];
    show(["job"]);
    expect(await screen.findByText("Central runs nothing. Switch to an agency to work.")).toBeTruthy();
  });

  // CA-16: before the first agency exists there is none to switch to.
  it("says there is no agency yet, and where one is made, when there is none", async () => {
    inUse.current = central(CHOICES.filter((ch) => ch.id !== "ag-ops"));
    show(["job"]);
    expect(await screen.findByText("There is no agency yet.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "All-agencies Dashboard" }).getAttribute("href")).toBe("/?agency=overview");
  });
});


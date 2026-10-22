// @vitest-environment jsdom
import type { ReactElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.2 OP-2 — a run's detail says who to call about it and opens its job (or
// workflow) in one click: the two things an operator mid-incident had to go to
// the agency's Jobs page to find.

let RUN: Record<string, unknown> = {};
let WF_RUN: Record<string, unknown> = {};

const { GET } = vi.hoisted(() => ({ GET: vi.fn() }));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET } as unknown as typeof actual.api };
});

import { RunDetail } from "./ExecutionsTab";
import { WorkflowRunDetail } from "../workflows/WorkflowRunDetail";

beforeEach(() => {
  GET.mockReset();
  GET.mockImplementation(async (path: string) => {
    if (path === "/runs/{traceId}") return { data: RUN };
    if (path === "/workflow-runs/{traceId}") return { data: WF_RUN };
    return { data: "" };
  });
  RUN = { traceId: "t-1", jobName: "payroll-export", jobUid: "u-1", ownerAgency: "ag-a", status: "failure" };
  WF_RUN = { traceId: "wt-1", workflowName: "month-end", workflowUid: "w-1", status: "failure", steps: [] };
});
afterEach(cleanup);

const renderIn = (el: ReactElement) => render(<MemoryRouter>{el}</MemoryRouter>);

describe("the run detail: who to call, and its job (2.5.2 OP-2)", () => {
  it("shows the job's contact, marks it critical, and opens the job in the run's agency", async () => {
    RUN = { ...RUN, annotation: { contact: "Payroll on-call, ext. 4410", critical: true } };
    renderIn(<RunDetail traceId="t-1" />);
    expect(await screen.findByText("Payroll on-call, ext. 4410")).toBeTruthy();
    expect(screen.getByText("Critical")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open job" }).getAttribute("href")).toBe("/jobs?job=u-1&agency=ag-a");
  });

  it("says None set for a job with no contact, without a Critical mark", async () => {
    RUN = { ...RUN, annotation: { contact: "", critical: false } };
    renderIn(<RunDetail traceId="t-1" />);
    expect(await screen.findByText("None set")).toBeTruthy();
    expect(screen.queryByText("Critical")).toBeNull();
  });

  it("falls back to the agency's contact, marked as the agency's (2.5.2 OP-28)", async () => {
    RUN = { ...RUN, annotation: { contact: "", critical: false, agencyContact: "Payroll desk" } };
    renderIn(<RunDetail traceId="t-1" />);
    expect(await screen.findByText(/Payroll desk/)).toBeTruthy();
    expect(screen.getByText("(agency)")).toBeTruthy();
    expect(screen.queryByText("None set")).toBeNull();
  });

  it("shows neither for a run with no job", async () => {
    RUN = { traceId: "t-2", jobName: "adhoc", jobUid: null, status: "success" };
    renderIn(<RunDetail traceId="t-2" />);
    await screen.findByText("t-2");
    expect(screen.queryByText("Contact")).toBeNull();
    expect(screen.queryByRole("link", { name: "Open job" })).toBeNull();
  });
});

describe("the workflow run detail: who to call, and its workflow (2.5.2 OP-2)", () => {
  it("shows the workflow's contact and opens the workflow", async () => {
    WF_RUN = { ...WF_RUN, annotation: { contact: "Finance ops", critical: false } };
    renderIn(<WorkflowRunDetail traceId="wt-1" />);
    expect(await screen.findByText("Finance ops")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open workflow" }).getAttribute("href")).toBe("/workflows?workflow=w-1");
  });

  it("leaves both out where it sits under its own workflow's row", async () => {
    WF_RUN = { ...WF_RUN, annotation: { contact: "Finance ops", critical: false } };
    renderIn(<WorkflowRunDetail traceId="wt-1" showOwner={false} />);
    await screen.findByText("No step-level detail recorded for this run.");
    expect(screen.queryByText("Finance ops")).toBeNull();
    expect(screen.queryByRole("link", { name: "Open workflow" })).toBeNull();
  });
});

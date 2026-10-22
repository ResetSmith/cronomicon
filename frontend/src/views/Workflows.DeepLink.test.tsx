// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

// 2.5.2 OP-3, the Workflows twin of Jobs' ?job= (written with it: the recurring
// defect in this area is a change made to one catalog and not the other).

const WORKFLOWS = [
  { id: 1, uid: "w-month", name: "month-end", source: "git", status: "failure", disabled: false, steps: [], sourcePath: "workflows/finance/month-end.yml" },
  { id: 2, uid: "w-tidy", name: "nightly-tidy", source: "git", status: "success", disabled: false, steps: [], sourcePath: "workflows/nightly-tidy.yml" },
];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/workflows") return { data: WORKFLOWS };
        if (path === "/workflows/{workflowId}") return { data: WORKFLOWS[0] };
        if (path === "/me") return { data: { email: "op@ex.com", roles: ["operator"] } };
        return { data: [] };
      }),
      POST: vi.fn(async () => ({ data: {} })),
      PUT: vi.fn(async () => ({ data: {} })),
      PATCH: vi.fn(async () => ({ data: {} })),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false,
      apprise: false,
      compose: false,
      manageRoles: false,
      configureApp: false,
      manageEnvVars: false,
      publishSchedule: false,
      triggerJobs: true,
      killJobs: true,
    })),
  };
});

import { Workflows } from "./Workflows";
import { AuthProvider } from "../auth";

function Where() {
  const loc = useLocation();
  return <span data-testid="where">{loc.search}</span>;
}

const renderAt = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <AuthProvider>
        <Where />
        <Workflows />
      </AuthProvider>
    </MemoryRouter>,
  );
const where = () => new URLSearchParams(screen.getByTestId("where").textContent ?? "");

afterEach(cleanup);

describe("Workflows ?workflow= (2.5.2 OP-3)", () => {
  it("opens the workflow's folder, expands its row, and drops the parameter", async () => {
    renderAt("/workflows?workflow=w-month&agency=ag-a");
    await waitFor(() => expect(document.getElementById("workflow-detail-1")).toBeTruthy());
    await waitFor(() => expect(where().get("workflow")).toBeNull());
    expect(where().get("path")).toBe("finance");
    expect(where().get("agency")).toBe("ag-a");
  });

  it("says so when the workflow is not in this agency's list", async () => {
    renderAt("/workflows?workflow=w-elsewhere");
    expect(await screen.findByText("That workflow is not in this agency's list.")).toBeTruthy();
    await waitFor(() => expect(where().get("workflow")).toBeNull());
  });
});

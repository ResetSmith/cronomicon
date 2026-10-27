// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// 2.5.3 CA-30, CA-33: a workflow's own Forbid and Allow calendars. The save is a
// full replace, and a list left out is cleared (until CA5 the editor did not
// send them): an edit sends them as loaded, in either editor, and the picker
// says a step's job calendars do not apply inside the workflow.

const WF = {
  id: 3,
  name: "nightly",
  source: "cronomicon",
  ownerAgency: "ag-ops",
  steps: [{ name: "job-a" }],
  schedules: [],
  forbidCalendars: ["holidays"],
  allowCalendars: ["weekdays"],
};

const writes: { method: string; path: string; body: Record<string, unknown> }[] = [];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/workflows/{workflowId}") return { data: WF };
        if (path === "/jobs") return { data: [{ name: "job-a", source: "cronomicon", uid: "u-a" }] };
        if (path === "/calendars")
          return { data: { items: [{ name: "holidays", ownerAgency: "ag-ops" }, { name: "weekdays", ownerAgency: "ag-ops" }] } };
        return { data: [] };
      }),
      POST: vi.fn(async (path: string, opts: { body: Record<string, unknown> }) => {
        if (path === "/workflows/validate") return { data: { ok: true, errors: [] } };
        writes.push({ method: "POST", path, body: opts.body });
        return { data: { id: 3 }, response: { ok: true, status: 200 } };
      }),
      PUT: vi.fn(async (path: string, opts: { body: Record<string, unknown> }) => {
        writes.push({ method: "PUT", path, body: opts.body });
        return { data: {}, response: { ok: true, status: 200 } };
      }),
      DELETE: vi.fn(async () => ({ data: {}, response: { ok: true, status: 200 } })),
    } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ compose: true, composeUnbound: true })),
  };
});

import { WorkflowEditor } from "./WorkflowEditor";

// The canvas editor (React Flow) measures itself; jsdom has no ResizeObserver.
globalThis.ResizeObserver ??= class {
  observe() {}
  unobserve() {}
  disconnect() {}
} as unknown as typeof ResizeObserver;

beforeEach(() => {
  writes.length = 0;
});
afterEach(cleanup);

const renderAt = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <WorkflowEditor />
    </MemoryRouter>,
  );
const field = () => screen.getByText("Calendars (optional)").parentElement!.parentElement as HTMLElement;
const wfWrite = () => writes.find((w) => w.path === "/workflows/{workflowId}" || w.path === "/workflows");

describe("WorkflowEditor — the workflow's own calendars (2.5.3 CA-30)", () => {
  it("sends them as loaded, and what the picker changes", async () => {
    renderAt("/workflow-editor?id=3");
    // The editor and its canvas are heavy: under a full run they mount slowly.
    await screen.findByText("Calendars (optional)", {}, { timeout: 10000 });
    expect(within(field()).getByText(/A step's job calendars do not apply inside the workflow/)).toBeTruthy();
    const allow = within(field()).getByText(/^Allow — only on days in$/).parentElement as HTMLElement;
    fireEvent.click(within(allow).getByRole("button", { name: /^weekdays/ })); // off
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(wfWrite()).toBeTruthy(), { timeout: 10000 });
    expect(wfWrite()!.body).toMatchObject({ forbidCalendars: ["holidays"], allowCalendars: [] });
  });

  it("sends them on a save that does not touch them", async () => {
    renderAt("/workflow-editor?id=3");
    // The editor and its canvas are heavy: under a full run they mount slowly.
    await screen.findByText("Calendars (optional)", {}, { timeout: 10000 });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(wfWrite()).toBeTruthy(), { timeout: 10000 });
    expect(wfWrite()!.body).toMatchObject({ forbidCalendars: ["holidays"], allowCalendars: ["weekdays"] });
  });
});

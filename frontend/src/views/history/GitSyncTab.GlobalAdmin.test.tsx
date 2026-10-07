// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// GC (v2.2.2, gate closing) — "Sync now" on History → Git Sync posts to
// /git/sync, which re-reads the whole definitions repo (every agency's files) and
// is therefore a global administrator's: configureApp on every agency.
//
// The log is readable by anyone who can open History, and until 2.2.2 the button
// was live for all of them — a viewer clicked it and read "Sync failed:
// forbidden". It stays for everyone and is disabled with the reason (FX-7),
// both in the toolbar and in the empty log's call to action.

let global = false;
const { GET, POST } = vi.hoisted(() => ({
  // The empty envelope carries `items: []` alongside `totalItems: 0` — the two
  // must agree or the pager and the empty state disagree.
  GET: vi.fn(async () => ({ data: { items: [], totalItems: 0, totalPages: 1, page: 1, pageSize: 25 } })),
  POST: vi.fn(async () => ({ data: {} })),
}));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: { GET, POST } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({
      vault: false, apprise: false, compose: false, composeUnbound: false, manageRoles: false,
      // The flat flag is deliberately TRUE in both cases: an administrator of
      // one agency holds configureApp and is still refused.
      configureApp: true, manageEnvVars: false, publishSchedule: false, triggerJobs: false,
      killJobs: false, unrestricted: false,
      configureAppGlobal: global,
    })),
  };
});

import { GitSyncTab } from "./GitSyncTab";

const WHY = "Only a global administrator (a role on every agency) can start a sync from GitLab.";

beforeEach(() => {
  global = false;
  GET.mockClear();
  POST.mockClear();
  localStorage.clear();
});
afterEach(cleanup);

const open = async () => {
  render(
    <MemoryRouter>
      <GitSyncTab />
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByText("No Git sync events yet")).toBeTruthy());
  // Let the capabilities read land before judging what is disabled.
  await new Promise((r) => setTimeout(r, 20));
  return screen.getAllByRole("button", { name: /Sync now/ }) as HTMLButtonElement[];
};

describe("Git Sync — Sync now needs a global administrator (GC)", () => {
  it("disables both Sync now buttons with the reason for an administrator of one agency", async () => {
    const buttons = await open();
    // The toolbar's and the empty state's.
    expect(buttons.length).toBe(2);
    for (const b of buttons) {
      expect(b.disabled).toBe(true);
      expect(b.title).toBe(WHY);
    }
    for (const b of buttons) fireEvent.click(b);
    expect(POST).not.toHaveBeenCalled();
  });

  it("enables both for a global administrator, and the click reaches POST /git/sync", async () => {
    global = true;
    const buttons = await open();
    expect(buttons.length).toBe(2);
    for (const b of buttons) {
      expect(b.disabled).toBe(false);
      expect(b.title).toBe("");
    }
    fireEvent.click(buttons[0]);
    await waitFor(() => expect(POST).toHaveBeenCalledTimes(1));
    expect((POST.mock.calls[0] as unknown as [string])[0]).toBe("/git/sync");
  });
});

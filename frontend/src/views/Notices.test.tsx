// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// LR-85 — one inbox. What the page owes: every notice the server returned
// (it has already filtered them to the agencies the caller administers), under
// its agency with Global's first; what each is and where its remedy is made;
// and a dismissal that goes to the server and says it does not fix anything.

let NOTICES: unknown[] = [];
const posts: { path: string; body: unknown }[] = [];
let dismissError: unknown = null;

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => (path === "/notices" ? { data: NOTICES } : { data: [] })),
      POST: vi.fn(async (path: string, init?: { body?: unknown }) => {
        posts.push({ path, body: init?.body });
        if (dismissError) return { error: dismissError };
        NOTICES = NOTICES.filter((n) => !(init?.body as { ids: string[] }).ids.includes((n as { id: string }).id));
        return { data: {} };
      }),
    } as unknown as typeof actual.api,
  };
});

import { Notices } from "./Notices";

const notice = (id: string, kind: string, agencyId: string, agencyName: string, detail: string) => ({
  id, kind, agencyId, agencyName, subject: id, detail, firstSeenAt: "2026-10-07T00:00:00Z", lastSeenAt: "2026-10-07T00:00:00Z",
});

beforeEach(() => {
  posts.length = 0;
  dismissError = null;
  NOTICES = [
    notice("n-tax", "target_host_outside_scope", "ag-tax", "Tax", "The job nightly names host db9, which is not in scope tax-hosts."),
    notice("n-legacy", "legacy_placement", "global", "Global", "The runner legacy serves Finance, Tax and is owned by Global."),
    notice("n-new", "a_kind_from_a_later_release", "global", "Global", "Something this client has never heard of."),
  ];
});
afterEach(cleanup);

const open = () =>
  render(
    <MemoryRouter>
      <Notices />
    </MemoryRouter>,
  );

describe("Notices (LR-85)", () => {
  it("groups notices by agency, Global first, and says what each is and where to act", async () => {
    open();
    const sections = await waitFor(() => {
      const s = screen.getAllByRole("region");
      expect(s).toHaveLength(2);
      return s;
    });
    expect(sections.map((s) => s.getAttribute("aria-label"))).toEqual(["Notices for Global", "Notices for Tax"]);

    const global = within(sections[0]);
    expect(global.getByText("A runner that serves agencies it is not owned by")).toBeTruthy();
    expect(global.getByText(/The runner legacy serves Finance, Tax/)).toBeTruthy();
    expect((global.getByRole("link", { name: "Open Runners" }) as HTMLAnchorElement).getAttribute("href")).toBe("/runners");
    // A kind this client does not know is shown by its detail, never dropped.
    expect(global.getByText("Notice")).toBeTruthy();
    expect(global.getByText("Something this client has never heard of.")).toBeTruthy();

    const tax = within(sections[1]);
    expect(tax.getByText("A job's target host is not in its scope")).toBeTruthy();
    expect((tax.getByRole("link", { name: "Open Jobs" }) as HTMLAnchorElement).getAttribute("href")).toBe("/jobs");
  });

  it("dismisses through the server, one notice, and says it fixes nothing", async () => {
    open();
    const row = (await screen.findByText(/The job nightly names host db9/)).closest("div")!.parentElement!.parentElement!;
    // Each Dismiss says which notice it is for: three buttons with one name are
    // indistinguishable to a screen reader.
    const btn = within(row).getByRole("button", { name: "Dismiss: A job's target host is not in its scope, Tax" }) as HTMLButtonElement;
    expect(btn.title).toMatch(/does not fix the condition, and it comes back if the condition does/);
    fireEvent.click(btn);
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]).toEqual({ path: "/notices/dismiss", body: { ids: ["n-tax"] } });
    await waitFor(() => expect(screen.queryByText(/The job nightly names host db9/)).toBeNull());
    // The others are untouched.
    expect(screen.getByText(/The runner legacy serves Finance, Tax/)).toBeTruthy();
  });

  it("shows the server's refusal and keeps the notice", async () => {
    dismissError = { code: "forbidden", message: "you do not administer the agency this notice belongs to" };
    open();
    fireEvent.click((await screen.findAllByRole("button", { name: /^Dismiss: / }))[0]);
    expect(await screen.findByText(/you do not administer the agency this notice belongs to/)).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /^Dismiss: / })).toHaveLength(3);
  });

  it("says so when nothing needs attention", async () => {
    NOTICES = [];
    open();
    expect(await screen.findByText(/Nothing needs attention/)).toBeTruthy();
  });
});

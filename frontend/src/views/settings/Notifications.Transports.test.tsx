// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// Phase K (VF-16) — the Alert Destinations section is gone, and the "last sent"
// signal it carried has moved onto the transports that actually send.
//
// The old signal was `alert_destinations.last_fired_at`, stamped by the
// dispatcher on EVERY enabled destination whose type matched. No destination was
// ever a delivery target — routing came from notification_config plus each
// rule's channels — so a row reading "last fired 3 minutes ago" could only ever
// mean "something sent over this transport". Now it says that, about the thing
// that sends.

let config: Record<string, unknown> = { provider: "apprise" };
let testReport: unknown = { results: [] };
let postError: unknown = null;
const posts: { path: string; body: unknown }[] = [];
const gets: string[] = [];

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        gets.push(path);
        if (path === "/settings/notifications") return { data: config };
        if (path === "/alerts") return { data: [] };
        // A GET for a route this section no longer owns would be a leftover.
        return { data: [] };
      }),
      PUT: vi.fn(async () => ({ data: config })),
      POST: vi.fn(async (path: string, opts?: { body?: unknown }) => {
        posts.push({ path, body: opts?.body });
        if (postError) return { error: postError };
        return { data: testReport };
      }),
      DELETE: vi.fn(async () => ({ data: {} })),
    } as unknown as typeof actual.api,
  };
});

import { NotificationsSection } from "./Notifications";

const renderSection = async () => {
  render(<NotificationsSection />);
  // Wait for the CONFIG, not just the card. "Run Notifications" is the card's
  // title and renders before GET /settings/notifications resolves, so every
  // assertion about lastSent used to race that fetch — passing only because the
  // promise usually settled first. Under a fuller test run it sometimes did not,
  // and the negative assertions ("says nothing at all…") were worse than flaky:
  // they passed trivially whenever the data had not landed. "Send test" is
  // disabled until `form` is populated (Notifications.tsx:176), which makes it
  // the honest readiness marker.
  await waitFor(() => expect(screen.getByText("Run Notifications")).toBeTruthy());
  await waitFor(() => expect((screen.getByText("Send test").closest("button") as HTMLButtonElement).disabled).toBe(false));
};

beforeEach(() => {
  posts.length = 0;
  gets.length = 0;
  testReport = { results: [] };
  postError = null;
});
afterEach(cleanup);

describe("Notifications — destinations removed (K-1)", () => {
  it("renders no Alert Destinations section", async () => {
    config = { provider: "apprise" };
    await renderSection();
    expect(screen.queryByText("Alert Destinations")).toBeNull();
    // The two cards that remain, and nothing between them.
    expect(screen.getByText("Run Notifications")).toBeTruthy();
    expect(screen.getByText("Alert Rules")).toBeTruthy();
    // The destination type picker's copy is the specific thing that lied
    // ("delivered to all active sessions") — it must be gone, not reworded.
    expect(screen.queryByText(/delivered to all active sessions/)).toBeNull();
    expect(screen.queryByText(/Add Destination/)).toBeNull();
  });
});

describe("Notifications — last sent per transport (K-2)", () => {
  it("says nothing at all when no transport has ever sent", async () => {
    config = { provider: "apprise" };
    await renderSection();
    // Absent, not "never" in a row of its own — an empty state for a signal
    // nobody has generated yet is noise on a page that is mostly forms.
    expect(screen.queryByText(/last send failed/)).toBeNull();
    expect(screen.queryByText(/^sent/)).toBeNull();
  });

  it("names the transport that sent, and distinguishes a failure from a success", async () => {
    config = {
      provider: "apprise",
      lastSent: {
        email: { at: "2026-07-28T10:00:00Z", status: "ok" },
        apprise: { at: "2026-07-28T11:30:00Z", status: "error" },
      },
    };
    await renderSection();
    expect(screen.getByText("Email")).toBeTruthy();
    expect(screen.getByText("sent")).toBeTruthy();
    // "failed" and "never" are different facts and must not collapse into one.
    expect(screen.getByText("last send failed")).toBeTruthy();
  });

  it("shows only the transport that has sent, when just one has", async () => {
    config = { provider: "smtp", lastSent: { email: { at: "2026-07-28T10:00:00Z", status: "ok" } } };
    await renderSection();
    expect(screen.getByText("Email")).toBeTruthy();
    expect(screen.getByText("sent")).toBeTruthy();
    expect(screen.queryByText("last send failed")).toBeNull();
  });
});

describe("Notifications — the test-send button (K-5)", () => {
  it("posts no body, so no recipient can be injected from the client", async () => {
    config = { provider: "apprise" };
    posts.length = 0;
    testReport = { results: [{ transport: "email", sent: true, skipped: false, detail: "Sent to 1 recipient(s)." }] };
    await renderSection();
    fireEvent.click(screen.getByText("Send test"));
    await waitFor(() => expect(posts.length).toBe(1));
    expect(posts[0].path).toBe("/settings/notifications/test");
    // Every address comes from stored config — the request carries nothing.
    expect(posts[0].body).toBeUndefined();
  });

  it("reports each transport separately, distinguishing skipped from failed", async () => {
    config = { provider: "apprise" };
    testReport = {
      results: [
        { transport: "email", sent: false, skipped: true, detail: "No SMTP host is configured." },
        { transport: "apprise", sent: false, skipped: false, detail: "dial tcp: connection refused" },
      ],
    };
    await renderSection();
    fireEvent.click(screen.getByText("Send test"));
    await waitFor(() => expect(screen.getByText("not attempted")).toBeTruthy());
    // "Not attempted" and "failed" are different facts: the first is a
    // configuration gap the operator can close, the second is a broken transport.
    expect(screen.getByText("failed")).toBeTruthy();
    expect(screen.getByText(/No SMTP host is configured/)).toBeTruthy();
    expect(screen.getByText(/connection refused/)).toBeTruthy();
    // And it says which settings were actually used.
    expect(screen.getByText(/uses the/)).toBeTruthy();
  });

  it("re-reads the config after a send so the last-sent line reflects it", async () => {
    config = { provider: "apprise" };
    gets.length = 0;
    testReport = { results: [{ transport: "email", sent: true, skipped: false, detail: "Sent to 1 recipient(s)." }] };
    await renderSection();
    const before = gets.filter((p) => p === "/settings/notifications").length;
    fireEvent.click(screen.getByText("Send test"));
    await waitFor(() =>
      expect(gets.filter((p) => p === "/settings/notifications").length).toBeGreaterThan(before),
    );
  });

  it("surfaces a server error instead of silently doing nothing", async () => {
    config = { provider: "apprise" };
    postError = { code: "test_failed", message: "could not read notification config" };
    await renderSection();
    fireEvent.click(screen.getByText("Send test"));
    await waitFor(() => expect(screen.getByText(/Test failed/)).toBeTruthy());
    postError = null;
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// 2.5.2 OP2.1 — Settings → Notifications shows the address alerts link to, how
// it got there, and lets a global administrator change or remove it.

let STORED: Record<string, string> = {};
const { GET, PUT } = vi.hoisted(() => ({ GET: vi.fn(), PUT: vi.fn() }));

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return { ...actual, api: { GET, PUT } as unknown as typeof actual.api };
});

import { AlertLinkCard } from "./Notifications";

beforeEach(() => {
  STORED = { address: "https://cron.example.gov", source: "detected", by: "root@example.gov", at: "2026-10-22T19:00:00Z" };
  GET.mockReset();
  PUT.mockReset();
  GET.mockImplementation(async () => ({ data: STORED }));
  PUT.mockImplementation(async (_p: string, opts: { body: { address: string } }) => {
    STORED = { address: opts.body.address, source: "set", by: "root@example.gov", at: "2026-10-22T19:05:00Z" };
    return { data: STORED };
  });
});
afterEach(cleanup);

describe("the alert link address card (2.5.2 OP2.1)", () => {
  it("shows the address and that it was detected, and whose browser it came from", async () => {
    render(<AlertLinkCard canWrite />);
    expect((await screen.findByTestId("alert-link-address")).textContent).toBe("https://cron.example.gov");
    expect(screen.getByText(/detected from root@example.gov's browser/)).toBeTruthy();
  });

  it("changes it", async () => {
    render(<AlertLinkCard canWrite />);
    await screen.findByTestId("alert-link-address");
    fireEvent.click(screen.getByRole("button", { name: "Change" }));
    const input = screen.getByLabelText("Address") as HTMLInputElement;
    expect(input.value).toBe("https://cron.example.gov");
    fireEvent.change(input, { target: { value: "https://ops.example.gov/cronomicon" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(PUT).toHaveBeenCalled());
    expect((PUT.mock.calls[0][1] as { body: { address: string } }).body.address).toBe("https://ops.example.gov/cronomicon");
    await waitFor(() => expect(screen.getByTestId("alert-link-address").textContent).toBe("https://ops.example.gov/cronomicon"));
    expect(screen.getByText(/set by root@example.gov/)).toBeTruthy();
  });

  it("removes it: alerts then carry no link", async () => {
    render(<AlertLinkCard canWrite />);
    await screen.findByTestId("alert-link-address");
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(PUT).toHaveBeenCalled());
    expect((PUT.mock.calls[0][1] as { body: { address: string } }).body.address).toBe("");
    await waitFor(() => expect(screen.getByTestId("alert-link-address").textContent).toBe("None: alerts carry no link"));
  });

  it("is read-only for someone who is not a global administrator", async () => {
    render(<AlertLinkCard canWrite={false} />);
    await screen.findByTestId("alert-link-address");
    expect((screen.getByRole("button", { name: "Change" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Remove" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// The log body the endpoint returns for this test. `undefined` ⇒ an empty log,
// which is the case where the copy button must not be offered at all.
let logBody: string | undefined = "line one\nline two";

vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: { GET: vi.fn(async () => ({ data: logBody })) } as unknown as typeof actual.api,
  };
});

import { RunLog } from "./RunLog";

const writeText = vi.fn(async () => {});

beforeEach(() => {
  logBody = "line one\nline two";
  writeText.mockClear();
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
});

afterEach(cleanup);

describe("RunLog — copy to clipboard (FX-3)", () => {
  it("copies the fetched log and flashes confirmation", async () => {
    render(<RunLog traceId="019f0000-1111-2222-3333-aaaaaaaaaaaa" />);

    const btn = await screen.findByRole("button", { name: "Copy log to clipboard" });
    fireEvent.click(btn);

    await waitFor(() => expect(writeText).toHaveBeenCalledWith("line one\nline two"));
    await waitFor(() => expect(screen.getByRole("button", { name: "Copy log to clipboard" }).textContent).toContain("Copied"));
  });

  it("offers nothing to copy when the run recorded no log", async () => {
    logBody = "";
    render(<RunLog traceId="019f0000-1111-2222-3333-bbbbbbbbbbbb" />);

    await screen.findByText("No log output recorded for this run.");
    expect(screen.queryByRole("button", { name: "Copy log to clipboard" })).toBeNull();
  });

  // A non-secure context withholds navigator.clipboard entirely. A button that
  // can only fail is worse than no button.
  it("hides the button when the browser withholds the clipboard API", async () => {
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
    render(<RunLog traceId="019f0000-1111-2222-3333-cccccccccccc" />);

    await screen.findByText(/line one/);
    expect(screen.queryByRole("button", { name: "Copy log to clipboard" })).toBeNull();
  });
});

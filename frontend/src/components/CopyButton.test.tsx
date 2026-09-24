// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CopyButton, CopyText } from "./ui";

// FX-13 — nine copy affordances in five styles, four of them bare `<span onClick>`
// with no role, no tab stop and (for the runner id) no feedback whatsoever. What
// these tests pin is the part that was actually broken: the affordance is a real
// button with a name, it tells you whether it worked, and it never claims a copy
// it did not make.

const writeText = vi.fn(async () => {});

beforeEach(() => {
  writeText.mockClear();
  writeText.mockImplementation(async () => {});
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
});
afterEach(cleanup);

describe("CopyButton (FX-13)", () => {
  it("is a real button with an accessible name, and copies", async () => {
    render(<CopyButton text="ssh-ed25519 AAAA" ariaLabel="Copy public key to clipboard" />);

    const btn = screen.getByRole("button", { name: "Copy public key to clipboard" });
    expect(btn.tagName).toBe("BUTTON");
    fireEvent.click(btn);

    await waitFor(() => expect(writeText).toHaveBeenCalledWith("ssh-ed25519 AAAA"));
    await waitFor(() => expect(btn.textContent).toContain("Copied"));
  });

  // Two of the old copies fired writeText without awaiting it, so they flashed
  // "Copied" even when the browser rejected the write.
  it("does not claim success when the write is rejected", async () => {
    writeText.mockImplementation(async () => {
      throw new Error("denied");
    });
    render(<CopyButton text="value" label="Copy" />);
    const btn = screen.getByRole("button");

    fireEvent.click(btn);

    await waitFor(() => expect(writeText).toHaveBeenCalled());
    expect(btn.textContent).toContain("Copy");
    expect(btn.textContent).not.toContain("Copied");
  });

  it("renders nothing when there is nothing to copy", () => {
    render(<CopyButton text="" />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  // A non-secure context withholds the clipboard entirely: an affordance that can
  // only fail is worse than none.
  it("renders nothing when the browser withholds the clipboard API", () => {
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
    render(<CopyButton text="value" />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("reports the copy to a caller that shows it their own way", async () => {
    const onCopied = vi.fn();
    render(<CopyButton text="value" onCopied={onCopied} />);
    fireEvent.click(screen.getByRole("button"));
    await waitFor(() => expect(onCopied).toHaveBeenCalledTimes(1));
  });

  // TraceId and the runner-id copy sit inside clickable rows; copying must not
  // also toggle the row open.
  it("can keep its click off the row behind it", async () => {
    const onRowClick = vi.fn();
    render(
      <div onClick={onRowClick}>
        <CopyButton text="value" stopPropagation />
      </div>,
    );
    fireEvent.click(screen.getByRole("button"));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    expect(onRowClick).not.toHaveBeenCalled();
  });
});

describe("CopyText (FX-13)", () => {
  it("shows the short form, copies the full value, and is keyboard reachable", async () => {
    render(<CopyText text="019f0000-1111-2222-3333-aaaaaaaaaaaa" display="019f…aaaaa" />);

    const btn = screen.getByRole("button", { name: /Copy 019f0000-1111-2222-3333-aaaaaaaaaaaa/ });
    expect(btn.tagName).toBe("BUTTON");
    expect(btn.textContent).toContain("019f…aaaaa");

    fireEvent.click(btn);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("019f0000-1111-2222-3333-aaaaaaaaaaaa"));
  });

  // The value still has to be readable when copying is impossible — it degrades
  // to plain text rather than to a button that does nothing.
  it("degrades to plain text with no clipboard API", () => {
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
    render(<CopyText text="019f-abc" />);
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("019f-abc")).toBeTruthy();
  });
});

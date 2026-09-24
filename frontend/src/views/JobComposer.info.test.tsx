// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { InfoToggle, InfoBody, Section } from "../components/ui";

afterEach(cleanup);

// EP-7 (the expanded-panels plan) — the Composer's section
// descriptions move behind the ⓘ. These tests pin the shared atom's contract
// and the rule that decides WHAT moves; the per-section placement is verified
// in the browser (a screenshot is the only thing that proves a wall of prose
// became a form).

describe("InfoToggle / InfoBody (EP-7)", () => {
  it("is closed by default and tracks aria-expanded", () => {
    const onToggle = vi.fn();
    const { rerender } = render(<InfoToggle open={false} onToggle={onToggle} />);
    const btn = screen.getByRole("button", { name: /about this section/i });
    expect(btn.getAttribute("aria-expanded")).toBe("false");
    expect(btn.getAttribute("title")).toMatch(/what is this/i);

    fireEvent.click(btn);
    expect(onToggle).toHaveBeenCalledTimes(1);

    rerender(<InfoToggle open onToggle={onToggle} />);
    expect(screen.getByRole("button", { name: /about this section/i }).getAttribute("aria-expanded")).toBe("true");
  });

  it("is a real <button>, not a clickable span — it is keyboard-reachable", () => {
    render(<InfoToggle open={false} onToggle={vi.fn()} />);
    const btn = screen.getByRole("button", { name: /about this section/i });
    expect(btn.tagName).toBe("BUTTON");
    expect((btn as HTMLButtonElement).type).toBe("button"); // never submits the form it sits in
  });

  it("InfoBody caps its measure so a long explanation stays readable", () => {
    const { container } = render(<InfoBody>explanation</InfoBody>);
    const body = container.firstElementChild as HTMLElement;
    expect(body.style.maxWidth).toBe("70ch");
  });
});

describe("Section still hides its info behind the toggle (unchanged by the extraction)", () => {
  it("renders the explanation only once the ⓘ is clicked", () => {
    render(
      <Section title="Notes" info="the education text">
        <div>body</div>
      </Section>,
    );
    expect(screen.queryByText("the education text")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /about this section/i }));
    expect(screen.getByText("the education text")).toBeTruthy();
  });
});

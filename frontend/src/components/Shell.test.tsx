// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { act, cleanup, fireEvent, render, renderHook, screen, within } from "@testing-library/react";
import { HelpMenu, nav, meta, SIDEBAR_WIDTH, SIDEBAR_WIDTH_COLLAPSED } from "./Shell";
import { SIDEBAR_COLLAPSED_KEY, useSidebarCollapsed } from "../hooks";

// Guards the sidebar information architecture (SC-P6). Before this, no test
// asserted the nav set/order, so an accidental drop or reorder of a top-level
// page would ship silently.
describe("Shell — sidebar IA guard", () => {
  it("renders the ratified top-down nav order (11 items, Scopes before Env Vars)", () => {
    expect(nav.map((n) => n.to)).toEqual([
      "/",
      "/jobs",
      "/workflows",
      "/scripts",
      "/schedules",
      "/scopes",
      "/env-vars",
      "/runners",
      "/runs",
      "/activity",
      "/settings",
    ]);
  });

  it("every nav entry has a meta label + subtitle", () => {
    for (const n of nav) {
      expect(meta[n.to]).toBeDefined();
      expect(meta[n.to].label).toBeTruthy();
    }
  });

  it("exposes Scopes as its own top-level page", () => {
    expect(meta["/scopes"].label).toBe("Scopes");
    expect(meta["/scopes"].subtitle).toBe("Hosts & execution environments");
  });

  it("drops 'scopes' from the Env Vars subtitle after the split", () => {
    expect(meta["/env-vars"].subtitle).toBe("Variables and secrets");
    expect(meta["/env-vars"].subtitle.toLowerCase()).not.toContain("scope");
  });
});

describe("Shell — collapsible sidebar", () => {
  beforeEach(() => localStorage.clear());

  it("starts expanded and toggles to collapsed", () => {
    const { result } = renderHook(() => useSidebarCollapsed());
    expect(result.current[0]).toBe(false);
    act(() => result.current[1]());
    expect(result.current[0]).toBe(true);
    act(() => result.current[1]());
    expect(result.current[0]).toBe(false);
  });

  it("persists the collapse so it survives a reload", () => {
    const first = renderHook(() => useSidebarCollapsed());
    act(() => first.result.current[1]());
    expect(localStorage.getItem(SIDEBAR_COLLAPSED_KEY)).toBe("1");

    // A fresh mount stands in for the next page load.
    const second = renderHook(() => useSidebarCollapsed());
    expect(second.result.current[0]).toBe(true);
  });

  it("reads a persisted expanded state as expanded", () => {
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, "0");
    const { result } = renderHook(() => useSidebarCollapsed());
    expect(result.current[0]).toBe(false);
  });

  it("collapses to an icons-only rail narrower than the expanded sidebar", () => {
    expect(SIDEBAR_WIDTH_COLLAPSED).toBeLessThan(SIDEBAR_WIDTH);
    // Wide enough for a centred 16px icon in its padded hit target.
    expect(SIDEBAR_WIDTH_COLLAPSED).toBeGreaterThanOrEqual(40);
  });
});

describe("Shell — Help menu (TR-5)", () => {
  afterEach(cleanup);

  it("opens a popover listing both manuals and both courses, each a new-tab link", () => {
    render(<HelpMenu btnStyle={{}} />);
    fireEvent.click(screen.getByRole("button", { name: /help/i }));
    const dialog = screen.getByRole("dialog", { name: "Documentation" });
    const links = within(dialog).getAllByRole("link");
    expect(links.map((l) => l.getAttribute("href"))).toEqual([
      "/user-manual.html",
      "/administrator-manual.html",
      "/training-operator.html",
      "/training-admin.html",
    ]);
    for (const l of links) {
      expect(l.getAttribute("target")).toBe("_blank");
      expect(l.getAttribute("rel")).toContain("noopener");
    }
  });

  it("closes on Escape and returns focus to the trigger", () => {
    render(<HelpMenu btnStyle={{}} />);
    const trigger = screen.getByRole("button", { name: /help/i });
    fireEvent.click(trigger);
    expect(screen.getByRole("dialog", { name: "Documentation" })).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Documentation" })).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });
});

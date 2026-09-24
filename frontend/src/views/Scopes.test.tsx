// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

// Stub the typed API client so the tab children's mount-time fetches resolve to
// empty data instead of hitting the network. We only assert each page's tab
// composition (rendered synchronously by TabBar), not the tab contents.
vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: { GET: vi.fn(async () => ({ data: [] })) } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ manageEnvVars: false, configureApp: false })),
  };
});

import { EnvVars } from "./EnvVars";
import { Scopes } from "./Scopes";

afterEach(cleanup);

const renderPage = (node: ReactNode) => render(<MemoryRouter>{node}</MemoryRouter>);
const hasTab = (name: string) => screen.queryAllByRole("button", { name }).length > 0;

// SC-P6: after the split, Env Vars is exactly Variables + Secrets and the new
// Scopes page hosts Scopes + Agencies.
describe("Env Vars / Scopes IA split", () => {
  it("Env Vars renders only Variables + Secrets (no Scopes/Agencies tabs)", () => {
    renderPage(<EnvVars />);
    expect(hasTab("Variables")).toBe(true);
    expect(hasTab("Secrets")).toBe(true);
    expect(hasTab("Scopes")).toBe(false);
    expect(hasTab("Agencies")).toBe(false);
  });

  it("Scopes page renders Scopes + Agencies", () => {
    renderPage(<Scopes />);
    expect(hasTab("Scopes")).toBe(true);
    expect(hasTab("Agencies")).toBe(true);
  });
});

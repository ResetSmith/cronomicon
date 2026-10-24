// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";

// 2.5.3 question 215: working in an agency, Settings → Repositories is that
// agency's. The repository list is narrowed to the agency in use, so another
// agency's repository read as missing, and a global administrator was offered
// a Connect card for every other agency.

vi.mock("../../agency", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../agency")>();
  return {
    ...actual,
    useAgencyOptional: () => ({ agency: "ag-fin", agencyName: "Finance", onOverview: false, choices: [] }),
  };
});
vi.mock("../../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string) => {
        if (path === "/git-repos") return { data: { items: [] } };
        if (path === "/agencies")
          return { data: [{ id: "global", name: "Global" }, { id: "ag-fin", name: "Finance" }, { id: "ag-tax", name: "Tax" }] };
        if (path === "/settings/general") return { data: { gitAllowedHosts: [] } };
        return { data: [] };
      }),
    } as unknown as typeof actual.api,
  };
});

import { RepositoriesSection } from "./Repositories";

afterEach(cleanup);

describe("Settings → Repositories, working in an agency", () => {
  it("offers a global administrator the agency in use's Connect alone", async () => {
    render(<RepositoriesSection isGlobal />);
    await waitFor(() => expect(screen.getByText("Finance's repository")).toBeTruthy());
    expect(screen.queryByText("Tax's repository")).toBeNull();
    expect(screen.queryByText("Global's repository")).toBeNull();
    expect(screen.getAllByText("Connect")).toHaveLength(1);
  });
});

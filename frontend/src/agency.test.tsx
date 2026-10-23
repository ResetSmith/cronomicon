// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

// The agency switcher's shell (2.4.4 F1): the provider, the switcher, the
// fallback for an agency the person may not choose.

let CONTEXT: Record<string, unknown> = {};
let BADGES: unknown[] = [];
const gets: string[] = [];
const puts: { path: string; body: unknown }[] = [];
const { setRequestAgency } = vi.hoisted(() => ({ setRequestAgency: vi.fn() }));

vi.mock("./api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api/client")>();
  return {
    ...actual,
    setRequestAgency,
    api: {
      GET: vi.fn(async (path: string) => {
        gets.push(path);
        if (path === "/me/context") return { data: CONTEXT };
        if (path === "/me/agency-badges") return { data: BADGES };
        return { data: [] };
      }),
      PUT: vi.fn(async (path: string, opts: { body: unknown }) => {
        puts.push({ path, body: opts.body });
        return { data: CONTEXT };
      }),
    } as unknown as typeof actual.api,
  };
});

import { AgencyProvider, useAgency, useEnsureAgency } from "./agency";
import { AgencySwitcher } from "./components/AgencySwitcher";
import { CreationAgencyPicker, useCreationAgencies } from "./components/CreationAgencyPicker";
import { AGENCY_REFRESH_EVENT } from "./api/client";

const agencies = [
  { id: "global", name: "Global", kind: "agency" },
  { id: "ag-a", name: "agency-alpha", kind: "agency" },
  { id: "ag-b", name: "agency-beta", kind: "agency" },
];

function Probe() {
  const ag = useAgency();
  const loc = useLocation();
  return (
    <div>
      <span data-testid="agency">{ag.agency}</span>
      <span data-testid="search">{loc.search}</span>
      <span data-testid="path">{loc.pathname}</span>
    </div>
  );
}

function renderAt(url: string) {
  return render(
    <MemoryRouter initialEntries={[url]}>
      <AgencyProvider>
        <AgencySwitcher collapsed={false} />
        <Probe />
      </AgencyProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  CONTEXT = { choices: agencies, storedDefault: null, effectiveDefault: "ag-a", workingAgency: "ag-a", canOverview: false };
  BADGES = [];
  gets.length = 0;
  puts.length = 0;
  setRequestAgency.mockClear();
});
afterEach(cleanup);

describe("the agency context (2.4.4 F1)", () => {
  it("renders nothing, and so fetches nothing, before the agency is known", async () => {
    let release: () => void = () => {};
    const { api } = await import("./api/client");
    (api.GET as ReturnType<typeof vi.fn>).mockImplementationOnce(
      (path: string) =>
        new Promise((resolve) => {
          gets.push(path);
          release = () => resolve({ data: CONTEXT });
        }),
    );
    renderAt("/jobs");
    expect(screen.queryByTestId("agency")).toBeNull();
    expect(gets).toEqual(["/me/context"]);
    await act(async () => release());
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    // The client names the agency before any child's effect could fetch.
    expect(setRequestAgency).toHaveBeenCalledWith("ag-a");
  });

  it("starts where /me/context says, and puts it in the address", async () => {
    CONTEXT = { ...CONTEXT, effectiveDefault: "ag-b", workingAgency: "ag-b" };
    renderAt("/jobs");
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-b"));
    await waitFor(() => expect(screen.getByTestId("search").textContent).toBe("?agency=ag-b"));
  });

  it("takes the agency a link carries", async () => {
    renderAt("/runs?trace=t1&agency=ag-b");
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-b"));
    expect(screen.getByTestId("search").textContent).toContain("trace=t1");
  });

  it("falls back to the default, saying no more than that, for an agency not among the choices", async () => {
    renderAt("/jobs?agency=ag-gone");
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    await waitFor(() => expect(screen.getByTestId("search").textContent).toBe("?agency=ag-a"));
    expect(screen.getByRole("status").textContent).toBe("That agency is not available to you; showing agency-alpha");
  });

  it("lands an every-agency viewer on the Dashboard with All agencies chosen, with Global for the lists (2.5.2 OP-25)", async () => {
    CONTEXT = { ...CONTEXT, choices: [{ id: "overview", name: "All agencies", kind: "overview" }, ...agencies], effectiveDefault: "overview", workingAgency: "global", canOverview: true };
    renderAt("/");
    await waitFor(() => expect(screen.getByTestId("search").textContent).toBe("?agency=overview"));
    expect(screen.getByTestId("path").textContent).toBe("/");
    expect(screen.getByTestId("agency").textContent).toBe("global");
    expect(screen.getByTestId("agency-switcher").textContent).toContain("All agencies");
  });

  it("keeps the working agency on every other page while All agencies is chosen (2.5.2 OP-24)", async () => {
    CONTEXT = { ...CONTEXT, choices: [{ id: "overview", name: "All agencies", kind: "overview" }, ...agencies], effectiveDefault: "overview", workingAgency: "global", canOverview: true };
    renderAt("/jobs?agency=overview");
    await waitFor(() => expect(screen.getByTestId("search").textContent).toBe("?agency=global"));
    expect(screen.getByTestId("agency").textContent).toBe("global");
    expect(screen.getByTestId("agency-switcher").textContent).toContain("Global");
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("switches to All agencies on the Dashboard, and back to an agency from it (2.5.2 OP-21)", async () => {
    CONTEXT = { ...CONTEXT, choices: [{ id: "overview", name: "All agencies", kind: "overview" }, ...agencies], effectiveDefault: "ag-a", workingAgency: "ag-a", canOverview: true };
    renderAt("/jobs?agency=ag-a");
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    fireEvent.click(screen.getByTestId("agency-switcher"));
    fireEvent.click(await screen.findByRole("option", { name: /All agencies/ }));
    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/"));
    expect(screen.getByTestId("search").textContent).toBe("?agency=overview");
    expect(screen.getByTestId("agency").textContent).toBe("ag-a");
    fireEvent.click(screen.getByTestId("agency-switcher"));
    fireEvent.click(await screen.findByRole("option", { name: /agency-beta/ }));
    await waitFor(() => expect(screen.getByTestId("search").textContent).toBe("?agency=ag-b"));
    expect(screen.getByTestId("path").textContent).toBe("/");
    expect(screen.getByTestId("agency").textContent).toBe("ag-b");
  });

  it("names the built-in agency by its name now, to people who do not read its rows (2.5.2 GR)", async () => {
    // FIN's admin: Global is not among their choices, and its name still shows.
    CONTEXT = { ...CONTEXT, choices: agencies.slice(1), globalName: "Central" };
    function Named() {
      const ag = useAgency();
      return <span data-testid="global-name">{ag.nameOf("global")}|{ag.globalName}</span>;
    }
    render(
      <MemoryRouter initialEntries={["/jobs"]}>
        <AgencyProvider>
          <Named />
        </AgencyProvider>
      </MemoryRouter>,
    );
    expect((await screen.findByTestId("global-name")).textContent).toBe("Central|Central");
  });

  it("reads the choices again when a request is refused", async () => {
    renderAt("/jobs");
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    const before = gets.filter((g) => g === "/me/context").length;
    await act(async () => {
      window.dispatchEvent(new Event(AGENCY_REFRESH_EVENT));
    });
    await waitFor(() => expect(gets.filter((g) => g === "/me/context").length).toBe(before + 1));
  });
});

describe("the views (2.4.4 F2)", () => {
  function Linked({ owner }: { owner: string }) {
    useEnsureAgency(owner);
    return null;
  }

  it("switches to the agency of an object a link opens, and says so", async () => {
    render(
      <MemoryRouter initialEntries={["/compose?id=7"]}>
        <AgencyProvider>
          <Linked owner="ag-b" />
          <Probe />
        </AgencyProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-b"));
    expect(screen.getByTestId("search").textContent).toContain("id=7");
    expect(screen.getByRole("status").textContent).toBe("Switched to agency-beta");
  });

  it("does not switch to an owner the person may not choose", async () => {
    render(
      <MemoryRouter initialEntries={["/compose?id=7"]}>
        <AgencyProvider>
          <Linked owner="ag-elsewhere" />
          <Probe />
        </AgencyProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("creates in the agency in use: the form shows it, fixed, and sends it", async () => {
    let body: unknown = null;
    function Form() {
      const pick = useCreationAgencies(false, "manageEnvVars");
      body = pick.body;
      return <CreationAgencyPicker label="variable" pick={pick} />;
    }
    render(
      <MemoryRouter initialEntries={["/env-vars?agency=ag-b"]}>
        <AgencyProvider>
          <Form />
        </AgencyProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId("creation-agency").textContent).toContain("Created in agency-beta"));
    expect(screen.queryByRole("combobox", { name: "Agency" })).toBeNull();
    expect(body).toEqual({ agencyIds: ["ag-b"] });
  });
});

describe("the switcher (2.4.4 F1)", () => {
  it("is a label for a person with one choice", async () => {
    CONTEXT = { ...CONTEXT, choices: [agencies[0]], effectiveDefault: "global", workingAgency: "global" };
    renderAt("/jobs");
    await waitFor(() => expect(screen.getByTestId("agency-label").textContent).toBe("G"[0] + "lobal"));
    expect(screen.queryByTestId("agency-switcher")).toBeNull();
  });

  it("switches, and shows each agency's waiting work and the default", async () => {
    BADGES = [
      { agencyId: "ag-b", openNotices: 2, failedRuns24h: 3 },
      { agencyId: "ag-a", openNotices: 0, failedRuns24h: 0 },
    ];
    renderAt("/jobs");
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    fireEvent.click(screen.getByTestId("agency-switcher"));
    const beta = await screen.findByRole("option", { name: /agency-beta/ });
    expect(beta.textContent).toContain("3");
    expect(beta.textContent).toContain("2");
    expect(screen.getByRole("option", { name: /agency-alpha/ }).textContent).toContain("Default");
    fireEvent.click(beta);
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-b"));
    expect(screen.getByTestId("search").textContent).toBe("?agency=ag-b");
  });

  // 2.5.0 C11, defect 85: on a page that shows one object of the agency in use
  // (a run's detail, a job in the composer), switching kept the object in the
  // address, the page switched straight back to the object's agency, and the
  // person could not leave it but by another page.
  function RunDetail() {
    const loc = useLocation();
    const trace = new URLSearchParams(loc.search).get("trace");
    useEnsureAgency(trace ? "ag-a" : undefined); // the run is agency-alpha's
    return null;
  }

  it("switches away from an object's page and stays, keeping how the page is shown", async () => {
    render(
      <MemoryRouter initialEntries={["/runs?trace=t1&tab=executions&agency=ag-a"]}>
        <AgencyProvider>
          <AgencySwitcher collapsed={false} />
          <RunDetail />
          <Probe />
        </AgencyProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    fireEvent.click(screen.getByTestId("agency-switcher"));
    fireEvent.click(await screen.findByRole("option", { name: /agency-beta/ }));
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-b"));
    await act(async () => new Promise((r) => setTimeout(r, 50)));
    expect(screen.getByTestId("agency").textContent).toBe("ag-b");
    const search = screen.getByTestId("search").textContent ?? "";
    expect(search).not.toContain("trace=");
    expect(search).toContain("tab=executions");
    expect(screen.getByTestId("path").textContent).toBe("/runs");
  });

  it("shows an editor's list when the object it had open goes with the switch", async () => {
    renderAt("/compose?id=7&agency=ag-a");
    await waitFor(() => expect(screen.getByTestId("agency").textContent).toBe("ag-a"));
    fireEvent.click(screen.getByTestId("agency-switcher"));
    fireEvent.click(await screen.findByRole("option", { name: /agency-beta/ }));
    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/jobs"));
    expect(screen.getByTestId("search").textContent).toBe("?agency=ag-b");
  });
});

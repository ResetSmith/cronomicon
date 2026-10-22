// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, waitFor } from "@testing-library/react";

// 2.5.2 OP2.1 — a global administrator's visit offers the page's address as the
// one alerts link to; nobody else's does, and it is offered once per load.

const { POST, caps } = vi.hoisted(() => ({ POST: vi.fn(async () => ({ data: {} })), caps: { configureAppGlobal: true } }));

vi.mock("./api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api/client")>();
  return {
    ...actual,
    api: { POST } as unknown as typeof actual.api,
    fetchCapabilities: vi.fn(async () => ({ ...caps })),
  };
});

import { appAddress, resetAlertLinkOffer, useOfferAlertLinkAddress } from "./alertLink";

function Probe() {
  useOfferAlertLinkAddress();
  return null;
}

beforeEach(() => {
  POST.mockClear();
  resetAlertLinkOffer();
  caps.configureAppGlobal = true;
});
afterEach(cleanup);

describe("offering the alert link address (2.5.2 OP2.1)", () => {
  it("offers the page's own address for a global administrator, once", async () => {
    render(<Probe />);
    await waitFor(() => expect(POST).toHaveBeenCalledTimes(1));
    const [path, opts] = POST.mock.calls[0] as unknown as [string, { body: { address: string } }];
    expect(path).toBe("/settings/alert-link-address/detect");
    expect(opts.body.address).toBe(window.location.origin);
    expect(appAddress()).toBe(window.location.origin);
    // Another mount in the same load does not offer again.
    cleanup();
    render(<Probe />);
    await new Promise((r) => setTimeout(r, 0));
    expect(POST).toHaveBeenCalledTimes(1);
  });

  it("offers nothing for anyone else", async () => {
    caps.configureAppGlobal = false;
    render(<Probe />);
    await new Promise((r) => setTimeout(r, 0));
    expect(POST).not.toHaveBeenCalled();
  });
});

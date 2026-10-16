// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { MiddlewareCallbackParams } from "openapi-fetch";
import { AGENCY_HEADER, AGENCY_REFRESH_EVENT, agencyMiddleware, allReadable, setRequestAgency } from "./client";

// The API client names the agency the person works in on every request
// (2.4.4 F1), and asks for the choices again when a request is refused. The
// middleware is driven directly: the client's relative base URL needs a
// browser's location to resolve.

afterEach(() => setRequestAgency(""));

const params = (request: Request) => ({ request }) as unknown as MiddlewareCallbackParams & { request: Request };

async function send(method: string): Promise<Request> {
  const out = await agencyMiddleware.onRequest!(params(new Request("http://localhost/api/v1/jobs", { method })));
  return out as Request;
}

describe("the agency header", () => {
  it("is on every request once an agency is set, reads and writes alike", async () => {
    setRequestAgency("ag-a");
    expect((await send("GET")).headers.get(AGENCY_HEADER)).toBe("ag-a");
    expect((await send("PUT")).headers.get(AGENCY_HEADER)).toBe("ag-a");
  });

  it("is left off a request that asks for every agency it may read (F4)", async () => {
    setRequestAgency("ag-a");
    const out = (await agencyMiddleware.onRequest!(
      params(new Request("http://localhost/api/v1/runs", { headers: allReadable })),
    )) as Request;
    expect(out.headers.get(AGENCY_HEADER)).toBeNull();
  });

  it("is absent when no agency is set", async () => {
    expect((await send("GET")).headers.get(AGENCY_HEADER)).toBeNull();
  });

  it("raises the refresh on a 403, and only then", async () => {
    const heard = vi.fn();
    window.addEventListener(AGENCY_REFRESH_EVENT, heard);
    const respond = (status: number) =>
      agencyMiddleware.onResponse!({
        ...params(new Request("http://localhost/api/v1/jobs")),
        response: new Response("{}", { status }),
      } as unknown as Parameters<NonNullable<typeof agencyMiddleware.onResponse>>[0]);
    await respond(200);
    await respond(403);
    window.removeEventListener(AGENCY_REFRESH_EVENT, heard);
    expect(heard).toHaveBeenCalledTimes(1);
  });
});

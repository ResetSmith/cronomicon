import createClient, { type Middleware } from "openapi-fetch";
import type { components, paths } from "./schema";

// Reads the non-httpOnly CSRF cookie the backend sets (T8 double-submit).
function csrfToken(): string | null {
  const m = document.cookie.match(/(?:^|;\s*)amadeus_csrf=([^;]+)/);
  return m ? decodeURIComponent(m[1]) : null;
}

// Echo the CSRF token on state-changing requests (T8). Safe methods skip it.
const csrf: Middleware = {
  async onRequest({ request }) {
    if (!["GET", "HEAD", "OPTIONS"].includes(request.method)) {
      const t = csrfToken();
      if (t) request.headers.set("X-CSRF-Token", t);
    }
    return request;
  },
};

// Intercept 401 Unauthorized responses to handle session expiration.
// Ignores the initial /me fetch to avoid infinite redirect loops at startup (FR-H3).
const authInterceptor: Middleware = {
  async onResponse({ request, response }) {
    if (response.status === 401) {
      let isMe = false;
      try {
        isMe = new URL(request.url).pathname.endsWith("/me");
      } catch {
        isMe = request.url.endsWith("/me") || request.url.includes("/me");
      }
      if (!isMe) {
        window.location.href = "/";
      }
    }
    return response;
  },
};

// Typed client generated from openapi.yaml. baseUrl is the /api/v1 server; the
// session cookie rides along via credentials: "include" (T8).
export const api = createClient<paths>({ baseUrl: "/api/v1", credentials: "include" });
api.use(csrf);
api.use(authInterceptor);

// Placeholder header object for the generated client's typed `header` param on
// mutating routes. The real X-CSRF-Token is attached by the `csrf` middleware
// above (double-submit), so this only satisfies the type — one shared copy
// instead of a per-view duplicate (CC.21).
export const csrfHeader = { "X-CSRF-Token": "" };

// errMsg extracts a human-readable message from an unknown API error (the
// openapi-fetch error union or a thrown Error), for toast/inline display. Falls
// back to a generic string when no message is present (CC.21).
export function errMsg(error: unknown): string {
  if (error && typeof error === "object" && "message" in error) {
    const m = (error as { message?: unknown }).message;
    if (typeof m === "string" && m) return m;
  }
  return "request failed";
}

// Redirect the browser into the identity provider's OIDC flow.
export function login(): void {
  window.location.href = "/api/v1/auth/login";
}

// Redirect into the local dev-login bypass (only works when AMADEUS_DEV_AUTH=true).
export function devLogin(): void {
  window.location.href = "/api/v1/auth/dev-login";
}

// The active auth mode + which login methods the backend offers. Used by the
// Login screen and Shell to render the right experience before any session
// exists. In trusted-header mode the in-app OIDC button is dead (the reverse
// proxy authenticates), so the Login screen suppresses it. Falls back to
// OIDC-only on error.
export interface Providers {
  mode: "trusted-header" | "oidc";
  oidc: boolean;
  dev: boolean;
  logoutUrl: string;
}
const providersFallback: Providers = { mode: "oidc", oidc: true, dev: false, logoutUrl: "" };

export async function fetchProviders(): Promise<Providers> {
  try {
    const res = await fetch("/api/v1/auth/providers", { credentials: "include" });
    if (!res.ok) return providersFallback;
    return { ...providersFallback, ...(await res.json()) } as Providers;
  } catch {
    return providersFallback;
  }
}

// Server feature flags (C.3) + the caller's effective authz permissions (PP-B1)
// so the SPA can gate optional integrations AND admin surfaces to match what the
// backend will allow.
//
// The shape is GENERATED from the OpenAPI schema as of RF-16
// (the RBAC-fixes plan). It used to be hand-maintained here while
// GET /capabilities was undocumented in the spec, so nothing caught a rename or
// a field the server stopped emitting — and the endpoint had been widened twice
// (RB-3, RB-29) in the meantime. The contract is now the single source.
//
// ⚠️ The permission flags are FLAT UNION semantics — "may this actor do this
// SOMEWHERE" (RB-3). Coarse nav gating only, never a substitute for a per-row
// check: an operator scoped to Finance reports triggerJobs=true and still gets a
// 403 on a Tax job. Per-row truth is canRun/canKill on list rows (RB-24).
// `unrestricted` is scope REACH, not a permission (RB-29).
export type Capabilities = components["schemas"]["Capabilities"];

const CAPS_OFF: Capabilities = {
  vault: false,
  apprise: false,
  compose: false,
  manageRoles: false,
  configureApp: false,
  manageEnvVars: false,
  publishSchedule: false,
  triggerJobs: false,
  killJobs: false,
  // AF-2 — may this caller author an All-scoped definition. Off in the failure
  // fallback like everything else here: withholding the option is the safe way
  // to be wrong, since the server refuses it anyway.
  composeUnbound: false,
  unrestricted: false,
};

export async function fetchCapabilities(): Promise<Capabilities> {
  try {
    const res = await fetch("/api/v1/capabilities", { credentials: "include" });
    if (!res.ok) return { ...CAPS_OFF };
    return { ...CAPS_OFF, ...(await res.json()) } as Capabilities;
  } catch {
    return { ...CAPS_OFF };
  }
}

// Build metadata stamped into the binary (ldflags), surfaced at the unauthenticated
// GET /version. Used by the sidebar footer so the displayed version tracks the
// build (the git tag → ldflags → /version) instead of a hardcoded string.
export interface BuildInfo {
  version: string;
  commit: string;
  date: string;
}

export async function fetchVersion(): Promise<BuildInfo | null> {
  try {
    const res = await fetch("/version", { credentials: "include" });
    if (!res.ok) return null;
    return (await res.json()) as BuildInfo;
  } catch {
    return null;
  }
}

// Sign out: clear the app session/CSRF cookies, then navigate to the backend's
// logout target if it returned one (the identity provider's logout endpoint in trusted-header
// mode). In oidc/dev mode there's no redirect, so reload to the login screen.
export async function logout(): Promise<void> {
  try {
    const res = await fetch("/api/v1/auth/logout", {
      method: "POST",
      credentials: "include",
      headers: { "X-CSRF-Token": csrfToken() ?? "" },
    });
    if (res.ok) {
      const body = (await res.json().catch(() => null)) as { redirect?: string } | null;
      if (body?.redirect) {
        window.location.href = body.redirect;
        return;
      }
    }
  } catch {
    // fall through to reload
  }
  window.location.href = "/";
}

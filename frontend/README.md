# Cronomicon frontend

Operator UI — **React + TypeScript + Vite** (T2). Builds into `../backend/web/dist`,
which the Go binary embeds and serves (T3). The API client is **generated from
the canonical `backend/openapi.yaml`** (T5) — never hand-write request/response types.

> Typed client + auth shell (OIDC session + CSRF) + theme, with sixteen view
> components under `src/views/` — the routed list and each view's purpose are
> the views table in `../AGENTS.md`, which is the one list.

## Develop

```bash
npm install
npm run gen     # regenerate src/api/schema.d.ts from ../backend/openapi.yaml
npm run dev     # Vite dev server on :5173, proxies /api → :8080
npm run build   # tsc -b && vite build → ../backend/web/dist
```

Run the backend (`cd ../backend && make run`) alongside `npm run dev`; the dev
server proxies API + auth routes to it.

## Architecture

- `src/api/schema.d.ts` — generated types (do not edit).
- `src/api/client.ts` — `openapi-fetch` typed client; sends the session cookie
  (`credentials: include`) and the CSRF token header on writes (T8).
- `src/auth.tsx` — fetches `/me`; unauthenticated → Login → `/api/v1/auth/login`.
- `src/components/Shell.tsx` — sidebar nav + routed outlet.
- `src/views/*` — per-view components calling the typed client.
- `src/theme.ts` — design tokens ported from the prototype.

## Adding a view

1. `src/views/Foo.tsx` — call `api.GET("/foo")`, render with `useGet`/`rows`.
2. Add a `<Route>` in `src/App.tsx` and a nav entry in `Shell.tsx`.
3. Fold in the relevant V1.1 polish from the backlog as you build.

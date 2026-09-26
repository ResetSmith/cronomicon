# Cronomicon frontend

Operator UI — **React + TypeScript + Vite**. Builds into `../backend/web/dist`,
which the Go binary embeds and serves. The API client is **generated from
the canonical `backend/openapi.yaml`** — never hand-write request/response types.

> Typed client + auth shell (OIDC session + CSRF) + theme, with sixteen view
> components under `src/views/`. The routes are declared in `src/App.tsx`, and
> the user manual describes each screen.

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
  (`credentials: include`) and the CSRF token header on writes.
- `src/auth.tsx` — fetches `/me`; unauthenticated → Login → `/api/v1/auth/login`.
- `src/components/Shell.tsx` — sidebar nav + routed outlet.
- `src/views/*` — per-view components calling the typed client.
- `src/theme.ts` — design tokens (palette, type scale, radii, faces).

## Adding a view

1. `src/views/Foo.tsx` — call `api.GET("/foo")`, render with `useGet`/`rows`.
2. Add a `<Route>` in `src/App.tsx` and a nav entry in `Shell.tsx`.
3. Read theme tokens (`c.*`) during render and build from the shared atoms in
   `src/components/ui.tsx` rather than hand-rolling copies.

## Design system

All tokens live in [`src/theme.ts`](src/theme.ts) — components read the mutable `c.*` object at render time (never freeze tokens in module-level consts, or the Light/Dark toggle silently breaks).

### Colors

- **Dark theme**: `#0a1119` (bg), `#e8eff7` (text), `#4da3d9` (primary), `#c9a227` (accent — **gold is the brand**)
- **Light theme**: `#f4f6fa` (bg), white panels, with darker counterpart tokens
- **Status**: `#45b26b` (success), `#e05a5a` (danger), `#d98a2b` (**warning is orange**; gold is reserved for the brand)
- **Contrast is a gate**: every text/background and control-boundary pair is held to WCAG AA (4.5:1 body, 3:1 controls); `c.onSolid` exists because white-on-solid fails in dark mode

### Typography

- **Faces**: IBM Plex Sans (body/titles), Plex Sans Condensed (uppercase structural type), Plex Mono with `tabular-nums` (cron, trace IDs, timestamps) — **self-hosted** under `public/fonts/`, never a font CDN
- **Six-token scale**: `fontDisplay` 36 · `fontTitle` 24 · `fontHead` 16 · `fontBody` 14 · `fontSm` 13 · `fontXs` 11 — no literals, and 10px is deliberately gone

### Shape & spacing

- **Three radii, only three**: `radiusChip` 4 (chips, buttons, inputs) · `radiusSurface` 8 (cards, panels, modals) · `radiusPill` 999 (**status pills only**); a test fails on any numeric `borderRadius` literal
- **A surface gets a border OR a shadow, never both** (floating overlays excepted)
- **Grid**: 8px base unit

## State management

The production frontend uses **no global store or external state library** (no Redux/Zustand/React Query). State is split into two layers:

**App-wide concerns → three React contexts** (provided in `main.tsx` / `App.tsx`):

| Context | Provider · hook | Holds |
|---|---|---|
| `auth.tsx` | `AuthProvider` · `useAuth()` | Current user (`Me`), loading state. Capability gating reads the server's `/capabilities` flags plus per-row `canRun`/`canKill`; a hardcoded client-side role list would be wrong the moment custom roles exist |
| `theme-context.tsx` | `ThemeProvider` | Light/dark mode |
| `timezone-context.tsx` | `TimezoneProvider` | Application timezone for rendering timestamps |

**Server data → fetched per-view via shared hooks** (`hooks.ts`), against the typed `openapi-fetch` client (`api/client.ts`, generated from `openapi.yaml`, CSRF double-submit + session cookie):

```typescript
// A view fetches its own data with useGet; useLiveGet polls only while work is in flight.
const jobsQ = useGet<Job[]>(
  () => api.GET("/jobs", { params: { query: { page: 1, pageSize: 200 } } }),
  [bump],                 // refetch when a mutation bumps this dep
);
```

- `useGet<T>(fetcher, deps, intervalMs?)` — typed fetch with `{ data, error, loading }`; keeps the last-good data on a background-poll error rather than blanking the view.
- `useLiveGet<T>(...)` — wraps `useGet` to poll (`activeMs`) **only while `isActive(data)`** is true (e.g. a run is still queued/running) and stops once everything is terminal.
- Other shared hooks: `useTableColumns` (order/visibility behind the Columns menu, `components/table.tsx`), `useColumnWidths` (localStorage-persisted widths), `useTableSort` (paired with the backend's `sortparam` package), `useClientPager`, `useDebounced`, `useInlineTags`, `useInlineAnnotation`, `useToast`.

Local UI state (active tab, search, expanded rows, pagination) is plain `useState` inside each view — no prop drilling, no shared mutable store.

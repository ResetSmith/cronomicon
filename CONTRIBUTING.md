# Contributing to Cronomicon

Thanks for your interest in improving Cronomicon. This document covers how to
report problems, propose changes, and get a pull request merged.

## Reporting bugs and requesting features

Open a GitHub issue. For bugs, include the Cronomicon version (`GET /version`
or the Settings page footer), the run type involved, and the steps to
reproduce. For security vulnerabilities, **do not open a public issue** —
use GitHub's private vulnerability reporting on this repository instead.

## Development setup

The repository has two production surfaces, described in `AGENTS.md`:

- `backend/` — Go 1.26, SQLite, single static binary
- `frontend/` — React + TypeScript + Vite, embedded into the binary

```bash
# Backend
cd backend
make verify          # tidy + vet + go test -race + build

# Frontend
cd frontend
npm ci
npm run test         # vitest
npm run build        # tsc -b && vite build → backend/web/dist
```

A local development instance with dev auth and demo data:

```bash
CRONOMICON_DEV_AUTH=true CRONOMICON_DEV_SEED=true CRONOMICON_COOKIE_SECURE=false \
  CRONOMICON_DB_PATH=/tmp/cronomicon-dev.db ./backend/bin/cronomicon
```

## Making changes

1. Fork the repository and create a branch from `develop`. Open the pull
   request against `develop` as well: GitHub pre-fills `release`, which is the
   production branch and only receives merges from `develop`.
2. Keep each pull request focused on one change.
3. Follow the conventions in `AGENTS.md`. In particular:
   - The API contract is `openapi.yaml`; `backend/openapi.yaml` must stay
     byte-identical, and the frontend client is regenerated with
     `npm run gen`.
   - Database changes are migrations in `backend/internal/db/migrations/`,
     numbered +10 from the latest, with both `up` and `down` files.
   - Frontend styling uses theme tokens from `frontend/src/theme.ts`; the
     test suite rejects literal radii, font sizes, and a `display` face.
   - After editing anything under `documentation/`, run `npm run build` so
     the embedded copies in `backend/web/dist` stay in sync.
4. Add or update tests. `make test` and `npm run test` must pass.
5. Add an entry to `CHANGELOG.md` under the next version.

## Pull requests

- Describe what the change does and why. Link the issue it addresses.
- CI runs the same gates as `make verify` and `npm run build`; a PR must be
  green before review.
- Reviews may ask for changes. Push additional commits rather than
  force-pushing while a review is in progress.

## Licensing of contributions

Cronomicon is licensed under the Apache License, Version 2.0 (see `LICENSE`).
By submitting a contribution you agree that it is licensed under the same
terms, as described in Section 5 of the License. No separate contributor
license agreement is required.

If you contribute a file you did not write, or one that carries its own
license, say so in the pull request so the `NOTICE` file can be updated.

## Code of conduct

Be respectful and constructive. Maintainers may close issues or pull
requests that are abusive or off-topic.

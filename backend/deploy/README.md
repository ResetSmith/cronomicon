# backend/deploy — operator reference shipped with the application

What lives here is what the application itself builds against, serves or
documents. The deployment stack (Docker Compose, reverse proxy, SSO provider,
Vault Agent sidecar, runner container images, load tests, the deployment guide
and the day-2 runbooks) lives in the separate `cron-ops` repository.

| File | Purpose | Consumed by |
|---|---|---|
| `cronomicon.env.example` | Annotated server runtime env template | operators; copied by cron-ops |
| `env-matrix.md` | Every `CRONOMICON_*` variable, default and requirement | `internal/config/envdoc_test.go` (both directions) |
| `runner-install.sh` | Runner agent installer, served in-app at `/runner-install.sh` | `frontend/vite-manuals-plugin.js`, Runners view |
| `runner-install-check.sh` | Post-install self-check for a runner host | runner install guide |
| `cronomicon-runner.env.example` | Runner agent env template, served in-app | `vite-manuals-plugin.js`, `runner-provision.test.ts` |
| `cronomicon-runner.service` | Hardened systemd unit the installer writes | `internal/agent/sandbox.go`, runner guides |
| `backup-restore.md` | The app's own `backup` / `restore` commands | `cmd/cronomicon/restore.go` |
| `security-review.md` | Security controls and where each is enforced | `internal/api/agents.go`, `install.go`, OpenAPI descriptions |
| `ci-validate-template.yml` | CI snippet for a job-definitions repo (`cronomicon validate`) | users' GitOps repos |

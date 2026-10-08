# backend/deploy — operator reference shipped with the application

What lives here is what the application itself builds against, serves or
documents. The deployment stack (Docker Compose, reverse proxy, SSO provider,
Vault Agent sidecar, load tests, the deployment guide and the day-2 runbooks) is
the operator's own concern and is not shipped here; the administrator manual's
Deployment chapter describes a reference topology. The container images are
built from `backend/Dockerfile`, `backend/Dockerfile.runner` and
`backend/Dockerfile.runner.fat`, and published to `ghcr.io/resetsmith/` by
`.github/workflows/publish-images.yml` on every release tag.

| File | Purpose | Consumed by |
|---|---|---|
| `cronomicon.env.example` | Annotated server runtime env template | operators; copy into your deployment |
| `env-matrix.md` | Every `CRONOMICON_*` variable, default and requirement | `internal/config/envdoc_test.go` (both directions) |
| `runner-install.sh` | Runner agent installer, served in-app at `/runner-install.sh`. `--instance <name>` installs a further agent beside a machine's first | `frontend/vite-manuals-plugin.js`, Runners view |
| `runner-install-check.sh` | The installer's own checks: syntax, flag parsing, and the layout and unit it derives for the default agent and for an instance. Needs no root and changes nothing | `make verify` (and `make installer-check` alone) |
| `runner-isolation-check.sh` | Served in-app at `/runner-isolation-check.sh`. Run as root on a runner host AFTER installing: checks each agent's account, directories and unit, and that no agent's user can read or write another's (state, config, `/dev/shm`). Creates only probe directories it removes | `vite-manuals-plugin.js`, runner install guide §3 |
| `cronomicon-runner.env.example` | Runner agent env template, served in-app | `vite-manuals-plugin.js`, `runner-provision.test.ts` |
| `cronomicon-runner.service` | Hardened systemd unit the installer writes | `internal/agent/sandbox.go`, runner guides |
| `backup-restore.md` | The app's own `backup` / `restore` commands | `cmd/cronomicon/restore.go` |
| `security-review.md` | Security controls and where each is enforced | `internal/api/agents.go`, `install.go`, OpenAPI descriptions |
| `ci-validate-template.yml` | CI snippet for a job-definitions repo (`cronomicon validate`) | users' GitOps repos |

## One machine, several agents

An agent serves exactly one agency. A machine that must serve two runs two
agents, and `runner-install.sh --instance <name>` installs each one after the
first. Every name an install owns comes from one value:

| | default agent | `--instance tax` |
|---|---|---|
| OS user and group | `cronomicon-runner` | `cronomicon-runner-tax` |
| unit | `cronomicon-runner.service` | `cronomicon-runner-tax.service` |
| state (identity, keys, `known_hosts`, mirrors, run directories) | `/var/lib/cronomicon-runner` | `/var/lib/cronomicon-runner-tax` |
| config (`runner.env`, checkout token, vault password, CA) | `/etc/cronomicon-runner` | `/etc/cronomicon-runner-tax` |
| default runner name | `<hostname>` | `<hostname>-tax` |
| binary | `/usr/local/bin/cronomicon-runner` | the same file |

The separate user and group are the separation: `/dev/shm`, where an agent
writes key material for a run, is one directory for the machine, and the config
files are readable by the group. The installer has no option to share either.
Each instance has a whole unit of its own, not an instance of a template,
because the installer decides the unit's hardening per install.
`cronomicon-runner.service` in this directory stays the reference copy of the
default unit. The binary is shared, so upgrading it restarts every agent unit
on the machine (the Runners view's upgrade command does). The runner install
guide has the procedure, the per-instance limits and how to remove an instance.

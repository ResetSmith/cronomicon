# Go / no-go checklist (Phase D.6)

Sign-off gate for production. Each item is **Automated** (CI-enforced, green now),
**Operator** (run against the live/staging stack), or **Doc** (artifact exists).
Go = every box checked against the actual deployment.

## A — Auth model
- [x] *(Automated)* Trusted-header identity + spoofed-header rejection + mode switch — `internal/auth`, `internal/api` security tests.
- [ ] *(Operator)* Real Authelia login resolves roles from `Remote-Groups`; an under-privileged user gets 403 on a role-gated route.

## B — Containerize & deploy
- [x] *(Doc/Tooling)* Dockerfile builds the UI in-image; compose stack; env matrix; healthchecks.
- [ ] *(Operator)* `docker compose up -d --build` brings the stack up; UI reachable only via TLS through the proxy; app port not reachable from the host; data survives `down && up`.

## C — Feature completion
- [x] *(Automated)* SMTP + Apprise dispatch, `/metrics`, Vault client (AppRole/KV v2) — `internal/notify`, `internal/metrics`, `internal/secrets` tests.
- [ ] *(Operator)* A failing run delivers a real email + Apprise push; Prometheus scrapes `/metrics`; (if Vault wired) a vault-source secret resolves.

## D — Hardening & verification
- [x] *(Automated)* Security-surface suite: spoofed-header rejection, unauth surface, dev-login gating.
- [ ] *(Release)* `make vuln` reports **zero reachable** vulnerabilities on the release commit. CI's `vuln` job gates every push, but re-run it at release time: the vulnerability DB moves independently of the code, so a commit that was clean when merged can be affected by an advisory published since. A reachable finding is a no-go until the dependency is bumped or the advisory is explicitly accepted in writing here.
- [ ] *(Operator)* `verify-deployment.sh` clean against the live stack (esp. the `APP_DIRECT_URL` spoofed-header probe).
- [ ] *(Operator)* Restore drill succeeds with the KEK intact (`backup-restore.md` / runbook D.2).
- [ ] *(Operator)* Staging smoke path green (D.3): GitLab sync + webhook, schedule publish 412-on-conflict, mock-runner poll/claim/log-stream/redaction, scheduled + manual + branching-workflow runs, activity/audit feeds.
- [ ] *(Operator)* Load/soak within targets (`loadtest/`, D.4): long-poll headroom, log-ingest throughput, SQLite WAL/busy_timeout/pool invariant, memory steady, 20s drain fits the grace period.

## Sign-off
- [ ] All Phase A–C exit criteria met; D.1 security review clean (`security-review.md`).
- [x] *(Doc)* Rollback plan documented (previous image tag + DB snapshot) — `runbook.md`.
- [x] *(Doc)* On-call runbook: restore, failed migration, Authelia/proxy outage degradation (T12), bootstrap-admin re-enable — `runbook.md`, `migrations-runbook.md`.
- [ ] **Go / no-go decision recorded** (owner, date, rollback owner).

---

**Status of this repo's contribution:** all *Automated* and *Doc/Tooling* items
are complete and green. The *Operator* items require a live/staging stack
(Docker + Authelia + S3 + a load environment) and are executed by the deploying
operator using the scripts and runbooks here — they cannot be performed from the
source repo. This checklist is the hand-off.

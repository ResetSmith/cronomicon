# Vault Agent sidecar for the Cronomicon runner (opt-in)

This directory contains a **stopgap** way to make **HashiCorp Vault the source of
truth for a runner's local secrets** — the sudo/`become` password, WinRM
passwords, or any other value a run resolves by NAME — without an operator ever
hand-typing them into `secrets.env`.

It is deliberately **outside the Cronomicon control plane**: a Vault Agent process
runs *beside* the runner, authenticates to Vault with its own AppRole, and
renders the secrets the runner needs into a file the runner sources at startup.
Cronomicon itself stays **names-only (D1)** — the server never sees or ships these
values. This is the "Option 1B, done outside Cronomicon" path from
`20260708-vault-integration.md`: it gets you Vault-as-source-of-truth
today and is fully reversible. The in-app "control plane reveals + pushes
values" model (Decision 1C in that plan) supersedes this later; nothing here
blocks that work.

```
   ┌──────────────┐   AppRole login    ┌─────────┐
   │ Vault Agent  │ ─────────────────► │  Vault  │
   │  (sidecar)   │ ◄───────────────── │ KV-v2   │
   └──────┬───────┘   KV read (lease)  └─────────┘
          │ renders (0640 root:amadeus-runner)
          ▼
   /etc/amadeus-runner/secrets.env
          │ EnvironmentFile=- (optional, sourced at start)
          ▼
   ┌──────────────────┐
   │ amadeus-runner   │  buildChildEnv forwards the NAME to the play when the
   │  (agent)         │  inventory references {{ lookup('env','NAME') }}
   └──────────────────┘
```

## Why this is safe(r) than a hand-managed `secrets.env`

- The secret lives in Vault, leased and auditable; the runner host holds only a
  short-lived rendered copy, re-rendered on rotation.
- The runner never has a Vault token of its own and never talks to Vault — only
  the sidecar does, under its own AppRole identity and ACL.
- Cronomicon's D1 posture is unchanged: no secret value ever crosses the
  control-plane → runner channel. The runner resolves the NAME locally exactly
  as it does today from a hand-written `secrets.env`.

## Files

| File | Installs to | Purpose |
|------|-------------|---------|
| `vault-agent.hcl` | `/etc/amadeus-runner/vault/vault-agent.hcl` | Vault Agent config: AppRole auto-auth + the template stanza |
| `secrets.env.ctmpl` | `/etc/amadeus-runner/vault/secrets.env.ctmpl` | Template that renders KV secrets into `NAME=value` lines |
| `amadeus-vault-agent.service` | `/etc/systemd/system/amadeus-vault-agent.service` | systemd unit for the sidecar |

The rendered output is `/etc/amadeus-runner/secrets.env`, which
`amadeus-runner.service` sources via an **optional** `EnvironmentFile=-` line
(the `-` prefix means the runner still starts cleanly when the sidecar is not
installed — this is non-breaking for existing runners).

## Prerequisites

- The `vault` binary (Vault Agent mode) on the runner host.
- A Vault KV-v2 mount holding the secrets, e.g. `amadeus/runners/<runner>`.
- An AppRole (`role_id` + `secret_id`) whose policy grants **read-only** access
  to only that path. Prefer delivering the `secret_id` **response-wrapped** and
  unwrapping it into the file below, so a long-lived `secret_id` never sits on
  disk.

Example policy (`amadeus-runner-nwd`):

```hcl
path "amadeus/data/runners/nwd/*" {
  capabilities = ["read"]
}
```

## Install (pilot, manual)

Run as root on the runner host (paths assume the standard layout from
`runner-install.sh`):

```bash
install -d -m 0750 -o root -g amadeus-runner /etc/amadeus-runner/vault
install -m 0640 vault-agent.hcl      /etc/amadeus-runner/vault/vault-agent.hcl
install -m 0640 secrets.env.ctmpl    /etc/amadeus-runner/vault/secrets.env.ctmpl
install -m 0644 amadeus-vault-agent.service /etc/systemd/system/

# AppRole material (0600 root) — role_id is not secret; secret_id is.
umask 077
printf '%s' "<ROLE_ID>"   > /etc/amadeus-runner/vault/role_id
printf '%s' "<SECRET_ID>" > /etc/amadeus-runner/vault/secret_id   # prefer response-wrapped, see below

# Point the sidecar at your Vault
sed -i 's#https://vault.example.com:8200#https://vault.your-domain:8200#' \
    /etc/amadeus-runner/vault/vault-agent.hcl

systemctl daemon-reload
systemctl enable --now amadeus-vault-agent
```

Response-wrapped `secret_id` (recommended): store the single-use wrap token
instead and let the agent unwrap it — set `secret_id_response_wrapping_path`
handling per your workflow, or unwrap once at provisioning time:

```bash
VAULT_TOKEN=<wrap-token> vault unwrap -field=secret_id > /etc/amadeus-runner/vault/secret_id
```

## Wire the runner to source the rendered file

`amadeus-runner.service` already carries the optional line:

```ini
EnvironmentFile=-/etc/amadeus-runner/secrets.env
```

`systemd` reads `EnvironmentFile` **only at service start**, so the runner picks
up a newly rendered value on its next (re)start. Two options for rotation:

1. **Manual (default, safest):** after Vault rotates the secret and the sidecar
   re-renders, `sudo systemctl restart amadeus-runner`. The runner drains active
   runs on SIGTERM (`TimeoutStopSec=300`).
2. **Automatic:** uncomment the `command` line in `secrets.env.ctmpl`'s template
   stanza (in `vault-agent.hcl`) so the sidecar runs
   `systemctl try-restart amadeus-runner` on every render. Note this triggers a
   drain-and-restart on rotation — fine for a pilot, but understand active runs
   are asked to wind down.

## Use it in an inventory

Nothing in your playbooks or inventory changes relative to the hand-managed
`secrets.env` — the runner resolves the NAME the same way. For the CVE-scan
example:

```ini
[nwd:vars]
ansible_user=admsa
ansible_ssh_private_key_file="{{ lookup('env','ansible_rh8_key') }}"
ansible_become_password="{{ lookup('env','NWD_BECOME_PASS') }}"
```

`NWD_BECOME_PASS` is now rendered from Vault by the sidecar rather than typed
into `secrets.env` by hand.

## Verify

```bash
# The sidecar authenticated and rendered
systemctl status amadeus-vault-agent
sudo test -f /etc/amadeus-runner/secrets.env && echo "rendered"

# The runner can read it (as the runner user), names present but not printed here
sudo runuser -u amadeus-runner -- bash -c 'set -a; . /etc/amadeus-runner/secrets.env; set +a; \
    [ -n "$NWD_BECOME_PASS" ] && echo "NWD_BECOME_PASS is set"'

# Re-run the job; the play should show the become escalation succeeding on hosts
# that require a sudo password.
```

## Scope & limitations

- This is a **pilot stopgap**, not the end state. It does not give Cronomicon
  visibility into which secrets a run needs, per-job least-privilege binding, or
  a UI. Those come with the 1C control-plane delivery work
  (`20260708-vault-integration.md`, un-built tasks).
- One sidecar renders one `secrets.env`; if runners on a host need different
  secret sets, give each its own KV path/AppRole and template.
- Keep the AppRole policy read-only and path-scoped. Rotate `secret_id`
  regularly; prefer response-wrapped delivery.

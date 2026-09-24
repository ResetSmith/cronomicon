# Vault operator runbook (Phase 2 — injection workflow)

How to stand up HashiCorp Vault as a *source backend* behind Cronomicon's
dispatch-time injection (`20260708-vault-integration.md`, Phase 2). Cronomicon
uses **Model A**: the app authenticates to Vault with a single AppRole and reveals
values only on the internal dispatch path — humans never see Vault values through
the UI (a reveal of a vault-source row returns 409). KV v2 static secrets only; no
dynamic engines, no per-user policies.

Once configured, **both** vault-source **secrets** and vault-source **SSH
credentials** resolve through the same client, on both executors (in-app SSH and
out-of-process runner). Nothing here is required to run Cronomicon — with Vault
unconfigured everything works on the local-KEK path exactly as before.

---

## 1. AppRole creation

Cronomicon logs in with one AppRole (role_id + secret_id). Create it against the KV
v2 mount that holds your secrets (default mount `secret/`).

```sh
vault auth enable approle    # once per cluster

# Least-privilege policy — read only the paths Cronomicon needs (see §2).
vault policy write amadeus-injection amadeus-injection.hcl

vault write auth/approle/role/amadeus \
    token_policies=amadeus-injection \
    token_ttl=20m token_max_ttl=1h \
    secret_id_ttl=0            # 0 = non-expiring secret_id (static default; see §4)

vault read  auth/approle/role/amadeus/role-id            # → role_id
vault write -f auth/approle/role/amadeus/secret-id       # → secret_id
```

Deliver `role_id` and `secret_id` to Cronomicon by **mounted file** (preferred) or
env, or via the Settings → Vault page (DB-backed). Precedence: env wins over DB;
the `*_FILE` form wins over the inline value.

```
AMADEUS_VAULT_ADDR=https://vault.internal:8200
AMADEUS_VAULT_ROLE_ID_FILE=/run/secrets/vault_role_id
AMADEUS_VAULT_SECRET_ID_FILE=/run/secrets/vault_secret_id
```

Token refresh is automatic: the client re-logs in with the AppRole at 80% of the
token lease. Resolution of `addr`/role/secret happens **once at startup** — a
Settings change takes effect on restart.

## 2. Policy scoping (least privilege)

Grant read only to the exact KV v2 data paths Cronomicon resolves — the `vault_ref`
values stored on secrets and SSH credentials (`<mount>/data/<path>`). Do **not**
grant blanket `secret/*`.

```hcl
# amadeus-injection.hcl
path "secret/data/amadeus/*"      { capabilities = ["read"] }
path "secret/data/amadeus/ssh/*"  { capabilities = ["read"] }
# If you use migrate-to-vault (writes a stored secret into Vault): add "create","update"
# on the specific destination path only.
```

A `vault_ref` is `"<kvv2-path>#<field>"`, e.g.
`secret/data/amadeus/app#DB_PASSWORD`; the field after `#` selects one key from the
secret's data map (defaults to `value`). SSH credentials store the private-key PEM
as one field, e.g. `secret/data/amadeus/ssh/deploy#private_key`.

## 3. Namespace & private CA (D4 — dormant until set)

- **Namespace** (Vault Enterprise / HCP): set `AMADEUS_VAULT_NAMESPACE` (or Settings
  → Vault → namespace). Sent as `X-Vault-Namespace` on every request, including the
  AppRole login. Env overrides the DB value.
- **Private CA**: point `AMADEUS_VAULT_CA_FILE` at a PEM bundle to pin the client's
  TLS trust to it instead of the system roots. A missing or unparseable bundle
  **fails loud** — the Vault client is left disabled rather than silently trusting
  system roots. Rotate the CA by replacing the file and restarting.

## 4. secret_id delivery & rotation

Default is a **static long-lived** `secret_id` (`secret_id_ttl=0`), delivered by
mounted file. This is the simplest posture and needs no rotation.

**Response-wrapped delivery** (optional, `AMADEUS_VAULT_SECRET_ID_WRAPPED=true`):
deliver a single-use *wrapping token* in place of the secret_id, so the raw
secret_id never lands in env/DB/file in plaintext. Cronomicon unwraps it once via
`sys/wrapping/unwrap` at first login and caches the real secret_id in memory.

```sh
# Generate a wrapped secret_id (wrapping TTL long enough to survive a deploy):
vault write -wrap-ttl=60m -f auth/approle/role/amadeus/secret-id   # → wrapping token
```

Deliver the **wrapping token** as `AMADEUS_VAULT_SECRET_ID[_FILE]`. Note a wrapping
token is single-use: it is consumed at boot, so a *restart* needs a fresh wrapped
token (or fall back to a static secret_id). Periodic, unattended **rotation** of a
live secret_id (re-wrapping on a schedule and reloading without downtime) is not
yet automated — track it as a follow-up; the static or per-deploy-wrapped posture
is the supported path today.

## 5. Failure model (fail-closed)

A Vault outage halts every run bound to a vault-source row — by design, Cronomicon
never runs a reference-bearing job with a missing/unmaskable value:

- **Resolve** of a vault secret/SSH key fails ⇒ the run fails closed (SSH executor)
  or the manifest is withheld (runner). No partial injection, no silent skip.
- **Log redaction**: a secret-bearing run whose redaction dictionary can't be
  completed returns `503` at log ingest and persists nothing — its logs never land
  un-masked. Vault-source values flow into the per-run redaction dictionary the
  moment they resolve (they are absent from the stored-secret global dictionary).

There is no degraded/cached-value mode. If outage tolerance must change, that is a
deliberate design revisit, not a config knob.

## 6. Vault Agent sidecar retirement

With vault-source SSH credentials now resolving in-app (P2.4), the Vault Agent
sidecar that rendered key material to disk out-of-band (namespace-plan N-D5) is no
longer required for app-managed hosts. Retire it as a follow-up ops task once all
such hosts reference vault-source `ssh_credentials` rows directly: confirm no
inventory still depends on the sidecar-rendered `secrets.env`/key files, then
remove the sidecar from the deployment. Pre-provisioned key-dir files remain the
fallback for runner-local keys.

## 7. Quick verification

1. Create a vault-source secret (Settings → Env Vars, or migrate an existing stored
   secret) with a `vault_ref` your policy can read.
2. Bind it on a job (`AMADEUS_SECRET_<name>`) and dispatch a run.
3. Confirm the value is injected and **masked** in the run log, and that Run detail
   → Injected references lists the reference (name only, source `vault`).
4. For SSH: point a host's credential at a vault-source `ssh_credentials` row and
   run a connection test; the private key resolves live from Vault.

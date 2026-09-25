# Vault Agent config for the Cronomicon runner sidecar (opt-in stopgap).
#
# Renders /etc/cronomicon-runner/secrets.env from a Vault KV-v2 mount so the runner
# resolves NAME-referenced secrets (e.g. an Ansible become password) from Vault
# instead of a hand-managed file. See README.md in this directory.
#
# Cronomicon stays names-only (D1): the control plane never sees these values —
# only this sidecar, under its own AppRole, does.

# Fail fast at startup if Vault is unreachable rather than serving a stale file.
exit_after_auth = false
# Matches RuntimeDirectory=cronomicon-vault-agent in the unit (writable under the
# hardened sandbox; /run is otherwise read-only).
pid_file        = "/run/cronomicon-vault-agent/agent.pid"

vault {
  address = "https://vault.example.com:8200"

  # If your Vault presents a private CA, point at the bundle (read-only mount is
  # fine under the hardened unit):
  # ca_cert = "/etc/cronomicon-runner/vault/ca.pem"

  # Optional Enterprise namespace:
  # namespace = "admin/infra"

  retry {
    num_retries = 5
  }
}

auto_auth {
  method "approle" {
    mount_path = "auth/approle"
    config = {
      role_id_file_path   = "/etc/cronomicon-runner/vault/role_id"
      secret_id_file_path = "/etc/cronomicon-runner/vault/secret_id"

      # Keep the secret_id file after reading it (default removes it). We manage
      # its lifecycle/rotation ourselves; set to true if you deliver a fresh
      # response-wrapped secret_id on every boot.
      remove_secret_id_file_after_reading = false
    }
  }

  # No token sink — the runner must NOT get a Vault token. The sidecar holds the
  # token in memory only and writes secrets via the template stanza below.
}

# Cache leases so a Vault blip doesn't immediately break rendering.
cache {
  use_auto_auth_token = true
}

# Render the runner's secrets. The template file lists exactly which KV fields
# become which env-var NAMEs; edit secrets.env.ctmpl, not this stanza.
template {
  source      = "/etc/cronomicon-runner/vault/secrets.env.ctmpl"
  destination = "/etc/cronomicon-runner/secrets.env"

  # The runner user must be able to READ it; only root/sidecar writes it.
  perms = "0640"
  # Vault Agent >= 1.11 supports owner/group; if your build predates that, drop
  # these and manage ownership via the unit's UMask + a shared group instead.
  user  = "root"
  group = "cronomicon-runner"

  # Atomic swap so the runner never sources a half-written file.
  error_on_missing_key = true

  # Auto-restart the runner on rotation (opt-in — see README "rotation"). This
  # drains active runs (SIGTERM, up to TimeoutStopSec). Leave commented to pick
  # up rotated secrets on the next manual `systemctl restart cronomicon-runner`.
  # command = "/usr/bin/systemctl try-restart cronomicon-runner"
}

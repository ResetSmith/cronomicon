#!/bin/bash
# runner-install.sh (R7.1 / R7.2)
# Installs and registers the Cronomicon runner agent as a systemd service on Ubuntu/Debian and RedHat-family systems.
# Must be run as root.

set -e

# --- Default Values ---
SERVER_URL=""
REG_TOKEN=""
REG_TOKEN_SRC=""
REG_TOKEN_STDIN=0
REG_TOKEN_INLINE=0
RUNNER_NAME=$(hostname)
# Empty = auto-detect: the agent probes the host's toolchains at startup and
# claims the matching run-types. -c writes an explicit (narrowing) override.
CAPABILITIES=""
BINARY_PATH=""
DOWNLOAD=0
KNOWN_HOSTS_SRC=""
KEY_DIR_SRC=""
KEY_MAP_SPEC=""
GENERATE_KEY_NAME=""
CA_CERT_SRC=""
INVENTORY=""
LOCAL_INVENTORY_SRC=""
# Ansible checkout/vault (Phase 3). Secrets are NEVER accepted as flag values
# (ps/shell-history exposure) — only as a file to install or a hidden stdin
# prompt.
ALLOW_CHECKOUT=0
CHECKOUT_REPOS=""
CHECKOUT_TOKEN_SRC=""
CHECKOUT_TOKEN_STDIN=0
VAULT_PASS_SRC=""
VAULT_PASS_STDIN=0

# Install destinations (fixed layout — matches the hardened unit's paths).
STATE_DIR="/var/lib/amadeus-runner"
CONF_DIR="/etc/amadeus-runner"
KEYS_DEST="${STATE_DIR}/keys"
KNOWN_HOSTS_DEST="${STATE_DIR}/known_hosts"
CA_CERT_DEST="${CONF_DIR}/ca.pem"
LOCAL_INVENTORY_DEST="${CONF_DIR}/inventory.json"
CHECKOUT_TOKEN_DEST="${CONF_DIR}/checkout-token"
VAULT_PASS_DEST="${CONF_DIR}/vault-pass"

# --- Helper: Print Usage ---
usage() {
  echo "Usage: $0 [options]"
  echo "Options:"
  echo "  -s, --server <url>         Cronomicon server URL (e.g., https://amadeus.example.com) [REQUIRED]"
  echo "  -t, --token <token>        Registration token (crn_reg_*). DISCOURAGED: a flag"
  echo "                             value is visible in ps(1) and shell history. Prefer"
  echo "                             --token-file, or '--token -' to be prompted."
  echo "      --token-file <path>    Read the registration token from a file [PREFERRED]"
  echo "      --token -              Prompt for the registration token on stdin (input"
  echo "                             hidden). Required when the token is a long-lived"
  echo "                             CRONOMICON_RUNNER_BOOTSTRAP_TOKEN rather than a spent"
  echo "                             single-use crn_reg_* value."
  echo "  -n, --name <name>          Runner display name (default: hostname)"
  echo "  -c, --capabilities <list>  Comma-separated run-type capabilities. Default: omitted —"
  echo "                             the agent auto-detects the host's toolchains at startup."
  echo "                             Set explicitly to narrow what this runner claims."
  echo "  -b, --binary <path>        Path to pre-built amadeus-runner binary"
  echo "      --download             Download the amadeus-runner binary from the server"
  echo "                             (--server)/agents/, verify its SHA-256 checksum, and"
  echo "                             install it. Falls back to -b/auto-search when the"
  echo "                             server does not bundle binaries. Requires curl."
  echo "      --known-hosts <file>   OpenSSH known_hosts file to install for target host-key"
  echo "                             verification (the agent REFUSES SSH runs without one)"
  echo "      --key-dir <dir>        Directory of private keys to copy into ${KEYS_DEST}"
  echo "                             (mutually exclusive with --key-map)"
  echo "      --key-map <spec>       NAME=path,NAME2=path2 key map; each file is copied into"
  echo "                             ${KEYS_DEST} (mutually exclusive with --key-dir)"
  echo "      --generate-key <NAME>  Generate an ed25519 key (no passphrase) at"
  echo "                             ${KEYS_DEST}/<NAME> and print its public key to add to"
  echo "                             each target's authorized_keys. Refuses to overwrite."
  echo "      --ca-cert <pem>        Private CA bundle (PEM) to trust for server TLS"
  echo "      --inventory <mode>     Inventory mode: amadeus (default) or local"
  echo "      --local-inventory <f>  Local inventory JSON (required with --inventory local)"
  echo "      --allow-checkout       Opt this runner into pinned playbook checkout"
  echo "      --checkout-repos <csv> Allowlist of repo clone URLs the runner may check out"
  echo "      --checkout-token-file <path>"
  echo "                             Install a read-only deploy-token file to"
  echo "                             ${CHECKOUT_TOKEN_DEST} (0640 root:amadeus-runner)"
  echo "      --checkout-token -     Prompt for the deploy token on stdin (input hidden) and"
  echo "                             write it to ${CHECKOUT_TOKEN_DEST}. NEVER pass the token"
  echo "                             as a flag value (visible in ps / shell history)."
  echo "      --vault-pass-file <path>"
  echo "                             Install an Ansible Vault password file to"
  echo "                             ${VAULT_PASS_DEST} (0640 root:amadeus-runner)"
  echo "      --vault-pass -         Prompt for the vault password on stdin (input hidden)."
  echo "  -h, --help                 Show this help message"
  exit "${1:-1}"
}

# reject_secret_value refuses a secret passed as a flag VALUE (it would leak via
# ps(1) and shell history). Callers accept only a file path or a hidden stdin
# prompt.
reject_secret_value() {
  echo "Error: refusing to read a secret from a command-line value (visible in ps and shell history)." >&2
  echo "       Use '$1 -' to type it at a hidden prompt, or ${2} <path> to install a file." >&2
  exit 1
}

# --- Parse Arguments ---
require_value() {
  if [ -z "$2" ]; then
    echo "Error: $1 requires a value." >&2
    exit 1
  fi
}

while [[ "$#" -gt 0 ]]; do
  case $1 in
    -s|--server) require_value "$1" "${2:-}"; SERVER_URL="$2"; shift ;;
    # A per-install crn_reg_* is single-use and expires in 24h, so an inline value
    # was historically harmless. A bootstrap token is multi-use and never expires,
    # so the same flag now carries a permanent credential: inline is accepted for
    # compatibility but warned, and the file/stdin forms are the documented path.
    -t|--token)
      case "${2:-}" in
        "") REG_TOKEN_STDIN=1 ;;
        -)  REG_TOKEN_STDIN=1; shift ;;
        -*) REG_TOKEN_STDIN=1 ;;
        *)  REG_TOKEN="$2"; REG_TOKEN_INLINE=1; shift ;;
      esac ;;
    --token-file) require_value "$1" "${2:-}"; REG_TOKEN_SRC="$2"; shift ;;
    -n|--name) require_value "$1" "${2:-}"; RUNNER_NAME="$2"; shift ;;
    -c|--capabilities) require_value "$1" "${2:-}"; CAPABILITIES="$2"; shift ;;
    -b|--binary) require_value "$1" "${2:-}"; BINARY_PATH="$2"; shift ;;
    --download) DOWNLOAD=1 ;;
    --known-hosts) require_value "$1" "${2:-}"; KNOWN_HOSTS_SRC="$2"; shift ;;
    --key-dir) require_value "$1" "${2:-}"; KEY_DIR_SRC="$2"; shift ;;
    --key-map) require_value "$1" "${2:-}"; KEY_MAP_SPEC="$2"; shift ;;
    --generate-key) require_value "$1" "${2:-}"; GENERATE_KEY_NAME="$2"; shift ;;
    --ca-cert) require_value "$1" "${2:-}"; CA_CERT_SRC="$2"; shift ;;
    --inventory) require_value "$1" "${2:-}"; INVENTORY="$2"; shift ;;
    --local-inventory) require_value "$1" "${2:-}"; LOCAL_INVENTORY_SRC="$2"; shift ;;
    --allow-checkout) ALLOW_CHECKOUT=1 ;;
    --checkout-repos) require_value "$1" "${2:-}"; CHECKOUT_REPOS="$2"; shift ;;
    --checkout-token-file) require_value "$1" "${2:-}"; CHECKOUT_TOKEN_SRC="$2"; shift ;;
    # A secret: accept only the stdin sentinel ('-') or no value (prompt); a real
    # value is refused so it never lands in ps/history.
    --checkout-token)
      case "${2:-}" in
        "") CHECKOUT_TOKEN_STDIN=1 ;;
        -)  CHECKOUT_TOKEN_STDIN=1; shift ;;
        -*) CHECKOUT_TOKEN_STDIN=1 ;;
        *)  reject_secret_value "--checkout-token" "--checkout-token-file" ;;
      esac ;;
    --vault-pass-file) require_value "$1" "${2:-}"; VAULT_PASS_SRC="$2"; shift ;;
    --vault-pass)
      case "${2:-}" in
        "") VAULT_PASS_STDIN=1 ;;
        -)  VAULT_PASS_STDIN=1; shift ;;
        -*) VAULT_PASS_STDIN=1 ;;
        *)  reject_secret_value "--vault-pass" "--vault-pass-file" ;;
      esac ;;
    -h|--help) usage 0 ;;
    # Strip any '=value' before echoing: a mistaken --checkout-token=SECRET must
    # not print the secret to stdout/logs (the =-form isn't a supported secret
    # entry path — use '--checkout-token -' or --checkout-token-file).
    *) echo "Unknown parameter: ${1%%=*}"; usage ;;
  esac
  shift
done

# --- Required Arguments Validation ---
if [ -z "$SERVER_URL" ]; then
  echo "Error: Server URL (-s | --server) is required." >&2
  usage
fi

# POSIX `case` (not the bash-only `[[ =~ ]]`) so the guard still fires when the
# script is piped to `sh`/dash instead of bash — otherwise a bare host sails
# through and the agent dies at register with `unsupported protocol scheme ""`.
case "$SERVER_URL" in
  http://*|https://*) ;;
  *)
    echo "Warning: Server URL does not start with http:// or https://."
    echo ">> Automatically prepending 'https://' to make it: https://${SERVER_URL}"
    SERVER_URL="https://${SERVER_URL}"
    ;;
esac

if [ -z "$REG_TOKEN" ] && [ -z "$REG_TOKEN_SRC" ] && [ "$REG_TOKEN_STDIN" != 1 ]; then
  echo "Error: a registration token is required (--token-file <path>, '--token -', or -t <token>)." >&2
  usage
fi
# Exactly one source.
if { [ -n "$REG_TOKEN_SRC" ] && [ "$REG_TOKEN_STDIN" = 1 ]; } || \
   { [ -n "$REG_TOKEN_SRC" ] && [ "$REG_TOKEN_INLINE" = 1 ]; } || \
   { [ "$REG_TOKEN_STDIN" = 1 ] && [ "$REG_TOKEN_INLINE" = 1 ]; }; then
  echo "Error: pass the registration token exactly one way (--token-file, '--token -', or -t)." >&2
  exit 1
fi
if [ "$REG_TOKEN_INLINE" = 1 ]; then
  echo "Warning: the registration token was passed as a command-line value, which is" >&2
  echo "         visible in ps(1) and recorded in shell history. Use --token-file <path>" >&2
  echo "         or '--token -' instead — especially for a bootstrap token, which does" >&2
  echo "         not expire." >&2
fi

# --- Flag-Combination Validation ---
if [ -n "$KEY_DIR_SRC" ] && [ -n "$KEY_MAP_SPEC" ]; then
  echo "Error: --key-dir and --key-map are mutually exclusive." >&2
  exit 1
fi

# --generate-key NAME must be a plain filename (it becomes ${KEYS_DEST}/<NAME>),
# not a path — same rule as a --key-map NAME.
if [ -n "$GENERATE_KEY_NAME" ]; then
  if [[ "$GENERATE_KEY_NAME" == */* || "$GENERATE_KEY_NAME" == *..* ]]; then
    echo "Error: --generate-key NAME must be a plain name, not a path (got: ${GENERATE_KEY_NAME})." >&2
    exit 1
  fi
fi

case "$INVENTORY" in
  ""|amadeus|local) ;;
  *) echo "Error: --inventory must be 'amadeus' or 'local' (got: ${INVENTORY})." >&2; exit 1 ;;
esac

if [ "$INVENTORY" = "local" ] && [ -z "$LOCAL_INVENTORY_SRC" ]; then
  echo "Error: --inventory local requires --local-inventory <file> (the agent cannot resolve scopes without it)." >&2
  exit 1
fi

if [ -n "$LOCAL_INVENTORY_SRC" ] && [ "$INVENTORY" != "local" ]; then
  echo "Error: --local-inventory only applies with --inventory local." >&2
  exit 1
fi

# --- Checkout / vault validation (Phase 3) ---
# A file source and a stdin prompt for the same secret is a usage error (check
# before the TTY guard so the message is specific).
if [ -n "$CHECKOUT_TOKEN_SRC" ] && [ "$CHECKOUT_TOKEN_STDIN" = 1 ]; then
  echo "Error: pass a checkout token either as --checkout-token-file <path> OR --checkout-token - (not both)." >&2
  exit 1
fi
if [ -n "$VAULT_PASS_SRC" ] && [ "$VAULT_PASS_STDIN" = 1 ]; then
  echo "Error: pass a vault password either as --vault-pass-file <path> OR --vault-pass - (not both)." >&2
  exit 1
fi
# A stdin prompt needs a real terminal; a piped install (curl | sudo bash) has
# the script on stdin, so it must use the *-file flags instead. Fail before any
# mutation.
if [ "$REG_TOKEN_STDIN" = 1 ] && [ ! -t 0 ]; then
  echo "Error: cannot prompt for the registration token — stdin is not a terminal (piped install?)." >&2
  echo "       Use --token-file <path> to read it from a pre-existing file instead." >&2
  exit 1
fi
if [ "$CHECKOUT_TOKEN_STDIN" = 1 ] && [ ! -t 0 ]; then
  echo "Error: cannot prompt for the checkout token — stdin is not a terminal (piped install?)." >&2
  echo "       Use --checkout-token-file <path> to install a pre-existing file instead." >&2
  exit 1
fi
if [ "$VAULT_PASS_STDIN" = 1 ] && [ ! -t 0 ]; then
  echo "Error: cannot prompt for the vault password — stdin is not a terminal (piped install?)." >&2
  echo "       Use --vault-pass-file <path> to install a pre-existing file instead." >&2
  exit 1
fi

[ -z "$INVENTORY" ] && INVENTORY="amadeus"

if [ -n "$KEY_MAP_SPEC" ]; then
  if [[ ! "$KEY_MAP_SPEC" =~ ^[^=,]+=[^=,]+(,[^=,]+=[^=,]+)*$ ]]; then
    echo "Error: --key-map must look like NAME=path or NAME=path,NAME2=path2 (got: ${KEY_MAP_SPEC})." >&2
    exit 1
  fi
  IFS=',' read -ra KEY_MAP_ENTRIES <<< "$KEY_MAP_SPEC"
  SEEN_NAMES=","
  for entry in "${KEY_MAP_ENTRIES[@]}"; do
    key_name="${entry%%=*}"
    if [[ "$key_name" == */* || "$key_name" == *..* ]]; then
      echo "Error: --key-map NAME must be a plain name, not a path (got: ${key_name})." >&2
      exit 1
    fi
    if [[ "$SEEN_NAMES" == *",${key_name},"* ]]; then
      echo "Error: --key-map has duplicate NAME: ${key_name}." >&2
      exit 1
    fi
    SEEN_NAMES="${SEEN_NAMES}${key_name},"
  done
fi

# --- Root Check ---
if [ "$EUID" -ne 0 ]; then
  echo "Error: This script must be run as root (sudo)." >&2
  exit 1
fi

# --- Source-File Existence Checks (fail before mutating anything) ---
if [ -n "$KNOWN_HOSTS_SRC" ] && [ ! -f "$KNOWN_HOSTS_SRC" ]; then
  echo "Error: known_hosts file not found: $KNOWN_HOSTS_SRC" >&2
  exit 1
fi

if [ -n "$KEY_DIR_SRC" ]; then
  if [ ! -d "$KEY_DIR_SRC" ]; then
    echo "Error: key directory not found: $KEY_DIR_SRC" >&2
    exit 1
  fi
  if ! find "$KEY_DIR_SRC" -maxdepth 1 -type f | read -r; then
    echo "Error: key directory contains no files: $KEY_DIR_SRC" >&2
    exit 1
  fi
fi

if [ -n "$KEY_MAP_SPEC" ]; then
  for entry in "${KEY_MAP_ENTRIES[@]}"; do
    key_path="${entry#*=}"
    if [ ! -f "$key_path" ]; then
      echo "Error: key file not found: $key_path (from --key-map entry: $entry)" >&2
      exit 1
    fi
  done
fi

if [ -n "$CA_CERT_SRC" ] && [ ! -f "$CA_CERT_SRC" ]; then
  echo "Error: CA certificate not found: $CA_CERT_SRC" >&2
  exit 1
fi

if [ -n "$LOCAL_INVENTORY_SRC" ] && [ ! -f "$LOCAL_INVENTORY_SRC" ]; then
  echo "Error: local inventory file not found: $LOCAL_INVENTORY_SRC" >&2
  exit 1
fi

if [ -n "$REG_TOKEN_SRC" ] && [ ! -f "$REG_TOKEN_SRC" ]; then
  echo "Error: registration token file not found: $REG_TOKEN_SRC" >&2
  exit 1
fi
if [ -n "$CHECKOUT_TOKEN_SRC" ] && [ ! -f "$CHECKOUT_TOKEN_SRC" ]; then
  echo "Error: checkout token file not found: $CHECKOUT_TOKEN_SRC" >&2
  exit 1
fi

if [ -n "$VAULT_PASS_SRC" ] && [ ! -f "$VAULT_PASS_SRC" ]; then
  echo "Error: vault password file not found: $VAULT_PASS_SRC" >&2
  exit 1
fi

# --- OS Detection ---
if [ -f /etc/os-release ]; then
  . /etc/os-release
  OS_ID=$ID
  OS_LIKE=$ID_LIKE
else
  echo "Error: Cannot detect OS. /etc/os-release not found." >&2
  exit 1
fi

echo ">> Detected OS: $NAME ($VERSION)"

# --- Missing-toolchain remediation (Phase 3) ---
# Checkout/vault imply this is meant to be an Ansible runner. If ansible-core
# isn't installed, capability auto-detection (Phase 1) won't claim `ansible` and
# such jobs will queue forever. Print a distro-matched hint — do NOT install:
# package management belongs to config management, not this script.
if [ "$ALLOW_CHECKOUT" = 1 ] || [ -n "$CHECKOUT_REPOS" ] || [ -n "$CHECKOUT_TOKEN_SRC" ] || \
   [ "$CHECKOUT_TOKEN_STDIN" = 1 ] || [ -n "$VAULT_PASS_SRC" ] || [ "$VAULT_PASS_STDIN" = 1 ]; then
  if ! command -v ansible-playbook >/dev/null 2>&1; then
    case "${OS_ID} ${OS_LIKE}" in
      *debian*|*ubuntu*) install_hint="sudo apt update && sudo apt install -y ansible-core" ;;
      *rhel*|*fedora*|*centos*|*rocky*|*almalinux*) install_hint="sudo dnf install -y ansible-core" ;;
      *) install_hint="install ansible-core with your package manager" ;;
    esac
    echo "!! NOTE: checkout/vault flags were given but 'ansible-playbook' is not on PATH."
    echo "!!       Capability auto-detection will NOT claim 'ansible', so ansible jobs"
    echo "!!       will stay queued. Install the toolchain, then restart the service:"
    echo "!!         ${install_hint}"
    echo "!!         sudo systemctl restart amadeus-runner"
  fi
fi

# --- Download the Agent Binary from the Server (--download) ---
# The server can bundle cross-compiled agent binaries + SHA256SUMS (served at
# /agents/ — provisioning D1). Download beats -b/auto-search when it succeeds;
# an unavailable server-side binary falls back, but a checksum mismatch is a
# HARD failure (corrupted download or tampering), never a fallback.
if [ "$DOWNLOAD" = "1" ]; then
  if ! command -v curl >/dev/null 2>&1; then
    echo "Error: --download requires curl." >&2
    exit 1
  fi
  if ! command -v sha256sum >/dev/null 2>&1; then
    echo "Error: --download requires sha256sum (coreutils) for checksum verification." >&2
    exit 1
  fi
  case "$(uname -m)" in
    x86_64|amd64) AGENT_ARCH="amd64" ;;
    aarch64|arm64) AGENT_ARCH="arm64" ;;
    *)
      echo "Error: no published amadeus-runner binary for architecture $(uname -m) — use -b/--binary." >&2
      exit 1
      ;;
  esac
  AGENT_FILE="amadeus-runner-linux-${AGENT_ARCH}"
  CURL_OPTS=(-fsSL)
  [ -n "$CA_CERT_SRC" ] && CURL_OPTS+=(--cacert "$CA_CERT_SRC")
  DL_DIR=$(mktemp -d)
  trap 'rm -rf "$DL_DIR"' EXIT
  echo ">> Downloading ${AGENT_FILE} from ${SERVER_URL}/agents/ ..."
  if curl "${CURL_OPTS[@]}" -o "${DL_DIR}/${AGENT_FILE}" "${SERVER_URL}/agents/${AGENT_FILE}" &&
     curl "${CURL_OPTS[@]}" -o "${DL_DIR}/SHA256SUMS" "${SERVER_URL}/agents/SHA256SUMS"; then
    echo ">> Verifying checksum..."
    if ! (cd "$DL_DIR" && grep " ${AGENT_FILE}\$" SHA256SUMS | sha256sum -c - >/dev/null 2>&1); then
      echo "Error: checksum verification FAILED for ${AGENT_FILE} (mismatch, or no SHA256SUMS entry for it) — refusing to install." >&2
      exit 1
    fi
    echo ">> Checksum OK."
    BINARY_PATH="${DL_DIR}/${AGENT_FILE}"
  else
    echo ">> WARNING: could not download the agent binary from ${SERVER_URL}/agents/"
    echo ">>          (this deployment may not bundle binaries — see the install guide §2)."
    echo ">>          Falling back to a local binary (-b / auto-search)."
  fi
fi

# --- Binary Location Check ---
if [ -z "$BINARY_PATH" ]; then
  # Try to find a pre-built binary in common locations
  if [ -f "./amadeus-runner" ]; then
    BINARY_PATH="./amadeus-runner"
  elif [ -f "./bin/amadeus-runner" ]; then
    BINARY_PATH="./bin/amadeus-runner"
  elif [ -f "backend/bin/amadeus-runner" ]; then
    BINARY_PATH="backend/bin/amadeus-runner"
  else
    echo "Error: amadeus-runner binary not specified (-b | --binary) and not found in default paths." >&2
    echo "       (Tip: --download fetches it from the Cronomicon server when the deployment bundles binaries.)" >&2
    exit 1
  fi
fi

if [ ! -f "$BINARY_PATH" ]; then
  echo "Error: Binary not found at path: $BINARY_PATH" >&2
  exit 1
fi

echo ">> Using binary: $BINARY_PATH"

# --- Determine Shell for System Account ---
SHELL_PATH="/usr/sbin/nologin"
if [ ! -x "$SHELL_PATH" ]; then
  SHELL_PATH="/bin/false"
fi

# --- Create amadeus-runner System Account ---
if ! getent group amadeus-runner >/dev/null; then
  echo ">> Creating group: amadeus-runner"
  groupadd --system amadeus-runner
fi

if ! getent passwd amadeus-runner >/dev/null; then
  echo ">> Creating user: amadeus-runner"
  useradd --system \
          --gid amadeus-runner \
          --home-dir "$STATE_DIR" \
          --shell "$SHELL_PATH" \
          --create-home \
          amadeus-runner
else
  echo ">> User amadeus-runner already exists. Ensuring home directory is created..."
  mkdir -p "$STATE_DIR"
  chown amadeus-runner:amadeus-runner "$STATE_DIR"
fi

# Set directory permissions
chmod 0750 "$STATE_DIR"

# --- Create Config Directories ---
echo ">> Creating configuration directories..."
mkdir -p "$CONF_DIR"
chown root:amadeus-runner "$CONF_DIR"
chmod 0750 "$CONF_DIR"

# --- Install Binary ---
echo ">> Installing binary to /usr/local/bin..."
install -m 0755 "$BINARY_PATH" /usr/local/bin/amadeus-runner

# --- Install known_hosts (target host-key verification) ---
KNOWN_HOSTS_LINE=""
if [ -n "$KNOWN_HOSTS_SRC" ]; then
  echo ">> Installing known_hosts to ${KNOWN_HOSTS_DEST}..."
  install -o amadeus-runner -g amadeus-runner -m 0640 "$KNOWN_HOSTS_SRC" "$KNOWN_HOSTS_DEST"
  KNOWN_HOSTS_LINE="CRONOMICON_RUNNER_KNOWN_HOSTS=${KNOWN_HOSTS_DEST}"
fi

# --- Install Private Keys (model b — the runner holds its own keys) ---
# The keys/ dir and CRONOMICON_RUNNER_KEY_DIR are ALWAYS provisioned so the installer
# owns the convention up front: a key dropped in later (or generated below) Just
# Works without a runner.env hand-edit. --key-dir / --key-map / --generate-key
# populate it; without them the dir is created empty. KEYS_PROVISIONED tracks
# whether any actual key material landed (drives the post-install guidance).
install -d -o amadeus-runner -g amadeus-runner -m 0700 "$KEYS_DEST"
KEY_DIR_LINE="CRONOMICON_RUNNER_KEY_DIR=${KEYS_DEST}"
KEY_MAP_LINE=""
KEYS_PROVISIONED=0
if [ -n "$KEY_DIR_SRC" ]; then
  echo ">> Copying keys from ${KEY_DIR_SRC} to ${KEYS_DEST}..."
  find "$KEY_DIR_SRC" -maxdepth 1 -type f -print0 | while IFS= read -r -d '' key_file; do
    install -o amadeus-runner -g amadeus-runner -m 0600 "$key_file" "${KEYS_DEST}/$(basename "$key_file")"
  done
  KEYS_PROVISIONED=1
elif [ -n "$KEY_MAP_SPEC" ]; then
  echo ">> Copying key-map keys to ${KEYS_DEST}..."
  INSTALLED_MAP=""
  for entry in "${KEY_MAP_ENTRIES[@]}"; do
    key_name="${entry%%=*}"
    key_path="${entry#*=}"
    install -o amadeus-runner -g amadeus-runner -m 0600 "$key_path" "${KEYS_DEST}/${key_name}"
    INSTALLED_MAP="${INSTALLED_MAP:+${INSTALLED_MAP},}${key_name}=${KEYS_DEST}/${key_name}"
  done
  KEY_MAP_LINE="CRONOMICON_RUNNER_KEY_MAP=${INSTALLED_MAP}"
  KEYS_PROVISIONED=1
fi

# --generate-key <NAME>: mint an ed25519 key the runner owns, into the key-dir the
# agent already searches by NAME. The private key never leaves the host; the
# public key is printed for the operator to add to each target's authorized_keys.
GENERATED_PUB=""
if [ -n "$GENERATE_KEY_NAME" ]; then
  if ! command -v ssh-keygen >/dev/null 2>&1; then
    echo "Error: --generate-key needs ssh-keygen (install openssh-client/openssh)." >&2
    exit 1
  fi
  gen_dest="${KEYS_DEST}/${GENERATE_KEY_NAME}"
  if [ -e "$gen_dest" ] || [ -e "${gen_dest}.pub" ]; then
    echo "Error: refusing to overwrite an existing key at ${gen_dest}." >&2
    echo "       Remove it first, or choose a different --generate-key NAME." >&2
    exit 1
  fi
  echo ">> Generating ed25519 key ${gen_dest} (no passphrase)..."
  ssh-keygen -t ed25519 -N '' -C "amadeus-runner:${GENERATE_KEY_NAME}" -f "$gen_dest" >/dev/null
  chown amadeus-runner:amadeus-runner "$gen_dest" "${gen_dest}.pub"
  chmod 0600 "$gen_dest"
  chmod 0644 "${gen_dest}.pub"
  GENERATED_PUB="$(cat "${gen_dest}.pub")"
  KEYS_PROVISIONED=1
fi

# --- Install CA Certificate (private/internal CA for server TLS) ---
CA_CERT_LINE=""
if [ -n "$CA_CERT_SRC" ]; then
  echo ">> Installing CA certificate to ${CA_CERT_DEST}..."
  install -o root -g amadeus-runner -m 0640 "$CA_CERT_SRC" "$CA_CERT_DEST"
  CA_CERT_LINE="CRONOMICON_RUNNER_CA_CERT=${CA_CERT_DEST}"
fi

# --- Install Local Inventory (inventory=local, T-b isolated segments) ---
LOCAL_INVENTORY_LINE=""
if [ -n "$LOCAL_INVENTORY_SRC" ]; then
  echo ">> Installing local inventory to ${LOCAL_INVENTORY_DEST}..."
  install -o root -g amadeus-runner -m 0640 "$LOCAL_INVENTORY_SRC" "$LOCAL_INVENTORY_DEST"
  LOCAL_INVENTORY_LINE="CRONOMICON_RUNNER_LOCAL_INVENTORY=${LOCAL_INVENTORY_DEST}"
fi

# --- Install Ansible secrets (Phase 3 — checkout deploy token, vault password) ---
# Same custody posture as runner.env: 0640 root:amadeus-runner, so the agent
# (group amadeus-runner) can read but the file is never world-readable. The
# server never sees these bytes. Two paths: install a file the operator staged,
# or read one typed at a hidden prompt (echo off).
#
# install_secret <dest> writes the mode-0640 file first (so content is never
# world-readable even briefly), then fills it.
install_secret_file() {  # <src> <dest> <label>
  echo ">> Installing $3 to $2..."
  install -o root -g amadeus-runner -m 0640 "$1" "$2"
}
prompt_secret() {  # <dest> <label>
  local secret=""
  printf '>> Enter %s (input hidden): ' "$2" >&2
  # `|| true` so a read EOF (Ctrl-D) doesn't trip `set -e` before the empty check.
  read -rs secret || true
  echo >&2
  if [ -z "$secret" ]; then
    echo "Error: empty $2 — aborting (nothing written to $1)." >&2
    exit 1
  fi
  install -o root -g amadeus-runner -m 0640 /dev/null "$1"
  printf '%s' "$secret" > "$1"
  unset secret
  echo ">> Wrote $2 to $1."
}

# The registration token is consumed once, at first contact, and is written into
# the env file rather than installed as its own secret file — so unlike the
# checkout token it is resolved into a VARIABLE here. Both non-inline forms keep
# the value out of argv, which is the whole point.
if [ -n "$REG_TOKEN_SRC" ]; then
  # Strip a trailing newline (and any surrounding whitespace) so an editor-saved
  # token file works without the operator having to use `printf`.
  REG_TOKEN="$(tr -d '\r\n' < "$REG_TOKEN_SRC" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  if [ -z "$REG_TOKEN" ]; then
    echo "Error: registration token file is empty: $REG_TOKEN_SRC" >&2
    exit 1
  fi
  echo ">> Read registration token from $REG_TOKEN_SRC."
elif [ "$REG_TOKEN_STDIN" = 1 ]; then
  printf '>> Enter registration token (input hidden): ' >&2
  # `|| true` so a read EOF (Ctrl-D) doesn't trip `set -e` before the empty check.
  read -rs REG_TOKEN || true
  echo >&2
  if [ -z "$REG_TOKEN" ]; then
    echo "Error: empty registration token — aborting." >&2
    exit 1
  fi
fi

CHECKOUT_TOKEN_FILE_LINE=""
if [ -n "$CHECKOUT_TOKEN_SRC" ]; then
  install_secret_file "$CHECKOUT_TOKEN_SRC" "$CHECKOUT_TOKEN_DEST" "checkout deploy token"
  CHECKOUT_TOKEN_FILE_LINE="CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE=${CHECKOUT_TOKEN_DEST}"
elif [ "$CHECKOUT_TOKEN_STDIN" = 1 ]; then
  prompt_secret "$CHECKOUT_TOKEN_DEST" "checkout deploy token"
  CHECKOUT_TOKEN_FILE_LINE="CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE=${CHECKOUT_TOKEN_DEST}"
fi

VAULT_PASS_LINE=""
if [ -n "$VAULT_PASS_SRC" ]; then
  install_secret_file "$VAULT_PASS_SRC" "$VAULT_PASS_DEST" "vault password"
  VAULT_PASS_LINE="CRONOMICON_RUNNER_VAULT_PASSWORD_FILE=${VAULT_PASS_DEST}"
elif [ "$VAULT_PASS_STDIN" = 1 ]; then
  prompt_secret "$VAULT_PASS_DEST" "vault password"
  VAULT_PASS_LINE="CRONOMICON_RUNNER_VAULT_PASSWORD_FILE=${VAULT_PASS_DEST}"
fi

# Checkout opt-in + repo allowlist are plain env writes (not secrets).
ALLOW_CHECKOUT_LINE=""
[ "$ALLOW_CHECKOUT" = 1 ] && ALLOW_CHECKOUT_LINE="CRONOMICON_RUNNER_ALLOW_CHECKOUT=true"
CHECKOUT_REPOS_LINE=""
[ -n "$CHECKOUT_REPOS" ] && CHECKOUT_REPOS_LINE="CRONOMICON_RUNNER_CHECKOUT_REPOS=${CHECKOUT_REPOS}"

# --- Generate Config File ---
echo ">> Writing ${CONF_DIR}/runner.env..."
# Create with final ownership/mode BEFORE writing — the file carries the
# registration token, so it must never be world-readable, even briefly.
install -o root -g amadeus-runner -m 0640 /dev/null "${CONF_DIR}/runner.env"
{
  echo "# Configuration for the Cronomicon runner agent (Ubuntu/RedHat VM instance)"
  echo "CRONOMICON_RUNNER_SERVER=${SERVER_URL}"
  echo "CRONOMICON_RUNNER_REGISTRATION_TOKEN=${REG_TOKEN}"
  echo "CRONOMICON_RUNNER_NAME=${RUNNER_NAME}"
  # No -c ⇒ omit the var entirely: the agent probes the host's toolchains at
  # startup (unset = auto-detect; set explicitly to narrow).
  [ -n "$CAPABILITIES" ] && echo "CRONOMICON_RUNNER_CAPABILITIES=${CAPABILITIES}"
  echo "CRONOMICON_RUNNER_INVENTORY=${INVENTORY}"
  echo "CRONOMICON_RUNNER_IDENTITY_FILE=${STATE_DIR}/identity.json"
  [ -n "$KNOWN_HOSTS_LINE" ] && echo "$KNOWN_HOSTS_LINE"
  [ -n "$KEY_DIR_LINE" ] && echo "$KEY_DIR_LINE"
  [ -n "$KEY_MAP_LINE" ] && echo "$KEY_MAP_LINE"
  [ -n "$CA_CERT_LINE" ] && echo "$CA_CERT_LINE"
  [ -n "$LOCAL_INVENTORY_LINE" ] && echo "$LOCAL_INVENTORY_LINE"
  [ -n "$ALLOW_CHECKOUT_LINE" ] && echo "$ALLOW_CHECKOUT_LINE"
  [ -n "$CHECKOUT_REPOS_LINE" ] && echo "$CHECKOUT_REPOS_LINE"
  [ -n "$CHECKOUT_TOKEN_FILE_LINE" ] && echo "$CHECKOUT_TOKEN_FILE_LINE"
  [ -n "$VAULT_PASS_LINE" ] && echo "$VAULT_PASS_LINE"
  true
} > "${CONF_DIR}/runner.env"

# --- Sandbox self-probe ---
# Some RHEL8/systemd hosts STALL filesystem access inside the hardened sandbox
# (ProtectSystem/namespace/seccomp), so the agent's exec.LookPath/stat hang at
# startup and it crash-loops before registering. Reproduce the sandbox in a
# bounded transient unit and stat a file; if it stalls or fails, generate the
# unit with REDUCED hardening so the runner can start. Force with
# CRONOMICON_INSTALL_MINIMAL_HARDENING=1.
HARDENING_OK=1
if [ -n "${CRONOMICON_INSTALL_MINIMAL_HARDENING:-}" ]; then
  HARDENING_OK=0
  echo ">> Minimal hardening forced (CRONOMICON_INSTALL_MINIMAL_HARDENING set)."
elif ! command -v systemd-run >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
  echo ">> Sandbox self-probe skipped (systemd-run/timeout absent); applying full hardening."
else
  echo ">> Probing whether the hardened sandbox stalls filesystem access..."
  if timeout 20 systemd-run --quiet --pipe --wait --collect \
       -p ProtectSystem=strict -p ProtectHome=yes -p PrivateTmp=yes \
       -p PrivateDevices=yes -p RestrictNamespaces=yes \
       -p 'SystemCallFilter=@system-service' \
       /usr/bin/stat /usr/bin/env >/dev/null 2>&1; then
    echo ">> Sandbox self-probe OK — full hardening will be applied."
  else
    HARDENING_OK=0
    echo "!! WARNING: the hardened systemd sandbox stalled or failed on this host."
    echo "!!          Generating the unit with REDUCED hardening (NoNewPrivileges +"
    echo "!!          directory isolation kept; mount-namespace/seccomp dropped) so the"
    echo "!!          runner can start. Investigate this host's systemd; re-run the"
    echo "!!          installer to restore full hardening once fixed."
  fi
fi

# --- Generate Systemd Unit File ---
echo ">> Writing /etc/systemd/system/amadeus-runner.service..."
{
cat << 'EOF'
# systemd unit for the Cronomicon runner agent.
[Unit]
Description=Cronomicon runner agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=amadeus-runner
Group=amadeus-runner

# Load runner config variables
EnvironmentFile=-/etc/amadeus-runner/runner.env
# Optional Vault Agent sidecar (deploy/vault-agent/): rendered secrets, if any.
# The "-" makes this a no-op when the sidecar is not installed.
EnvironmentFile=-/etc/amadeus-runner/secrets.env
Environment=CRONOMICON_RUNNER_IDENTITY_FILE=/var/lib/amadeus-runner/identity.json
WorkingDirectory=/var/lib/amadeus-runner

# Preflight self-check. Non-fatal ('-'): logs a labeled PASS/FAIL report to the
# journal right before the agent starts, so an env problem (unreachable/proxied
# server, missing token, hung PATH dir) is visible immediately instead of as a
# silent non-appearance in the UI.
ExecStartPre=-/usr/local/bin/amadeus-runner doctor --quick
ExecStart=/usr/local/bin/amadeus-runner

# Graceful drain on SIGTERM
KillSignal=SIGTERM
TimeoutStopSec=300

Restart=on-failure
RestartSec=10s

EOF

if [ "$HARDENING_OK" = "1" ]; then
cat << 'EOF'
# Sandboxing & Hardening (full — sandbox self-probe passed at install time)
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
EOF
else
cat << 'EOF'
# Sandboxing & Hardening (REDUCED — the install-time sandbox self-probe found
# that this host's systemd stalls filesystem access under the full sandbox.
# The mount-namespace/seccomp hardening is omitted so the runner can start; the
# basic privilege drop is kept.)
NoNewPrivileges=true
RestrictSUIDSGID=true
EOF
fi

cat << 'EOF'

StateDirectory=amadeus-runner
ReadWritePaths=/var/lib/amadeus-runner

[Install]
WantedBy=multi-user.target
EOF
} > /etc/systemd/system/amadeus-runner.service

chown root:root /etc/systemd/system/amadeus-runner.service
chmod 0644 /etc/systemd/system/amadeus-runner.service

# --- Systemd Reload & Start ---
echo ">> Reloading systemd and starting service..."
systemctl daemon-reload
systemctl enable amadeus-runner
systemctl restart amadeus-runner

# --- Post-install self-check ---
# Validate the environment (server reachable, token present, toolchains found)
# as the runner user BEFORE declaring success, so a broken deploy is caught here
# instead of surfacing later as a runner that never appears in the UI.
if command -v runuser >/dev/null 2>&1; then
  echo ">> Running post-install self-check (amadeus-runner doctor)..."
  if runuser -u amadeus-runner -- bash -c 'set -a; . /etc/amadeus-runner/runner.env; set +a; exec /usr/local/bin/amadeus-runner doctor'; then
    echo ">> Self-check passed."
  else
    echo "!! Self-check reported issues (above) — the runner may not register until they are resolved."
    echo "!! Re-run: sudo runuser -u amadeus-runner -- bash -c 'set -a; . /etc/amadeus-runner/runner.env; set +a; exec /usr/local/bin/amadeus-runner doctor'"
  fi
fi

echo "=========================================================="
echo " Cronomicon Runner Installed and Started Successfully!       "
echo "=========================================================="
echo "Runner Name:  ${RUNNER_NAME}"
if [ -n "$CAPABILITIES" ]; then
  echo "Capabilities: ${CAPABILITIES} (explicit override)"
else
  echo "Capabilities: auto-detect at agent startup (probed from this host's"
  echo "              toolchains; pass -c to narrow the claimed run-types)"
fi
echo "Inventory:    ${INVENTORY}"
[ -n "$KNOWN_HOSTS_LINE" ]     && echo "known_hosts:  ${KNOWN_HOSTS_DEST}"
[ -n "$KEY_DIR_LINE" ]         && echo "Key dir:      ${KEYS_DEST}"
[ -n "$KEY_MAP_LINE" ]         && echo "Key map:      ${INSTALLED_MAP}"
[ -n "$CA_CERT_LINE" ]         && echo "CA cert:      ${CA_CERT_DEST}"
[ -n "$LOCAL_INVENTORY_LINE" ] && echo "Inventory f.: ${LOCAL_INVENTORY_DEST}"
[ -n "$ALLOW_CHECKOUT_LINE" ]  && echo "Checkout:     enabled"
[ -n "$CHECKOUT_REPOS_LINE" ]  && echo "Checkout repos: ${CHECKOUT_REPOS}"
[ -n "$CHECKOUT_TOKEN_FILE_LINE" ] && echo "Checkout tok: ${CHECKOUT_TOKEN_DEST}"
[ -n "$VAULT_PASS_LINE" ]      && echo "Vault pass:   ${VAULT_PASS_DEST}"
echo "Status:       $(systemctl is-active amadeus-runner)"
echo ""

if [ -n "$GENERATE_KEY_NAME" ]; then
  echo "=========================================================="
  echo "Generated key: ${KEYS_DEST}/${GENERATE_KEY_NAME}"
  echo "Add this PUBLIC key to each target's ~/.ssh/authorized_keys"
  echo "for the login/ansible_user the runner connects as:"
  echo ""
  echo "  ${GENERATED_PUB}"
  echo ""
  echo "The private key never leaves this host. Reference it in Cronomicon"
  echo "host config by the name '${GENERATE_KEY_NAME}' (authKeyEnvVar)."
  echo "=========================================================="
  echo ""
fi

if [ -z "$KNOWN_HOSTS_LINE" ]; then
  echo "!!========================================================!!"
  echo "!! BEFORE THE FIRST SSH RUN                                !!"
  echo "!!========================================================!!"
  echo "!! No --known-hosts was given. The agent REFUSES to open   !!"
  echo "!! SSH connections without a known_hosts file (strict      !!"
  echo "!! host-key verification, no trust-on-first-use).          !!"
  echo "!!                                                         !!"
  echo "!! Before triggering any SSH run:                          !!"
  echo "!!  1. Populate ${KNOWN_HOSTS_DEST}"
  echo "!!     (ssh-keyscan the targets from a trusted vantage).   !!"
  echo "!!  2. Add to ${CONF_DIR}/runner.env:"
  echo "!!       CRONOMICON_RUNNER_KNOWN_HOSTS=${KNOWN_HOSTS_DEST}"
  if [ "$KEYS_PROVISIONED" = 0 ]; then
    echo "!!  3. Provision a private key: re-run with --generate-key   !!"
    echo "!!     NAME, or drop a key file into ${KEYS_DEST}"
    echo "!!     (CRONOMICON_RUNNER_KEY_DIR is already set for you).      !!"
  fi
  echo "!!  Then: sudo systemctl restart amadeus-runner            !!"
  echo "!!========================================================!!"
  echo ""
elif [ "$KEYS_PROVISIONED" = 0 ]; then
  echo "NOTE: no private key was provisioned. CRONOMICON_RUNNER_KEY_DIR is"
  echo "set to ${KEYS_DEST}; before the first SSH run, drop a"
  echo "key file there (named after its authKeyEnvVar) or re-run with"
  echo "--generate-key NAME, then restart the service."
  echo ""
fi

echo "To check runner status:"
echo "  sudo systemctl status amadeus-runner"
echo ""
echo "To tail the service logs:"
echo "  sudo journalctl -u amadeus-runner -f"
echo "=========================================================="

-- SU-4: pin the bastion SSH host key so the jump hop is verified (was
-- InsecureIgnoreHostKey → first-connect MITM + secret exfil). Mirrors
-- ssh_hosts.host_key (migration 070): nullable TEXT, empty ⇒ TOFU-capture on
-- first connect, non-empty ⇒ strict compare. Managed by the in-app executor.
ALTER TABLE bastions ADD COLUMN host_key TEXT;

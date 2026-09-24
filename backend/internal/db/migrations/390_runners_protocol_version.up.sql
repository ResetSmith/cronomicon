-- 390_runners_protocol_version — persist the runner↔server wire-protocol version
-- so the manifest handler can gate behavior on agent capability (M1, §3.5/§7.2).
--
-- Back-compat gating: an amadeus-mode ansible run that needs a managed inventory
-- hard-fails when the assigned runner speaks protocol < 2 (it would otherwise
-- silently ignore the inventory field and run unscoped). Existing rows default to
-- 1 (pre-inventory agents). The register handler persists req.ProtocolVersion.
ALTER TABLE runners ADD COLUMN protocol_version INTEGER NOT NULL DEFAULT 1;

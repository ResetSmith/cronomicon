import { useEffect, useState, type ReactNode } from "react";
import { fetchCapabilities } from "../api/client";
import { Btn, Modal } from "./ui";
import { c } from "../theme";

/**
 * A repair (2.4.0 I4, IN-7; the owner's answer to question 48).
 *
 * Changing an object's agency is no longer an agency administrator's move: an
 * agency gets another's by a copy (a scope's clone, a secret or a record made
 * again). What is left is a global administrator's repair of a placement that
 * was a mistake, sent with `repair: true` and a reason the server writes to
 * the audit log (403 for anyone else, 409 `repair_required` without the flag,
 * 422 without a reason of at most 500 characters).
 *
 * The screens offer the control to a global administrator alone
 * (`configureAppGlobal`, the server's `GlobalAdmin(configureApp)`); anyone
 * else reads REPAIR_HINT instead.
 */

export const REPAIR_REASON_MAX = 500;

/** What someone who is not a global administrator reads where a move was. */
export const REPAIR_HINT =
  "Its agency does not change: another agency gets one by a copy. A global administrator can repair a placement that was a mistake.";

export type RepairBody = { repair: true; reason: string; confirmVaultChange?: true };

/** The error a write answers when it changes an agency without `repair: true`. */
export function isRepairRequired(error: unknown): boolean {
  return !!error && typeof error === "object" && (error as { code?: unknown }).code === "repair_required";
}

/** A Vault-backed row a move would take to another Vault (2.5.1 SG6.2). */
export type VaultChange = {
  kind: string;
  id: string;
  name: string;
  fromName: string;
  fromVault: "installation" | "own";
  toName: string;
  toVault: "installation" | "own";
};

/**
 * The rows of a 409 `vault_changes`: a move of a Vault-backed secret or key to
 * an owner on another Vault, refused until it is confirmed. Null for any other
 * answer.
 */
export function vaultChangesOf(error: unknown): VaultChange[] | null {
  if (!error || typeof error !== "object" || (error as { code?: unknown }).code !== "vault_changes") return null;
  const rows = (error as { rows?: unknown }).rows;
  return Array.isArray(rows) ? (rows as VaultChange[]) : [];
}

function vaultName(agency: string, vault: VaultChange["fromVault"]): string {
  return vault === "own" ? `${agency}'s own Vault` : "the installation's Vault";
}

/**
 * useRepairAllowed answers whether the caller may repair: a global
 * administrator (configureApp on every agency). False until the capabilities
 * are read, and on an older server that omits the flag.
 */
export function useRepairAllowed(): boolean {
  const [allowed, setAllowed] = useState(false);
  useEffect(() => {
    let cancelled = false;
    void fetchCapabilities().then((caps) => {
      if (!cancelled) setAllowed(!!caps.configureAppGlobal);
    });
    return () => {
      cancelled = true;
    };
  }, []);
  return allowed;
}

/**
 * RepairDialog asks for the reason and hands `{repair: true, reason}` to
 * onConfirm. The confirm stays off until there is a reason within the limit.
 */
export function RepairDialog({
  title,
  message,
  confirmLabel,
  busy,
  error,
  vaultChanges,
  onConfirm,
  onCancel,
}: {
  title: string;
  message: ReactNode;
  confirmLabel: string;
  busy?: boolean;
  /** The server's refusal, when the repair was sent and refused. */
  error?: string;
  /**
   * The rows of a 409 `vault_changes` (vaultChangesOf): the dialog says which
   * Vault each would be read from, and the confirm, "Move anyway", sends the
   * repair again with `confirmVaultChange`.
   */
  vaultChanges?: VaultChange[] | null;
  onConfirm: (repair: RepairBody) => void;
  onCancel: () => void;
}) {
  const [reason, setReason] = useState("");
  const trimmed = reason.trim();
  const tooLong = [...trimmed].length > REPAIR_REASON_MAX;
  const ready = trimmed !== "" && !tooLong;
  const confirmVault = !!vaultChanges && vaultChanges.length > 0;
  return (
    <Modal
      title={title}
      onClose={onCancel}
      footer={
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <Btn onClick={onCancel} disabled={busy}>
            Cancel
          </Btn>
          <Btn
            danger
            onClick={() => onConfirm(confirmVault ? { repair: true, reason: trimmed, confirmVaultChange: true } : { repair: true, reason: trimmed })}
            disabled={busy || !ready}
          >
            {busy ? "Working…" : confirmVault ? "Move anyway" : confirmLabel}
          </Btn>
        </div>
      }
    >
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
        {message}
        <p style={{ margin: "10px 0 6px" }}>
          This is a repair: it changes the object's agency, and the reason is written to the audit log.
        </p>
        <label style={{ display: "block", fontSize: c.fontXs, color: c.textSec }}>
          Reason
          <textarea
            aria-label="Reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={3}
            placeholder="Why the placement was a mistake"
            style={{
              display: "block",
              width: "100%",
              marginTop: 4,
              padding: 8,
              boxSizing: "border-box",
              borderRadius: c.radiusChip,
              border: `1px solid ${tooLong ? c.danger : c.border}`,
              background: c.panelInput,
              color: c.text,
              fontFamily: c.sans,
              fontSize: c.fontSm,
            }}
          />
        </label>
        <div style={{ fontSize: c.fontXs, color: tooLong ? c.danger : c.textMuted, marginTop: 4 }}>
          {[...trimmed].length} / {REPAIR_REASON_MAX}
        </div>
        {error && (
          <div role="alert" style={{ marginTop: 8, color: c.danger, fontSize: c.fontXs }}>
            {error}
          </div>
        )}
        {confirmVault && (
          <div
            role="alert"
            style={{
              marginTop: 10,
              padding: "8px 12px",
              background: c.warningBg,
              border: `1px solid ${c.warning}55`,
              borderRadius: c.radiusSurface,
              color: c.text,
              fontSize: c.fontXs,
            }}
          >
            <strong>It changes the Vault its path is read from.</strong> The same path in the other Vault may hold another value, or
            none.
            <ul style={{ margin: "6px 0 0", paddingLeft: 18 }}>
              {vaultChanges!.map((v) => (
                <li key={`${v.kind}-${v.id}`}>
                  {v.name}: from {vaultName(v.fromName, v.fromVault)} to {vaultName(v.toName, v.toVault)}
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </Modal>
  );
}

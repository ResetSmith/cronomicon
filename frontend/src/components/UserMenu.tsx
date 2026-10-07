import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { c } from "../theme";
import { api, csrfHeader, errMsg, fetchCapabilities, logout } from "../api/client";
import { fetchMyAccess, type MyAccess } from "../api/access";
import { ConfirmDialog, InlineLoading, Modal } from "./ui";

// The user menu (LR-86, LR-87; v2.3.0) — the sidebar footer's identity block,
// made into a menu on the HelpMenu pattern.
//
// It answers the first question a person in a department has, without asking a
// global administrator: "what am I allowed to do here, and why not that?".
// GET /me/access reports the caller's own groups and, for each access grant
// they resolve to, the role, the agency and the permissions it carries. Grants
// are resolved per request since 2.3.0, so what it shows is what is in force.

const PERMISSION_LABELS: Record<string, string> = {
  triggerJobs: "Run jobs",
  killJobs: "Stop and pause",
  manageEnvVars: "Manage variables & secrets",
  publishSchedule: "Publish schedules",
  configureApp: "Configure",
  manageRoles: "Manage access",
  compose: "Create jobs & workflows",
};

/**
 * accessChip is the one-line answer to "who am I here": "Global admin" for a
 * grant on every agency that can configure, otherwise the role and its agency,
 * or a count when there are several.
 */
export function accessChip(access: MyAccess | null): string {
  const grants = access?.grants ?? [];
  if (grants.length === 0) return access ? "No access" : "";
  const global = grants.find((g) => g.allScopes && g.permissions.includes("configureApp"));
  if (global) return "Global admin";
  if (grants.length === 1) {
    const g = grants[0];
    return `${g.role} · ${g.allScopes ? "every agency" : g.agencyName || g.agencyId || "—"}`;
  }
  return `${grants.length} grants`;
}

export function UserMenu({
  email,
  collapsed,
  icon,
}: {
  email?: string;
  collapsed: boolean;
  /** The trigger's icon when the rail is collapsed. */
  icon: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [access, setAccess] = useState<MyAccess | null>(null);
  const [canRevoke, setCanRevoke] = useState(false);
  const [showAccess, setShowAccess] = useState(false);
  const [confirmRevoke, setConfirmRevoke] = useState(false);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    let cancelled = false;
    void fetchMyAccess().then((a) => !cancelled && setAccess(a));
    void fetchCapabilities().then((caps) => !cancelled && setCanRevoke(!!caps.manageRolesGlobal));
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open]);

  const chip = accessChip(access);
  const item = (label: string, onClick: () => void, extra?: { danger?: boolean; title?: string }) => (
    <button
      type="button"
      title={extra?.title}
      onClick={() => {
        setOpen(false);
        onClick();
      }}
      style={{
        display: "block",
        width: "100%",
        textAlign: "left",
        padding: "7px 10px",
        borderRadius: c.radiusChip,
        border: "none",
        background: "transparent",
        color: extra?.danger ? c.danger : c.text,
        fontSize: c.fontSm,
        cursor: "pointer",
      }}
      onMouseEnter={(e) => {
        e.currentTarget.style.background = c.panelHover;
      }}
      onMouseLeave={(e) => {
        e.currentTarget.style.background = "transparent";
      }}
    >
      {label}
    </button>
  );

  const revoke = async () => {
    setBusy(true);
    const { error } = await api.POST("/auth/sessions/revoke", { params: { header: csrfHeader } });
    setBusy(false);
    setConfirmRevoke(false);
    // True in either sign-in mode: with a sign-on proxy that asserts the groups
    // on every request there are no sessions of this kind to end, and the
    // proxy's own are not Cronomicon's to end.
    setNote(
      error
        ? `Could not sign everyone out: ${errMsg(error)}`
        : "Every other Cronomicon session has been signed out. If sign-in goes through a proxy that asserts groups on every request, access already follows the proxy and there was nothing here to end.",
    );
  };

  return (
    <div ref={ref} style={{ position: "relative", alignSelf: collapsed ? "center" : "stretch" }}>
      <button
        ref={triggerRef}
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-haspopup="true"
        aria-label={collapsed ? `Account${email ? ` (${email})` : ""}` : undefined}
        title={collapsed ? `Account${email ? ` (${email})` : ""}` : "Your access, and sign out"}
        style={{
          display: "flex",
          flexDirection: "column",
          alignItems: collapsed ? "center" : "flex-start",
          gap: 2,
          width: collapsed ? undefined : "100%",
          background: "transparent",
          color: c.sidebarText,
          border: `1px solid ${c.border}`,
          borderRadius: c.radiusChip,
          padding: collapsed ? "6px" : "6px 10px",
          fontSize: c.fontXs,
          cursor: "pointer",
          textAlign: "left",
          overflow: "hidden",
        }}
      >
        {collapsed ? (
          icon
        ) : (
          <>
            <span style={{ maxWidth: "100%", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{email ?? "Account"}</span>
            {chip && <span style={{ opacity: 0.75 }}>{chip}</span>}
          </>
        )}
      </button>
      {open && (
        <div
          role="dialog"
          aria-label="Account"
          style={{
            position: "absolute",
            bottom: "100%",
            left: 0,
            zIndex: 30,
            marginBottom: 4,
            minWidth: 220,
            background: c.panel,
            // A floating overlay is the documented exception to "a surface gets
            // a border OR a shadow, never both".
            border: `1px solid ${c.border}`,
            borderRadius: c.radiusSurface,
            boxShadow: c.shadow,
            padding: 8,
          }}
        >
          <div style={{ padding: "6px 10px 8px", borderBottom: `1px solid ${c.borderLight}`, marginBottom: 4 }}>
            <div style={{ fontSize: c.fontSm, color: c.text, fontWeight: 600, overflowWrap: "anywhere" }}>{email ?? "Signed in"}</div>
            {chip && <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 2 }}>{chip}</div>}
          </div>
          {item("My access…", () => setShowAccess(true))}
          {canRevoke &&
            item("Sign out everyone else…", () => setConfirmRevoke(true), {
              title: "End every other signed-in session. Yours stays.",
            })}
          {item("Sign out", () => void logout())}
        </div>
      )}
      {note && (
        <Modal title="Sessions" onClose={() => setNote(null)}>
          <div style={{ fontSize: c.fontSm, color: c.text }}>{note}</div>
        </Modal>
      )}
      {confirmRevoke && (
        <ConfirmDialog
          title="Sign out everyone else?"
          message={
            <>
              Every other signed-in session ends now; yours stays. No change to roles, grants or scopes needs this — those reach a
              live session on its next request. It is for the one change Cronomicon cannot see: a person removed from a group at
              your identity provider keeps that group until they sign in again.
            </>
          }
          confirmLabel="Sign out everyone else"
          busy={busy}
          onCancel={() => setConfirmRevoke(false)}
          onConfirm={revoke}
        />
      )}
      {showAccess && <MyAccessDialog onClose={() => setShowAccess(false)} />}
    </div>
  );
}

/** MyAccessDialog — the caller's groups and the grants they resolve to (LR-87). Read fresh each time it opens. */
export function MyAccessDialog({ onClose }: { onClose: () => void }) {
  const [access, setAccess] = useState<MyAccess | null | undefined>(undefined);
  useEffect(() => {
    let cancelled = false;
    void fetchMyAccess().then((a) => !cancelled && setAccess(a));
    return () => {
      cancelled = true;
    };
  }, []);

  const th = { textAlign: "left" as const, padding: "6px 8px", fontFamily: c.sansCond, textTransform: "uppercase" as const, letterSpacing: 0.5, fontSize: c.fontXs, color: c.textSec, borderBottom: `1px solid ${c.border}` };
  const td = { padding: "8px", fontSize: c.fontSm, color: c.text, borderBottom: `1px solid ${c.borderLight}`, verticalAlign: "top" as const };

  return (
    <Modal title="My access" wide onClose={onClose}>
      {access === undefined && <InlineLoading what="your access" />}
      {access === null && <div style={{ fontSize: c.fontSm, color: c.danger }}>Your access could not be read. Reload the page and try again.</div>}
      {access && (
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
            Signed in as <strong style={{ color: c.text }}>{access.email}</strong>. What you may do comes from access grants: a group
            you are in, given a role, on one agency or on every agency. A permission and the agency it applies to always come from
            the same grant.
          </div>
          {access.grants.length === 0 ? (
            <div style={{ fontSize: c.fontSm, color: c.text }}>
              None of your groups is named by an access grant, so you can see what belongs to Global and change nothing. Ask an
              administrator to grant one of your groups a role.
            </div>
          ) : (
            <table style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr>
                  <th style={th}>Role</th>
                  <th style={th}>On</th>
                  <th style={th}>May</th>
                  <th style={th}>Through</th>
                </tr>
              </thead>
              <tbody>
                {access.grants.map((g, i) => (
                  <tr key={i}>
                    <td style={{ ...td, fontWeight: 600 }}>{g.role}</td>
                    <td style={td}>
                      {g.allScopes ? (
                        "Every agency"
                      ) : (
                        <>
                          {g.agencyName || g.agencyId}
                          <div style={{ fontSize: c.fontXs, color: g.scopes.length === 0 ? c.warning : c.textSec, marginTop: 2 }}>
                            {g.scopes.length === 0
                              ? "This agency has no scopes yet, so the grant reaches nothing."
                              : `${g.scopes.length} scope${g.scopes.length === 1 ? "" : "s"}: ${g.scopes.join(", ")}`}
                          </div>
                        </>
                      )}
                    </td>
                    <td style={td}>
                      {g.permissions.length === 0 ? "View only" : g.permissions.map((p) => PERMISSION_LABELS[p] ?? p).join(", ")}
                    </td>
                    <td style={{ ...td, color: c.textSec }}>
                      {g.origin === "dev" ? "the developer login" : g.origin === "bootstrap" ? `${g.groups.join(", ")} (bootstrap administrator)` : g.groups.join(", ")}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <div>
            <div style={{ fontFamily: c.sansCond, textTransform: "uppercase", letterSpacing: 0.5, fontSize: c.fontXs, color: c.textSec, marginBottom: 4 }}>
              Your groups ({access.groups.length})
            </div>
            <div style={{ fontSize: c.fontSm, color: c.text, overflowWrap: "anywhere" }}>{access.groups.length ? access.groups.join(", ") : "None."}</div>
            {access.unmatchedGroups.length > 0 && (
              <div style={{ fontSize: c.fontXs, color: c.textSec, marginTop: 6, lineHeight: 1.5 }}>
                Not named by any access grant: <span style={{ color: c.text }}>{access.unmatchedGroups.join(", ")}</span>. A grant's
                group name must match exactly, letter case included: that is the usual reason for "I was added to the group and
                nothing changed".
              </div>
            )}
          </div>
        </div>
      )}
    </Modal>
  );
}

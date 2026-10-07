import { useEffect, useState } from "react";
import { c } from "../theme";
import { AlertBanner, InlineLoading } from "../components/ui";
import { fetchCapabilities, type Capabilities } from "../api/client";
import { GLOBAL_ADMIN_ONLY } from "../api/globalAdmin";
import { GeneralSection } from "./settings/General";
import { NotificationsSection } from "./settings/Notifications";
import { SshTargetsSection } from "./settings/SshTargets";
import { UsersAccessSection } from "./settings/UsersAccess";
import { AuditComplianceSection } from "./settings/AuditCompliance";
import { ServiceAccountsSection } from "./settings/ServiceAccounts";
import { RecycleBinSection } from "./settings/RecycleBin";
import { GitlabSection, VaultSection, LogStorageSection, ObservabilitySection } from "./settings/Integrations";
import { TimezoneAlert } from "./settings/TimezoneAlert";

// Settings — left-rail sections mirroring the prototype (cronomicon-settings.jsx).
// Runners has its own top-level view and is intentionally not duplicated here.
// `requires` is the /capabilities permission the backend enforces for that
// section (PP-B1); sections the caller can't manage are hidden so the UI matches
// what the server will allow instead of rendering controls that 403.
//
// GC (v2.2.2) — `requires` is still the FLAT flag ("holds this permission
// somewhere") and still decides whether the section is listed. `global` is the
// flag the section's WRITES now need: one grant covering every agency. A caller
// with the first and not the second is an administrator of one agency, who keeps
// the section — disabled, with the reason — rather than losing it (FX-7: a
// precondition disables with an explanation; only irrelevance hides).
// `readOnlyNote` marks the sections that are read-only as a whole for such a
// caller, so the reason is stated once above the card; the others (SSH Targets,
// Users & Access, Service Accounts) keep agency-level work and explain
// themselves control by control, and GitLab, Vault and the Recycle Bin — whose
// READ is refused too — render their own explanation in place of the data.
const SECTIONS = [
  { group: "General", key: "general", label: "General", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: true },
  { group: "General", key: "notifications", label: "Notifications", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: true },
  { group: "Integrations", key: "gitlab", label: "GitLab Connection", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: false },
  { group: "Integrations", key: "vault", label: "Vault", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: false },
  { group: "Integrations", key: "observability", label: "Observability", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: true },
  { group: "Execution", key: "targets", label: "SSH Targets", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: false },
  { group: "Execution", key: "logstorage", label: "Log Storage", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: true },
  { group: "Access & Security", key: "users", label: "Users & Access", requires: "manageRoles", global: "manageRolesGlobal", readOnlyNote: false },
  // ET-C: minting one IS granting a role, so it sits behind manageRoles rather
  // than configureApp — the same permission that edits access grants.
  { group: "Access & Security", key: "serviceaccounts", label: "Service Accounts", requires: "manageRoles", global: "manageRolesGlobal", readOnlyNote: false },
  { group: "Access & Security", key: "audit", label: "Audit & Compliance", requires: "configureApp", global: "configureAppGlobal", readOnlyNote: true },
  // RH: cross-kind, and its purge window lives next door in Audit & Compliance.
  { group: "Access & Security", key: "recyclebin", label: "Recycle Bin", requires: "configureApp", global: "composeAdmin", readOnlyNote: false },
] as const;

type SectionKey = (typeof SECTIONS)[number]["key"];
type CapKey = (typeof SECTIONS)[number]["requires"];
type GlobalKey = (typeof SECTIONS)[number]["global"];

export function Settings() {
  const [caps, setCaps] = useState<Capabilities | null>(null);
  const [section, setSection] = useState<SectionKey | null>(null);

  useEffect(() => {
    fetchCapabilities().then((cp) => {
      setCaps(cp);
      const first = SECTIONS.find((s) => cp[s.requires as CapKey]);
      setSection(first ? first.key : null);
    });
  }, []);

  if (caps == null) {
    return <InlineLoading style={{ padding: 16 }} />;
  }

  const visible = SECTIONS.filter((s) => caps[s.requires as CapKey]);
  if (visible.length === 0) {
    return (
      <div style={{ padding: 24, color: c.textSec, fontSize: c.fontBody }}>
        You don't have permission to manage any settings. Settings require the
        <strong> Admin</strong> role.
      </div>
    );
  }

  // The active section's global flag — `undefined` (an older server) is false.
  const current = SECTIONS.find((s) => s.key === section);
  const isGlobal = current ? !!caps[current.global as GlobalKey] : false;

  return (
    <div>
      <TimezoneAlert />
      <div style={{ display: "grid", gridTemplateColumns: "210px 1fr", gap: 20, alignItems: "start" }}>
        <div>
          {visible.map((s, i) => {
            const isFirstInGroup = i === 0 || visible[i - 1].group !== s.group;
            const active = section === s.key;
            return (
              <div key={s.key}>
                {isFirstInGroup && (
                  <div
                    style={{
                      padding: i === 0 ? "4px 14px 6px" : "14px 14px 6px",
                      fontSize: c.fontXs,
                      fontFamily: c.sansCond,
                      fontWeight: 700,
                      color: c.textSec,
                      textTransform: "uppercase",
                      letterSpacing: 0.7,
                      opacity: 0.7,
                      marginTop: i === 0 ? 0 : 4,
                      borderTop: i === 0 ? "none" : `1px solid ${c.border}`,
                    }}
                  >
                    {s.group}
                  </div>
                )}
                <div
                  onClick={() => setSection(s.key)}
                  style={{
                    padding: "9px 14px",
                    borderRadius: c.radiusChip,
                    fontSize: c.fontSm,
                    cursor: "pointer",
                    userSelect: "none",
                    background: active ? `${c.primary}1a` : "transparent",
                    color: active ? c.primary : c.textSec,
                    fontWeight: active ? 600 : 400,
                    marginBottom: 1,
                  }}
                >
                  {s.label}
                </div>
              </div>
            );
          })}
        </div>
        <div>
          {current?.readOnlyNote && !isGlobal && <AlertBanner type="info">Read-only. {GLOBAL_ADMIN_ONLY}</AlertBanner>}
          {section === "general" && <GeneralSection canWrite={isGlobal} />}
          {section === "notifications" && <NotificationsSection canWrite={isGlobal} />}
          {section === "gitlab" && <GitlabSection canWrite={isGlobal} />}
          {section === "vault" && <VaultSection canWrite={isGlobal} />}
          {section === "observability" && <ObservabilitySection canWrite={isGlobal} />}
          {section === "targets" && <SshTargetsSection canWrite={isGlobal} />}
          {section === "logstorage" && <LogStorageSection canWrite={isGlobal} />}
          {section === "users" && <UsersAccessSection />}
          {section === "serviceaccounts" && <ServiceAccountsSection canGrantEverywhere={isGlobal} />}
          {section === "audit" && <AuditComplianceSection canWrite={isGlobal} />}
          {section === "recyclebin" && <RecycleBinSection canWrite={isGlobal} />}
        </div>
      </div>
    </div>
  );
}

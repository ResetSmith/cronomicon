import { useEffect, useState } from "react";
import { c } from "../theme";
import { InlineLoading } from "../components/ui";
import { fetchCapabilities, type Capabilities } from "../api/client";
import { GeneralSection } from "./settings/General";
import { NotificationsSection } from "./settings/Notifications";
import { SshTargetsSection } from "./settings/SshTargets";
import { UsersAccessSection } from "./settings/UsersAccess";
import { AuditComplianceSection } from "./settings/AuditCompliance";
import { ServiceAccountsSection } from "./settings/ServiceAccounts";
import { RecycleBinSection } from "./settings/RecycleBin";
import { GitlabSection, VaultSection, LogStorageSection, ObservabilitySection } from "./settings/Integrations";
import { TimezoneAlert } from "./settings/TimezoneAlert";

// Settings — left-rail sections mirroring the prototype (amadeus-settings.jsx).
// Runners has its own top-level view and is intentionally not duplicated here.
// `requires` is the /capabilities permission the backend enforces for that
// section (PP-B1); sections the caller can't manage are hidden so the UI matches
// what the server will allow instead of rendering controls that 403.
const SECTIONS = [
  { group: "General", key: "general", label: "General", requires: "configureApp" },
  { group: "General", key: "notifications", label: "Notifications", requires: "configureApp" },
  { group: "Integrations", key: "gitlab", label: "GitLab Connection", requires: "configureApp" },
  { group: "Integrations", key: "vault", label: "Vault", requires: "configureApp" },
  { group: "Integrations", key: "observability", label: "Observability", requires: "configureApp" },
  { group: "Execution", key: "targets", label: "SSH Targets", requires: "configureApp" },
  { group: "Execution", key: "logstorage", label: "Log Storage", requires: "configureApp" },
  { group: "Access & Security", key: "users", label: "Users & Access", requires: "manageRoles" },
  // ET-C: minting one IS granting a role, so it sits behind manageRoles rather
  // than configureApp — the same permission that edits access grants.
  { group: "Access & Security", key: "serviceaccounts", label: "Service Accounts", requires: "manageRoles" },
  { group: "Access & Security", key: "audit", label: "Audit & Compliance", requires: "configureApp" },
  // RH: cross-kind, and its purge window lives next door in Audit & Compliance.
  { group: "Access & Security", key: "recyclebin", label: "Recycle Bin", requires: "configureApp" },
] as const;

type SectionKey = (typeof SECTIONS)[number]["key"];
type CapKey = (typeof SECTIONS)[number]["requires"];

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
          {section === "general" && <GeneralSection />}
          {section === "notifications" && <NotificationsSection />}
          {section === "gitlab" && <GitlabSection />}
          {section === "vault" && <VaultSection />}
          {section === "observability" && <ObservabilitySection />}
          {section === "targets" && <SshTargetsSection />}
          {section === "logstorage" && <LogStorageSection />}
          {section === "users" && <UsersAccessSection />}
          {section === "serviceaccounts" && <ServiceAccountsSection />}
          {section === "audit" && <AuditComplianceSection />}
          {section === "recyclebin" && <RecycleBinSection />}
        </div>
      </div>
    </div>
  );
}

import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { c } from "../theme";
import { InlineLoading } from "../components/ui";
import { accessChip } from "../components/UserMenu";
import { fetchCapabilities, type Capabilities } from "../api/client";
import { useMyAccess } from "../api/access";
import { GeneralSection } from "./settings/General";
import { NotificationsSection } from "./settings/Notifications";
import { SshTargetsSection } from "./settings/SshTargets";
import { UsersAccessSection } from "./settings/UsersAccess";
import { AuditComplianceSection } from "./settings/AuditCompliance";
import { ServiceAccountsSection } from "./settings/ServiceAccounts";
import { RecycleBinSection } from "./settings/RecycleBin";
import { GitlabSection, VaultSection, LogStorageSection, ObservabilitySection } from "./settings/Integrations";
import { LocalRunnerSection } from "./settings/LocalRunner";
import { TimezoneAlert } from "./settings/TimezoneAlert";

// Settings — two groups since v2.3.0 (LR-86).
//
//   INSTALLATION — what only a global administrator changes: General, the
//   notification relay, the GitLab and Vault connections, Observability, Log
//   Storage, Audit & Compliance. It is not shown to anyone else. Until 2.3.0 an
//   administrator of one agency saw every one of these read-only, with a note;
//   a whole section that can never apply to its reader is irrelevance, which
//   the house rule (FX-7) says to hide. A single control with an unmet
//   precondition is still disabled with its reason.
//
//   AGENCY — what an agency's administrators change for their own agency:
//   access grants and roles, service accounts, SSH targets, and the recycle
//   bin. `requires` is the flat /capabilities permission the section needs
//   ("holds it somewhere"); `global` is the flag its install-wide parts ask
//   for, and each section explains those control by control.
//
// Sections are addressable: /settings?tab=<key>. An address the caller may not
// open falls back to their first section.
//
// (The agency catalog is on Scopes → Agencies, and Runners has its own view;
// neither is duplicated here.)
const SECTIONS = [
  { group: "Installation", key: "general", label: "General", requires: "configureAppGlobal", global: "configureAppGlobal" },
  { group: "Installation", key: "notifications", label: "Notifications", requires: "configureAppGlobal", global: "configureAppGlobal" },
  { group: "Installation", key: "gitlab", label: "GitLab Connection", requires: "configureAppGlobal", global: "configureAppGlobal" },
  { group: "Installation", key: "vault", label: "Vault", requires: "configureAppGlobal", global: "configureAppGlobal" },
  { group: "Installation", key: "observability", label: "Observability", requires: "configureAppGlobal", global: "configureAppGlobal" },
  { group: "Installation", key: "logstorage", label: "Log Storage", requires: "configureAppGlobal", global: "configureAppGlobal" },
  { group: "Installation", key: "audit", label: "Audit & Compliance", requires: "configureAppGlobal", global: "configureAppGlobal" },
  // This server running shell jobs itself: its switch, its concurrency and the
  // agencies it serves (LR-1, LR-43).
  { group: "Installation", key: "localrunner", label: "Local runner", requires: "configureAppGlobal", global: "configureAppGlobal" },
  { group: "Agency", key: "users", label: "Users & Access", requires: "manageRoles", global: "manageRolesGlobal" },
  // ET-C: minting one IS granting a role, so it sits behind manageRoles rather
  // than configureApp — the same permission that edits access grants.
  { group: "Agency", key: "serviceaccounts", label: "Service Accounts", requires: "manageRoles", global: "manageRolesGlobal" },
  { group: "Agency", key: "targets", label: "SSH Targets", requires: "configureApp", global: "configureAppGlobal" },
  // RH: cross-kind. Still a global administrator's to use (`composeAdmin`); it
  // says so in place of its list for anyone else.
  { group: "Agency", key: "recyclebin", label: "Recycle Bin", requires: "configureApp", global: "composeAdmin" },
] as const;

type SectionKey = (typeof SECTIONS)[number]["key"];
type CapKey = (typeof SECTIONS)[number]["requires"];
type GlobalKey = (typeof SECTIONS)[number]["global"];

export function Settings() {
  const [caps, setCaps] = useState<Capabilities | null>(null);
  const [params, setParams] = useSearchParams();
  const access = useMyAccess();

  useEffect(() => {
    fetchCapabilities().then(setCaps);
  }, []);

  if (caps == null) {
    return <InlineLoading style={{ padding: 16 }} />;
  }

  const visible = SECTIONS.filter((s) => caps[s.requires as CapKey]);
  if (visible.length === 0) {
    return (
      <div style={{ padding: 24, color: c.textSec, fontSize: c.fontBody }}>
        You don't administer anything here. Settings are for an agency's administrators and for global administrators; open{" "}
        <strong>My access</strong> from your name in the sidebar to see what your groups grant you.
      </div>
    );
  }

  // The section the address names, when the caller may open it; else their first.
  const wanted = params.get("tab");
  const current = visible.find((s) => s.key === wanted) ?? visible[0];
  const section: SectionKey = current.key;
  const setSection = (key: SectionKey) => {
    const next = new URLSearchParams(params);
    next.set("tab", key);
    setParams(next, { replace: true });
  };
  // The active section's global flag — `undefined` (an older server) is false.
  const isGlobal = !!caps[current.global as GlobalKey];
  const chip = accessChip(access ?? null);

  return (
    <div>
      <TimezoneAlert />
      <div style={{ display: "grid", gridTemplateColumns: "210px 1fr", gap: 20, alignItems: "start" }}>
        <div>
          {/* Who the reader is here (LR-86): it decides which of the two groups
              below exist for them. */}
          {chip && (
            <div
              title="Your access. Open My access from your name in the sidebar for the detail."
              style={{ margin: "0 14px 10px", display: "inline-flex", padding: "2px 8px", borderRadius: c.radiusChip, border: `1px solid ${c.border}`, fontSize: c.fontXs, fontWeight: 600, color: c.textSec }}
            >
              {chip}
            </div>
          )}
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
          {section === "localrunner" && <LocalRunnerSection canWrite={isGlobal} />}
          {section === "recyclebin" && <RecycleBinSection canWrite={isGlobal} />}
        </div>
      </div>
    </div>
  );
}

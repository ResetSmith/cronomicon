import { useEffect, useState } from "react";
import { AGENCY_REFRESH_EVENT, api, csrfHeader, errMsg, fetchCapabilities } from "../../api/client";
import { GLOBAL_AGENCY } from "../../api/access";
import { grantAgencyAdmins } from "../../api/grants";
import { useAgencyOptional } from "../../agency";
import { useGet, rows } from "../../hooks";
import { Btn, Card } from "../../components/ui";
import { c } from "../../theme";
import { AgencyFormModal } from "../scopes/AgenciesTab";

// The first agency (2.5.3 CA-16). Central runs nothing, so a new installation
// starts with one working agency, Operations, made at first start (question
// 213: an upgrade that held only Central gets it too). A solo administrator or
// small team names it and works there; the card below offers that, and the
// agency's administrators' AD group (question 210), until it has a name of its
// own or a grant names it. With every agency deleted, the All-agencies
// Dashboard offers to create one.

/** The name and maker the first start gives its agency (settings.EnsureFirstAgency). */
export const FIRST_AGENCY_NAME = "Operations";
const FIRST_AGENCY_ACTOR = "system (first start)";

type AgencyRow = { id: string; name: string; description?: string | null; createdBy?: string };
type Grant = { agencyId?: string };

const dismissKey = (id: string) => `cronomicon.nameYourAgency.dismissed.${id}`;
function isDismissed(id: string): boolean {
  try {
    return window.localStorage.getItem(dismissKey(id)) === "1";
  } catch {
    return false;
  }
}

function useGlobalAdmin(): boolean {
  const [yes, setYes] = useState(false);
  useEffect(() => {
    let cancelled = false;
    void fetchCapabilities().then((caps) => {
      if (!cancelled) setYes(!!caps.configureAppGlobal && !!caps.manageRolesGlobal);
    });
    return () => {
      cancelled = true;
    };
  }, []);
  return yes;
}

const field = (): React.CSSProperties => ({
  background: c.panelInput,
  color: c.text,
  border: `1px solid ${c.borderStrong}`,
  borderRadius: c.radiusChip,
  padding: "6px 8px",
  fontSize: c.fontSm,
  width: 240,
});

/** NameYourAgencyCard is on the first agency's Dashboard while it is as the first start made it. */
export function NameYourAgencyCard() {
  const ag = useAgencyOptional();
  if (!ag || ag.onOverview || ag.agency === GLOBAL_AGENCY) return null;
  return <NameYourAgency agency={ag.agency} />;
}

function NameYourAgency({ agency }: { agency: string }) {
  const admin = useGlobalAdmin();
  const [dep, setDep] = useState(0);
  const none = () => Promise.resolve({ data: [] });
  const agencyQ = useGet<unknown>(() => (admin ? api.GET("/agencies") : none()), [admin, dep]);
  const grantsQ = useGet<unknown>(() => (admin ? api.GET("/access-grants") : none()), [admin, dep]);
  const [dismissed, setDismissed] = useState(() => isDismissed(agency));
  const [name, setName] = useState(FIRST_AGENCY_NAME);
  const [group, setGroup] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);

  const me = rows<AgencyRow>(agencyQ.data).find((a) => a.id === agency);
  const granted = rows<Grant>(grantsQ.data).some((g) => g.agencyId === agency);
  const first = !!me && me.name === FIRST_AGENCY_NAME && me.createdBy === FIRST_AGENCY_ACTOR;
  if (done) {
    return (
      <Card title="Your agency">
        <div role="status" style={{ fontSize: c.fontSm, color: c.success }}>{done}</div>
      </Card>
    );
  }
  if (!admin || dismissed || !first || grantsQ.loading || granted) return null;

  const save = async () => {
    if (!me) return;
    if (!name.trim()) {
      setErr("Give the agency a name.");
      return;
    }
    setBusy(true);
    setErr(null);
    const said: string[] = [];
    if (name.trim() !== me.name) {
      const { error } = await api.PUT("/agencies/{agencyId}", {
        params: { path: { agencyId: agency }, header: csrfHeader },
        body: { name: name.trim(), description: me.description ?? null },
      });
      if (error) {
        setBusy(false);
        setErr(errMsg(error));
        return;
      }
      said.push(`It is called ${name.trim()} now.`);
      window.dispatchEvent(new Event(AGENCY_REFRESH_EVENT));
    }
    if (group.trim()) {
      const refused = await grantAgencyAdmins(agency, group);
      if (refused) {
        setBusy(false);
        setErr(`${said.join(" ")} The administrators' group was not added: ${refused}`.trim());
        setDep((n) => n + 1);
        return;
      }
      said.push(`${group.trim()} administers it.`);
    }
    setBusy(false);
    setDone(said.length > 0 ? `${said.join(" ")} Change either in Scopes → Agencies and Settings → Users & Access.` : "Kept as it is.");
  };
  const dismiss = () => {
    try {
      window.localStorage.setItem(dismissKey(agency), "1");
    } catch {
      // Browser storage refused (a private window): hidden for this visit only.
    }
    setDismissed(true);
  };

  return (
    <Card title="Name your agency" action={<Btn small onClick={dismiss}>Dismiss</Btn>}>
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, marginBottom: 10 }}>
        This installation's work happens here, in <strong style={{ color: c.text }}>{me.name}</strong>, made at its first
        start. Give it your team's name, and optionally the AD group whose members administer it. A second agency later is
        made under Scopes → Agencies; nothing here moves.
      </div>
      <div style={{ display: "flex", gap: 12, flexWrap: "wrap", alignItems: "flex-end" }}>
        <label style={{ fontSize: c.fontSm, color: c.textMuted }}>
          Name
          <input aria-label="Agency name" value={name} onChange={(e) => setName(e.target.value)} style={{ ...field(), display: "block", marginTop: 4 }} />
        </label>
        <label style={{ fontSize: c.fontSm, color: c.textMuted }}>
          Administrators' AD group (optional)
          <input
            aria-label="Administrators' AD group"
            value={group}
            onChange={(e) => setGroup(e.target.value)}
            placeholder="e.g. ops-admins"
            style={{ ...field(), display: "block", marginTop: 4 }}
          />
        </label>
        <Btn primary onClick={save} disabled={busy}>
          {busy ? "Saving…" : "Save"}
        </Btn>
      </div>
      {err && (
        <div role="alert" style={{ marginTop: 8, fontSize: c.fontSm, color: c.danger }}>
          {err}
        </div>
      )}
    </Card>
  );
}

/** CreateFirstAgencyCard is on the All-agencies Dashboard while no agency exists. */
export function CreateFirstAgencyCard({ onCreated }: { onCreated?: () => void }) {
  const ag = useAgencyOptional();
  if (!ag || !ag.onOverview) return null;
  return <CreateFirstAgency globalName={ag.globalName} onCreated={onCreated} />;
}

function CreateFirstAgency({ globalName, onCreated }: { globalName: string; onCreated?: () => void }) {
  const admin = useGlobalAdmin();
  const agencyQ = useGet<unknown>(() => (admin ? api.GET("/agencies") : Promise.resolve({ data: undefined })), [admin]);
  const [open, setOpen] = useState(false);
  const none = !!agencyQ.data && rows<AgencyRow>(agencyQ.data).every((a) => a.id === GLOBAL_AGENCY);
  if (!admin || !none) return null;
  return (
    <Card title="Create your first agency">
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6, marginBottom: 10 }}>
        {globalName} is the installation's administration and runs nothing: jobs, scripts, schedules, runners and
        variables belong to an agency. Create the agency your work happens in, with the AD group that administers it if you
        like; its repository can be connected now or later.
      </div>
      <Btn primary onClick={() => setOpen(true)}>
        Create your first agency
      </Btn>
      {open && (
        <AgencyFormModal
          initial={null}
          withAdminGroup
          onClose={() => setOpen(false)}
          onSaved={() => setOpen(false)}
          onCreated={(id: string) => {
            setOpen(false);
            onCreated?.();
            // A fresh start in it: the switcher's choices are read again with it.
            window.location.assign(`/?agency=${encodeURIComponent(id)}`);
          }}
        />
      )}
    </Card>
  );
}

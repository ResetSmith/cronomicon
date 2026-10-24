import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useAgencyOptional } from "../agency";
import { GLOBAL_AGENCY } from "../api/access";
import { api } from "../api/client";
import { useGet } from "../hooks";
import { AlertBanner } from "./ui";
import { c } from "../theme";

// 2.5.3 CA-12 — Central is the installation's administration and runs nothing
// (CA-1): it stays in the switcher as "<name> · administration", and its work
// pages say so in one empty state, with no create actions. The change log,
// Activity, Notices and Settings show the installation's as before.
//
// What Central still held at the upgrade (CA-17) is listed by the
// central_holds_work notice, one finding per kind, and its way out is made on
// the work page of that kind (move it to an agency, or delete it): while a kind
// is held, its page shows those rows under a banner instead of the empty state.

/** useInCentral reports whether the agency in use is Central (not on the
 * All-agencies Dashboard, and never outside the provider). */
export function useInCentral(): boolean {
  const ag = useAgencyOptional();
  return !!ag && !ag.onOverview && ag.agency === GLOBAL_AGENCY;
}

type Notice = { kind: string; subject: string; resolvedAt?: string | null };

/** useCentralHolds answers which of the kinds Central still holds (the open
 * central_holds_work findings, `central:<kind>`), and whether that is known
 * yet. Asked only in Central and only for kinds given. */
export function useCentralHolds(kinds: string[]): { held: string[]; known: boolean } {
  const inCentral = useInCentral();
  const ask = inCentral && kinds.length > 0;
  const q = useGet<Notice[]>(
    () => (ask ? api.GET("/notices") : Promise.resolve({ data: [] as Notice[], error: undefined })) as never,
    [ask, kinds.join(",")],
  );
  if (!ask) return { held: [], known: true };
  const open = Array.isArray(q.data) ? q.data : [];
  const held = kinds.filter((k) =>
    open.some((n) => n.kind === "central_holds_work" && n.subject === `central:${k}` && !n.resolvedAt),
  );
  return { held, known: !q.loading };
}

/** CentralRunsNothing is the empty state of a work page while in Central. */
export function CentralRunsNothing() {
  const ag = useAgencyOptional();
  const name = ag?.agencyName ?? "Central";
  // Before the first agency exists (CA-16) there is none to switch to.
  const none = !!ag && !ag.choices.some((ch) => ch.kind === "agency" && ch.id !== GLOBAL_AGENCY);
  if (none) {
    return (
      <div
        role="status"
        data-testid="central-runs-nothing"
        style={{
          padding: "48px 24px",
          textAlign: "center",
          border: `1px dashed ${c.borderStrong}`,
          borderRadius: c.radiusSurface,
          background: c.panel,
          color: c.textSec,
          fontSize: c.fontBody,
        }}
      >
        <div style={{ color: c.text, fontWeight: 600, marginBottom: 6 }}>There is no agency yet.</div>
        <div style={{ fontSize: c.fontSm }}>
          {name} runs nothing: work happens in an agency. A global administrator creates the first one on the{" "}
          <Link to={`/?agency=overview`}>All-agencies Dashboard</Link>.
        </div>
      </div>
    );
  }
  return (
    <div
      role="status"
      data-testid="central-runs-nothing"
      style={{
        padding: "48px 24px",
        textAlign: "center",
        border: `1px dashed ${c.borderStrong}`,
        borderRadius: c.radiusSurface,
        background: c.panel,
        color: c.textSec,
        fontSize: c.fontBody,
      }}
    >
      <div style={{ color: c.text, fontWeight: 600, marginBottom: 6 }}>{name} runs nothing. Switch to an agency to work.</div>
      <div style={{ fontSize: c.fontSm }}>
        {name} is the installation's administration: its notices, change log, Activity and Settings are here.
      </div>
    </div>
  );
}

/** CentralLeftovers is the banner over what Central still holds from before 2.5.3. */
export function CentralLeftovers() {
  const name = useAgencyOptional()?.agencyName ?? "Central";
  return (
    <AlertBanner type="warning">
      <span data-testid="central-leftovers">
        {name} runs nothing. What is listed here is left from before 2.5.3 and no longer runs: move each to the agency whose
        work it is, or delete it. <Link to="/notices">Notices</Link> lists each and its way out.
      </span>
    </AlertBanner>
  );
}

/** CentralGate renders a work page; in Central, the page under a banner while
 * Central still holds one of `holds`, else the empty state. */
export function CentralGate({ holds = [], children }: { holds?: string[]; children: ReactNode }) {
  const inCentral = useInCentral();
  const { held, known } = useCentralHolds(holds);
  if (!inCentral) return <>{children}</>;
  if (!known) return null;
  if (held.length === 0) return <CentralRunsNothing />;
  return (
    <>
      <CentralLeftovers />
      {children}
    </>
  );
}

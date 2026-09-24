// RX-16 — the "Reacts to / Reacted on by" panels on job and workflow detail.
//
// Both directions, and the second one is the point. "What runs when this
// finishes?" is the question a reaction makes unanswerable everywhere else: a
// workflow's coupling is visible in its step graph, but a reaction lives on the
// OTHER definition, so a job can acquire downstream consequences that nothing on
// its own page would ever show. Rendering only "reacts to" would leave the
// dangerous direction — the one that surprises people during an incident —
// invisible.
import { Fragment } from "react";
import { c } from "../../theme";
import { Rule, Section } from "../../components/ui";
import { OwnerBadge } from "./ui";
import { reactedOnBy, reactsTo, secondsLabel, type Reaction } from "./reactions";

const NO_CLOCK_NOTE =
  "A reaction has no clock — it fires when the named definition finishes, so it never appears in Upcoming or in this definition's next-run time. A newly added one starts from now: it fires on the next matching completion, never retroactively on one that already happened.";

function OutcomeText({ outcome }: { outcome?: string }) {
  const tone =
    outcome === "success" ? c.success
      : outcome === "failure" ? c.danger
        : outcome === "stopped" ? c.textSec
          : c.info;
  return <span style={{ color: tone, fontWeight: 600 }}>{outcome}</span>;
}

function Row({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8, padding: "8px 2px", fontSize: c.fontSm, flexWrap: "wrap" }}>
      {children}
    </div>
  );
}

function OffChip() {
  return (
    <span
      title="Switched off — it stays authored but observes nothing until re-enabled."
      style={{ fontSize: c.fontXs, fontWeight: 600, color: c.textMuted }}
    >
      off
    </span>
  );
}

export function ReactionPanels({
  edges,
  kind,
  source,
  name,
}: {
  edges: Reaction[];
  kind: "job" | "workflow";
  source?: string;
  name?: string;
}) {
  const up = reactsTo(edges, kind, source, name);
  const down = reactedOnBy(edges, kind, source, name);
  if (up.length === 0 && down.length === 0) return null;

  return (
    <>
      {up.length > 0 && (
        <Section title={`Reacts to (${up.length})`} info={NO_CLOCK_NOTE}>
          <div style={{ display: "flex", flexDirection: "column" }}>
            {up.map((r, i) => (
              <Fragment key={r.name ?? i}>
                {i > 0 && <Rule />}
                <Row>
                  <span style={{ color: c.textMuted }}>when</span>
                  <OwnerBadge kind={r.onKind} />
                  <span style={{ fontFamily: c.mono, fontWeight: 600, color: r.missing ? c.textMuted : c.text }}>
                    {r.onName}
                  </span>
                  <OutcomeText outcome={r.onOutcome} />
                  {!!r.delaySeconds && (
                    <span style={{ color: c.textMuted }}>· after {secondsLabel(r.delaySeconds)}</span>
                  )}
                  {!!r.minIntervalSeconds && (
                    <span style={{ color: c.textMuted }} title="At most one run per this interval; events arriving inside it are dropped, not queued.">
                      · min {secondsLabel(r.minIntervalSeconds)} apart
                    </span>
                  )}
                  {!r.enabled && <OffChip />}
                </Row>
                {r.missing && (
                  // Dangling is a supported state — the Git prune can create it
                  // with nobody to ask — so it has to be visible rather than
                  // quietly inert. A reaction that can never fire looks exactly
                  // like one that simply has not fired yet.
                  <div
                    title="The watched definition no longer exists. This reaction was kept rather than deleted, because removing it silently would lose work you authored."
                    style={{ fontSize: c.fontXs, color: c.warning, padding: "0 2px 8px" }}
                  >
                    ⚠ missing: this {r.onKind} no longer exists — the reaction can never fire
                  </div>
                )}
              </Fragment>
            ))}
          </div>
        </Section>
      )}

      {down.length > 0 && (
        <Section
          title={`Reacted on by (${down.length})`}
          info={`What this ${kind} sets off when it finishes. ${NO_CLOCK_NOTE}`}
        >
          <div style={{ display: "flex", flexDirection: "column" }}>
            {down.map((r, i) => (
              <Fragment key={`${r.ownerKind}/${r.ownerName}/${r.name}` || i}>
                {i > 0 && <Rule />}
                <Row>
                  <span style={{ color: c.textMuted }}>on</span>
                  <OutcomeText outcome={r.onOutcome} />
                  <span style={{ color: c.textMuted }}>run</span>
                  <OwnerBadge kind={r.ownerKind} />
                  <span style={{ fontFamily: c.mono, fontWeight: 600, color: c.text }}>{r.ownerName}</span>
                  {!!r.delaySeconds && (
                    <span style={{ color: c.textMuted }}>· after {secondsLabel(r.delaySeconds)}</span>
                  )}
                  {!r.enabled && <OffChip />}
                </Row>
              </Fragment>
            ))}
          </div>
        </Section>
      )}
    </>
  );
}

import { useEffect, useState } from "react";
import { api } from "../api/client";
import { c } from "../theme";
import { appZoneName, fmtInAppZone } from "../utils/datetime";

// 2.5.0 C11, defect 84 (question 105): the freeze of the whole installation
// (`freeze.installation`, §0a open point 2) stops every agency's scheduled
// fires. Until C11 only the API set it and nothing in the app said it was on,
// so every schedule went quiet with no visible reason. This banner sits above
// every page while it is on.

export type InstallationFreeze = {
  on?: boolean;
  reason?: string;
  /** 2.5.2 FZ-1 — the window, each end optional (UTC). */
  from?: string;
  until?: string;
  /** 2.5.2 FZ-4 — the server's judgement now, so nothing here does clock arithmetic. */
  state?: "off" | "scheduled" | "in_effect" | "ended";
  setBy?: string;
  setAt?: string;
};

/** A freeze's moment in words, in the app zone ("Fri, 24 Oct, 18:00"). */
export function freezeWhen(iso?: string): string {
  return fmtInAppZone(iso, { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hour12: false });
}

/** The window in words, with the zone named (FZ-4, FZ-5). */
export function freezeWindowWords(f: { from?: string; until?: string }): string {
  const start = f.from ? `from ${freezeWhen(f.from)}` : "from now";
  const end = f.until ? `until ${freezeWhen(f.until)}` : "until lifted";
  return `${start} ${end}, ${appZoneName()}`;
}

/** The state, for a row written before the server said it (an old server): on is in effect. */
export function freezeState(f: InstallationFreeze | null): NonNullable<InstallationFreeze["state"]> {
  if (!f?.on) return "off";
  return f.state ?? "in_effect";
}

/** FREEZE_CHANGED_EVENT is fired by the Settings switch so the banner reads the freeze again at once. */
export const FREEZE_CHANGED_EVENT = "cronomicon:freeze-changed";

/** How often the banner reads the freeze when nothing tells it of a change (another person may set it). */
const FREEZE_POLL_MS = 60_000;

export function useInstallationFreeze(): InstallationFreeze | null {
  const [freeze, setFreeze] = useState<InstallationFreeze | null>(null);
  useEffect(() => {
    let alive = true;
    const read = async () => {
      const { data } = await api.GET("/settings/freeze");
      if (alive) setFreeze((data as InstallationFreeze | undefined) ?? null);
    };
    void read();
    const t = setInterval(() => void read(), FREEZE_POLL_MS);
    const again = () => void read();
    window.addEventListener(FREEZE_CHANGED_EVENT, again);
    return () => {
      alive = false;
      clearInterval(t);
      window.removeEventListener(FREEZE_CHANGED_EVENT, again);
    };
  }, []);
  return freeze;
}

export function InstallationFreezeBanner() {
  const freeze = useInstallationFreeze();
  const state = freezeState(freeze);
  // FZ-4: an ended window disappears by itself; off is nothing to say.
  if (!freeze || state === "off" || state === "ended") return null;
  if (state === "scheduled") {
    return (
      <div
        role="status"
        data-testid="installation-freeze-banner"
        data-state="scheduled"
        style={{
          marginBottom: 14,
          padding: "10px 14px",
          borderRadius: c.radiusChip,
          border: `2px solid ${c.warning}`,
          background: c.panel2,
          color: c.text,
          fontSize: c.fontSm,
        }}
      >
        <strong>Installation freeze {freezeWindowWords(freeze)}:</strong> {freeze.reason}. No run will start in any agency
        while it is in effect. A global administrator can cancel it under Settings → General.
      </div>
    );
  }
  return (
    <div
      role="status"
      data-testid="installation-freeze-banner"
      data-state="in_effect"
      style={{
        marginBottom: 14,
        padding: "10px 14px",
        borderRadius: c.radiusChip,
        // 2.5.1 (the owner): red, not orange, so it reads as "something is up".
        border: `2px solid ${c.danger}`,
        background: c.panel2,
        color: c.text,
        fontSize: c.fontSm,
      }}
    >
      <strong>
        Installation freeze: no run starts in any agency{freeze.until ? ` until ${freezeWhen(freeze.until)} (${appZoneName()})` : ""}.
      </strong>{" "}
      {freeze.reason}
      {(freeze.setBy || freeze.setAt) && (
        <span style={{ color: c.textMuted }}>
          {" "}
          — set{freeze.setBy ? ` by ${freeze.setBy}` : ""}
          {freeze.setAt ? ` at ${fmtInAppZone(freeze.setAt)}` : ""}
        </span>
      )}
      . Schedules, Run, tokens, reactions and file arrivals are all skipped; runs already going finish. A global
      administrator lifts it under Settings → General.
    </div>
  );
}

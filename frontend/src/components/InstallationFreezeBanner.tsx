import { useEffect, useState } from "react";
import { api } from "../api/client";
import { c } from "../theme";
import { fmtInAppZone } from "../utils/datetime";

// 2.5.0 C11, defect 84 (question 105): the freeze of the whole installation
// (`freeze.installation`, §0a open point 2) stops every agency's scheduled
// fires. Until C11 only the API set it and nothing in the app said it was on,
// so every schedule went quiet with no visible reason. This banner sits above
// every page while it is on.

export type InstallationFreeze = { on?: boolean; reason?: string; setBy?: string; setAt?: string };

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
  if (!freeze?.on) return null;
  return (
    <div
      role="status"
      data-testid="installation-freeze-banner"
      style={{
        marginBottom: 14,
        padding: "10px 14px",
        borderRadius: c.radiusChip,
        border: `1px solid ${c.warning}`,
        background: c.panel2,
        color: c.text,
        fontSize: c.fontSm,
      }}
    >
      <strong>Installation freeze: no run starts in any agency.</strong> {freeze.reason}
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

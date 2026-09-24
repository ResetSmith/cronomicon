import { useEffect, useState } from "react";
import { api } from "../../api/client";
import { c } from "../../theme";

// The application timezone now governs BOTH scheduling and display (timezone-update
// §5.4): cron fires in it and every timestamp renders in it. This non-blocking
// banner is informational — it tells the operator which zone the app is using and
// notes when their browser differs, so a "09:00" schedule is never a surprise. It
// changes no behavior. Shown only when the browser zone differs from the app zone.
interface GeneralTz {
  appTimezone?: string;
  serverTimezone?: string;
}

export function TimezoneAlert() {
  const [tz, setTz] = useState<GeneralTz | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.GET("/settings/general").then(({ data }) => {
      if (!cancelled && data) setTz(data as GeneralTz);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!tz?.appTimezone) return null;
  const browserZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  // Nothing to note when the viewer's browser already matches the app zone.
  if (browserZone === tz.appTimezone) return null;

  return (
    <div
      style={{
        marginBottom: 16,
        padding: "11px 14px",
        borderRadius: c.radiusSurface,
        border: `1px solid ${c.border}`,
        background: c.panel2,
        color: c.text,
        fontSize: c.fontSm,
        lineHeight: 1.55,
      }}
    >
      <span style={{ color: c.textSec, fontWeight: 700, marginRight: 6 }}>🕐 Application timezone</span>
      Times are shown — and schedules fire — in the application timezone (
      <strong>{tz.appTimezone}</strong>). Your browser is {browserZone}.
    </div>
  );
}

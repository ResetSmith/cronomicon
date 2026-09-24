import { useEffect, useState } from "react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { InlineLoading } from "../../components/ui";
import { setAppZone } from "../../utils/datetime";
import { Btn, Card, SettingRow, csrfHeader, errMsg, inputStyle } from "./ui";

type GeneralSettings = components["schemas"]["GeneralSettings"];

const TIMEZONES = [
  "UTC",
  "America/New_York",
  "America/Chicago",
  "America/Denver",
  "America/Los_Angeles",
  "Europe/London",
  "Europe/Berlin",
  "Europe/Paris",
  "Europe/Amsterdam",
  "Asia/Tokyo",
  "Asia/Singapore",
  "Asia/Kolkata",
  "Australia/Sydney",
];

// Left-aligned deliberately: Firefox always shows the native spin buttons at
// the right edge, and a right-aligned value sits flush against them.
const numInput = (): React.CSSProperties => ({ ...inputStyle(), width: 80 });

export function GeneralSection() {
  const [bump, setBump] = useState(0);
  const { data, error, loading } = useGet<GeneralSettings>(() => api.GET("/settings/general"), [bump]);
  const [form, setForm] = useState<GeneralSettings | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);

  useEffect(() => {
    if (data) setForm(data);
  }, [data]);

  // Keep the app-zone formatter in sync with the server's resolved appTimezone —
  // both on initial load and after a save (the save bumps the fetch). Without this,
  // changing the zone would re-time schedules on the backend but leave every
  // timestamp rendering in the OLD zone until a full reload (timezone-update §2.4).
  useEffect(() => {
    if (data?.appTimezone) setAppZone(data.appTimezone);
  }, [data?.appTimezone]);

  const patch = (p: Partial<GeneralSettings>) => setForm((f) => (f ? { ...f, ...p } : f));

  async function save() {
    if (!form) return;
    setSaving(true);
    setSaveErr(null);
    const { error: putErr } = await api.PUT("/settings/general", { params: { header: csrfHeader }, body: form });
    setSaving(false);
    if (putErr) {
      setSaveErr(errMsg(putErr));
    } else {
      setSaved(true);
      setTimeout(() => setSaved(false), 2200);
      setBump((b) => b + 1);
    }
  }

  const tzOpts =
    form?.timezone && !TIMEZONES.includes(form.timezone) ? [form.timezone, ...TIMEZONES] : TIMEZONES;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <Card
        title="General Settings"
        action={
          <Btn primary onClick={save} disabled={!form || saving} style={saved ? { background: c.success, borderColor: c.success } : undefined}>
            {saved ? "✓ Saved" : saving ? "Saving…" : "Save Changes"}
          </Btn>
        }
      >
        {loading && <InlineLoading />}
        {error && <div style={{ color: c.danger }}>Error: {error}</div>}
        {saveErr && <div style={{ color: c.danger, marginBottom: 10, fontSize: c.fontSm }}>Save failed: {saveErr}</div>}
        {!loading && !error && form && (
          <div style={{ fontSize: c.fontSm }}>
            <SettingRow label="App Name">
              <input
                value={form.appName ?? ""}
                onChange={(e) => patch({ appName: e.target.value })}
                style={{ ...inputStyle(), width: 180, textAlign: "right" }}
              />
            </SettingRow>
            <SettingRow
              label="Timezone"
              hint="Schedules fire in this zone and every timestamp is shown in it. Changing it re-times every schedule."
            >
              <select
                value={form.timezone ?? "UTC"}
                onChange={(e) => patch({ timezone: e.target.value })}
                style={{ ...inputStyle(), width: 220, cursor: "pointer" }}
              >
                {tzOpts.map((tz) => (
                  <option key={tz} value={tz}>
                    {tz}
                  </option>
                ))}
              </select>
            </SettingRow>
            {/* F2-1 removed the Session Idle Timeout and re-auth controls with
                the instruction "re-add them WITH the enforcement, not before" —
                sessionPolicy round-tripped through the PUT while nothing in auth
                read it, so an operator could set an idle timeout, watch it
                persist, and believe their console locked. FX-E4 wired the
                enforcement (auth.readSession), so the controls return.

                Job Timeout stays gone: sshexec enforces the JOB's own
                timeout_seconds, never the global jobTimeoutSeconds, and that
                one is still unenforced. The field still round-trips so nothing
                stored is lost. */}
            <SettingRow
              label="Session Idle Timeout"
              hint="Minutes of inactivity before a login expires. 0 disables the cap; the 8-hour session ceiling always applies."
            >
              <input
                type="number"
                min={0}
                max={480}
                value={form.sessionPolicy?.timeoutMinutes ?? 0}
                onChange={(e) =>
                  patch({
                    sessionPolicy: {
                      timeoutMinutes: Math.max(0, Number(e.target.value) || 0),
                      reauth: form.sessionPolicy?.reauth ?? false,
                    },
                  })
                }
                style={numInput()}
              />
            </SettingRow>
            <SettingRow
              label="Require periodic re-login"
              hint="Measures the timeout from LOGIN instead of from the last request: activity does not extend it, so everyone re-authenticates every N minutes. Off, the timeout is an idle cap that slides with use."
            >
              <input
                type="checkbox"
                checked={form.sessionPolicy?.reauth ?? false}
                disabled={(form.sessionPolicy?.timeoutMinutes ?? 0) === 0}
                onChange={(e) =>
                  patch({
                    sessionPolicy: {
                      timeoutMinutes: form.sessionPolicy?.timeoutMinutes ?? 0,
                      reauth: e.target.checked,
                    },
                  })
                }
              />
            </SettingRow>
            <SettingRow label="Max Concurrent Jobs" hint="The most jobs that can run at once, across every runner." last>
              <input
                type="number"
                min={1}
                max={50}
                value={form.maxConcurrent ?? 1}
                onChange={(e) => patch({ maxConcurrent: Number(e.target.value) || 1 })}
                style={numInput()}
              />
            </SettingRow>
          </div>
        )}
      </Card>

      <Card title="Documentation">
        <div style={{ fontSize: c.fontSm }}>
          <SettingRow
            label="Administrator Manual"
            hint="Full operator reference — install, configuration, and administration. Opens in a new tab."
            last
          >
            <a
              href="/administrator-manual.html"
              target="_blank"
              rel="noopener noreferrer"
              style={manualLinkStyle()}
            >
              Open Manual ↗
            </a>
          </SettingRow>
        </div>
      </Card>
    </div>
  );
}

// manualLinkStyle mirrors the app's secondary-button look (matching the header's
// top-bar controls) for the anchor that opens the administrator manual. Built at
// render time from the mutable `c` tokens so it repaints on the light/dark toggle.
const manualLinkStyle = (): React.CSSProperties => ({
  display: "inline-flex",
  alignItems: "center",
  gap: 6,
  padding: "8px 14px",
  borderRadius: c.radiusChip,
  border: `1px solid ${c.border}`,
  background: c.panelInput,
  color: c.text,
  fontSize: c.fontSm,
  fontWeight: 600,
  textDecoration: "none",
  whiteSpace: "nowrap",
});

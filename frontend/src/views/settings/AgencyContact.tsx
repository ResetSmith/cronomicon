import { useEffect, useState } from "react";
import { api } from "../../api/client";
import { useGet } from "../../hooks";
import { c } from "../../theme";
import { Card, SaveBtn, csrfHeader, errMsg, inputStyle, lblStyle } from "./ui";

// The agency's contact (2.5.2 OP-28) — who the central team calls about the
// agency's runs when the job or workflow names no contact of its own. Shown,
// marked as the agency's, on the Dashboard's attention items and on run
// details. Optional: empty is none. On the Alerts page because it is the same
// question an alert answers — who should hear about this agency's runs — and
// the same people keep it (configureApp on the agency, or a global
// administrator).
//
// `why` is the reason the caller may not change it ("" when they may).
const MAX = 256;

export function AgencyContactCard({ agency, agencyName, why }: { agency: string; agencyName: string; why: string }) {
  const [bump, setBump] = useState(0);
  // The per-agency view carries the agency row; its readers are the agency's
  // people and everywhere-readers, which is everyone who reaches this page.
  const q = useGet<{ agency?: { contact?: string } }>(
    () => api.GET("/agencies/{agencyId}", { params: { path: { agencyId: agency } } }),
    [agency, bump],
  );
  const stored = q.data?.agency?.contact ?? "";
  const [value, setValue] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // The stored value is the form's start, and again after each save.
  useEffect(() => setValue(stored), [stored]);

  async function save() {
    setSaving(true);
    setErr(null);
    const { error } = await api.PUT("/agencies/{agencyId}/contact", {
      params: { path: { agencyId: agency }, header: csrfHeader },
      body: { contact: value },
    });
    setSaving(false);
    if (error) {
      setErr(errMsg(error));
      return;
    }
    setSaved(true);
    window.setTimeout(() => setSaved(false), 1500);
    setBump((b) => b + 1);
  }

  if (!agency) return null;
  return (
    <Card title="Agency contact">
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.6 }}>
          Who the operations team should call about {agencyName || "this agency"}'s runs when a job or workflow names no contact of its
          own. It shows, marked as the agency's, beside each such run on the Dashboard and in History. Optional; a job's own contact
          (on the Jobs page) always comes first.
        </div>
        <label htmlFor="agency-contact" style={lblStyle()}>
          Contact
        </label>
        <div style={{ display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap" }}>
          <input
            id="agency-contact"
            value={value}
            maxLength={MAX}
            disabled={!!why || q.loading}
            placeholder="An on-call rotation, a mailing list or a phone number"
            onChange={(e) => setValue(e.target.value)}
            style={{ ...inputStyle(), flex: 1, minWidth: 260 }}
          />
          <SaveBtn onClick={save} saving={saving} saved={saved} disabled={value.trim() === stored.trim()} reason={why} />
        </div>
        {why && <div style={{ fontSize: c.fontXs, color: c.textMuted }}>{why}</div>}
        {(err || q.error) && <div style={{ fontSize: c.fontSm, color: c.danger }}>{err ?? q.error}</div>}
      </div>
    </Card>
  );
}

import { c } from "../../theme";
import { Card } from "../../components/ui";
import { RunAnalytics } from "../../components/RunAnalytics";

// History → Analytics (SL-E).
//
// The fleet-wide view, and the home the shipped Prometheus endpoint never had a
// UI for. Same component and same endpoint as the per-job panel in the Jobs
// detail, just without a job filter — so the two can never disagree about how a
// success rate is computed.

export function AnalyticsTab() {
  return (
    <Card title="Fleet analytics">
      <div style={{ fontSize: c.fontSm, color: c.textSec, lineHeight: 1.5, paddingBottom: 12 }}>
        Every job run you can see, aggregated over the selected window. Computed from run history on
        request, so these numbers always agree with the Executions tab.
        <br />
        <strong>Late</strong> counts runs warned as past a deadline; <strong>Missed</strong> counts
        scheduled fires that produced no run at all and had no calendar, pause or concurrency
        suppression on record to explain them.
      </div>
      <RunAnalytics />
    </Card>
  );
}

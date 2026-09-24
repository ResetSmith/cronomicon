import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { c } from "../theme";
import { ExecutionsTab } from "./history/ExecutionsTab";
import { WorkflowRunsTab } from "./history/WorkflowRunsTab";
import { ChangeLogTab } from "./history/ChangeLogTab";
import { SchedulePushesTab } from "./history/SchedulePushesTab";
import { GitSyncTab } from "./history/GitSyncTab";
import { AnalyticsTab } from "./history/AnalyticsTab";

const TABS = ["Executions", "Workflow Runs", "Analytics", "Change Log", "Schedule Pushes", "Git Sync"];
const SLUGS = ["executions", "workflow-runs", "analytics", "change-log", "schedule-pushes", "git-sync"];
const PANELS = [ExecutionsTab, WorkflowRunsTab, AnalyticsTab, ChangeLogTab, SchedulePushesTab, GitSyncTab];

export function History() {
  // Tab index is driven by ?tab= so other views can deep-link a specific tab
  // (e.g. the Jobs publish builder → Schedule Pushes); defaults to Executions.
  const [params, setParams] = useSearchParams();
  const initial = Math.max(0, SLUGS.indexOf(params.get("tab") ?? "executions"));
  const [tab, setTab] = useState(initial);
  // Tabs are mounted lazily on first visit, then kept alive (display:none) so
  // each tab retains its own pagination/filter state — fixes prototype X3,
  // where a single shared page index leaked across tabs.
  const [seen, setSeen] = useState<number[]>([initial]);
  const select = (i: number) => {
    setTab(i);
    if (!seen.includes(i)) setSeen([...seen, i]);
    const next = new URLSearchParams(params);
    if (i === 0) next.delete("tab");
    else next.set("tab", SLUGS[i]);
    setParams(next, { replace: true });
  };

  return (
    <div>
      <div style={{ display: "flex", gap: 4, borderBottom: `1px solid ${c.border}`, marginBottom: 16 }}>
        {TABS.map((t, i) => (
          <button
            key={t}
            onClick={() => select(i)}
            style={{
              padding: "8px 14px",
              fontSize: c.fontSm,
              fontFamily: c.sans,
              cursor: "pointer",
              background: "transparent",
              border: "none",
              color: tab === i ? c.primary : c.textSec,
              fontWeight: tab === i ? 600 : 400,
              borderBottom: `2px solid ${tab === i ? c.primary : "transparent"}`,
              marginBottom: -1,
            }}
          >
            {t}
          </button>
        ))}
      </div>
      {PANELS.map((Panel, i) =>
        seen.includes(i) ? (
          <div key={TABS[i]} style={{ display: tab === i ? "block" : "none" }}>
            <Panel />
          </div>
        ) : null,
      )}
    </div>
  );
}

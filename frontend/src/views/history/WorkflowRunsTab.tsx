import { useSearchParams } from "react-router-dom";
import { WorkflowRunsTable } from "../workflows/WorkflowRunsTable";

// History → Workflow Runs tab. Thin wrapper over the shared WorkflowRunsTable
// (WB-O5): the same paged surface as the Workflows page, now WITH per-run
// step-level drill-down (graph + timeline + per-step logs/context).
//
// RX-17 — it also honours ?reactedTo=, the reverse because-of pivot. A reaction
// can fire a job OR a workflow, so "what did this run set off?" has to be asked
// of both tables; answering it only on the Executions tab silently drops every
// workflow a cascade started.
export function WorkflowRunsTab() {
  const [params] = useSearchParams();
  const reactedTo = params.get("reactedTo") ?? undefined;
  const traceFocus = params.get("trace") ?? undefined;
  return (
    <WorkflowRunsTable storageKey="history-workflowruns" reactedTo={reactedTo} traceFocus={traceFocus} />
  );
}

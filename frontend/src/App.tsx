import { lazy, Suspense } from "react";
import { Routes, Route, Navigate } from "react-router-dom";
import { useAuth } from "./auth";
import { Shell } from "./components/Shell";
import { Login } from "./views/Login";
import { Jobs } from "./views/Jobs";
import { Scripts } from "./views/Scripts";
import { Schedules } from "./views/Schedules";
import { ScheduleBuilder } from "./views/ScheduleBuilder";
import { JobComposer } from "./views/JobComposer";
import { PublishBuilder } from "./views/jobs/PublishBuilder";
import { Activity } from "./views/Activity";
import { Dashboard } from "./views/Dashboard";
import { Workflows } from "./views/Workflows";
import { History } from "./views/History";
import { EnvVars } from "./views/EnvVars";
import { Scopes } from "./views/Scopes";
import { Settings } from "./views/Settings";
import { Runners } from "./views/Runners";
import { TimezoneProvider } from "./timezone-context";
import { useTheme } from "./theme-context";
import { c } from "./theme";

// Lazy-loaded so the workflow editor/canvas (React Flow, WC-P0) lands in its own
// chunk rather than the eager bundle — the app's first code-split. The view is a
// named export, hence the `{ default: m.WorkflowEditor }` interop shim.
const WorkflowEditor = lazy(() =>
  import("./views/WorkflowEditor").then((m) => ({ default: m.WorkflowEditor })),
);

export function App() {
  const { me, loading } = useAuth();
  // Subscribe App to the theme context so a light/dark toggle RE-RENDERS the route
  // tree — recreating the route elements below — instead of Shell remounting the
  // Outlet. The re-render repaints every view from the mutable `c` tokens while
  // preserving each view's local state (active tab, search, pagination, expanded
  // rows); a remount would reset all of it.
  useTheme();

  if (loading) {
    return <div style={{ height: "100vh", display: "grid", placeItems: "center", background: c.bg, color: c.textSec }}>Loading…</div>;
  }
  if (!me) return <Login />;

  return (
    <TimezoneProvider>
      <Routes>
      <Route element={<Shell />}>
        <Route index element={<Dashboard />} />
        <Route path="jobs" element={<Jobs />} />
        <Route path="compose" element={<JobComposer />} />
        <Route path="jobs/publish" element={<PublishBuilder />} />
        <Route path="scripts" element={<Scripts />} />
        <Route path="schedules" element={<Schedules />} />
        <Route path="schedule-builder" element={<ScheduleBuilder />} />
        <Route path="workflows" element={<Workflows />} />
        <Route
          path="workflow-editor"
          element={
            <Suspense fallback={<div style={{ padding: 24, color: c.textSec }}>Loading editor…</div>}>
              <WorkflowEditor />
            </Suspense>
          }
        />
        <Route path="activity" element={<Activity />} />
        <Route path="runs" element={<History />} />
        {/* The Schedule hub was dissolved — keep a redirect for stale bookmarks. */}
        <Route path="schedule" element={<Navigate to="/schedules" replace />} />
        <Route path="scopes" element={<Scopes />} />
        <Route path="env-vars" element={<EnvVars />} />
        <Route path="runners" element={<Runners />} />
        <Route path="settings" element={<Settings />} />
      </Route>
      </Routes>
    </TimezoneProvider>
  );
}

import { useEffect, useRef, useState } from "react";
import { api } from "../../api/client";
import { c } from "../../theme";
import { CopyButton } from "../../components/ui";
import { ErrorMsg, Loading } from "./shared";

// Shared redacted-log viewer for a single run (WB-O1). The run's trace id is the
// log key for GET /runs/{traceId}/log — the same endpoint the executions drill-in
// uses — so a workflow step (whose id IS its child-run trace id) reuses this with
// no new plumbing. Extracted from ExecutionsTab's RunDetail to avoid drift; the
// Jobs recent-runs drill-in adopts it too (CC.20) with an empty label to keep its
// header-less look.
//
// EP-8b (the expanded-panels plan) — LIVE TAILING.
//
// `active` turns on follow-the-tail for a run that is still going. It defaults
// to false, so a mount that does not pass it behaves exactly as it did before
// EP-8: one fetch, no polling.
//
// The transport is a POLL, not SSE/WebSocket, and deliberately: the app has no
// browser-streaming plumbing anywhere, a ~2.5s poll is indistinguishable from a
// stream for a human watching a log, and v1's single-container-behind-arbitrary-
// proxies invariant makes long-lived connections the fragile choice. A stream
// would slot behind this same component's seam later if a real need appears.
//
// The poll is incremental: each request sends ?offset= and the server answers
// with the bytes after it plus X-Log-Offset (EP-8a). Without that, a 2.5s poll
// would re-download a chatty Ansible run's whole log to gain a few lines.
const TAIL_MS = 2500;

export function RunLog({ traceId, label = "Log Output (redacted)", active = false }: { traceId: string; label?: string; active?: boolean }) {
  const [text, setText] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const offset = useRef(0);
  // `following` is the sticky-tail state: true while the reader is parked at the
  // bottom. It is not a preference toggle — scrolling up sets it false and
  // scrolling back to the bottom sets it true again, which is the behaviour a
  // tail has to have if reading mid-log while lines land is to be possible.
  const [following, setFollowing] = useState(true);
  const [pre, setPre] = useState<HTMLPreElement | null>(null);

  // One fetch primitive for every path (first load, tail tick, final read).
  // Returns whether the run's log grew, which is all the caller needs.
  const fetchFrom = useRef<(initial: boolean) => Promise<void>>(async () => {});
  fetchFrom.current = async (initial: boolean) => {
    try {
      const res = await api.GET("/runs/{traceId}/log", {
        params: { path: { traceId }, query: { offset: offset.current } },
        parseAs: "text",
      });
      if (res.error) {
        // A failed TICK keeps the buffer: a transient blip must not blank a log
        // someone is reading. A failed FIRST load is the real error.
        if (initial) setError(String((res.error as { message?: string })?.message ?? "request failed"));
        return;
      }
      const chunk = (res.data as unknown as string) ?? "";
      // The server's X-Log-Offset is authoritative — it is measured before the
      // body is written, so it never covers bytes we did not receive. Falling
      // back to a local byte count would drift on any multi-byte character.
      const hdr = res.response?.headers?.get("X-Log-Offset");
      const next = hdr != null && hdr !== "" ? Number(hdr) : offset.current + chunk.length;
      // A server offset BELOW ours means the file was reaped, rotated or
      // replaced under us. Restart from zero rather than appending a fresh
      // file's bytes onto a stale buffer, which would render an impossible log.
      if (Number.isFinite(next) && next < offset.current) {
        offset.current = 0;
        setText(chunk);
        return;
      }
      offset.current = Number.isFinite(next) ? next : offset.current + chunk.length;
      if (chunk) setText((prev) => (initial ? chunk : prev + chunk));
      else if (initial) setText("");
      setError(null);
    } catch (e) {
      if (initial) setError(String((e as { message?: string })?.message ?? "request failed"));
    }
  };

  // Identity change (a different run) is a full reset, never an append.
  useEffect(() => {
    let cancelled = false;
    offset.current = 0;
    setText("");
    setError(null);
    setLoading(true);
    setFollowing(true);
    void fetchFrom.current(true).finally(() => {
      if (!cancelled) setLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, [traceId]);

  // The tail. Polls while active; on the flip to terminal it does ONE final
  // read — without it the viewer permanently misses whatever was written
  // between the last tick and the run finishing, which is usually the part
  // that says why it ended.
  const wasActive = useRef(active);
  useEffect(() => {
    if (active) {
      wasActive.current = true;
      const t = setInterval(() => void fetchFrom.current(false), TAIL_MS);
      return () => clearInterval(t);
    }
    if (wasActive.current) {
      wasActive.current = false;
      void fetchFrom.current(false);
    }
  }, [active]);

  // Sticky follow. 1px of slack because fractional line heights leave
  // scrollTop + clientHeight just short of scrollHeight at the bottom — the
  // same reason NotesBody's fade carries it.
  useEffect(() => {
    if (!pre) return;
    const sync = () => setFollowing(pre.scrollHeight - pre.clientHeight - pre.scrollTop <= 1);
    pre.addEventListener("scroll", sync, { passive: true });
    return () => pre.removeEventListener("scroll", sync);
  }, [pre]);

  // Park at the tail on every new chunk, but only while following.
  useEffect(() => {
    if (pre && following) pre.scrollTop = pre.scrollHeight;
  }, [text, following, pre]);

  return (
    <div>
      {label && (
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
          <span style={{ fontSize: c.fontXs, fontFamily: c.sansCond, fontWeight: 600, color: c.textSec, textTransform: "uppercase", letterSpacing: 0.7 }}>
            {label}
          </span>
          {active && <TailingChip />}
        </div>
      )}
      {loading && <Loading />}
      {error && <ErrorMsg msg={error} />}
      {!loading && !error && (
        // FX-3 — the copy affordance overlays the log rather than sitting above it,
        // so every one of the four surfaces that mount this component (Jobs, Runners,
        // Workflow steps, History → Executions) gets it without touching their
        // headers. Geometry follows Scripts' CodeBlockWithCopy; the glyph is the
        // TraceId clipboard pair, which is the one clipboard icon in the app and
        // already sits in the same rows as this log.
        <div style={{ position: "relative" }}>
          <CopyButton text={text} overlay ariaLabel="Copy log to clipboard" />
          <pre
            ref={setPre}
            style={{
              margin: 0,
              padding: "12px 14px",
              background: c.bg,
              border: `1px solid ${c.border}`,
              borderRadius: c.radiusSurface,
              fontFamily: c.mono,
              fontSize: c.fontXs,
              lineHeight: 1.6,
              color: c.textSec,
              overflowX: "auto",
              maxHeight: 400,
              whiteSpace: "pre-wrap",
            }}
          >
            {text || "No log output recorded for this run."}
          </pre>
          {/* Shown only when the reader has scrolled away from the tail while a
              run is still writing — i.e. exactly when new lines are arriving
              off-screen. A permanent control would be noise on a finished log. */}
          {active && !following && (
            <button
              type="button"
              onClick={() => setFollowing(true)}
              style={{
                position: "absolute",
                right: 12,
                bottom: 12,
                padding: "3px 9px",
                borderRadius: c.radiusChip,
                border: `1px solid ${c.border}`,
                background: c.panel,
                color: c.textSec,
                fontFamily: c.sans,
                fontSize: c.fontXs,
                cursor: "pointer",
              }}
            >
              ↓ Resume following
            </button>
          )}
        </div>
      )}
    </div>
  );
}

// The "this is live" cue. Its pulse lives inside the reduced-motion guard in
// global-css (the `pulse` keyframes), so a reader who asked for no motion gets
// the dot without the animation.
function TailingChip() {
  return (
    <span
      title="Following this run's log as it is written"
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 5,
        fontSize: c.fontXs,
        fontFamily: c.sans,
        color: c.textMuted,
        textTransform: "none",
        letterSpacing: 0,
      }}
    >
      <span
        style={{
          width: 6,
          height: 6,
          borderRadius: "50%",
          background: c.success,
          animation: "pulse 1.8s ease-in-out infinite",
        }}
      />
      Tailing
    </span>
  );
}

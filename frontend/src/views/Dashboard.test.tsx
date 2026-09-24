// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { groupMarks, scoreWindow, inWindow, type Mark } from "../components/score-model";
import { c } from "../theme";

// VU-19: the two tests that used to live here rendered `JobHistogram`, which
// Phase D deleted along with the three list cards. Their replacement is not a
// like-for-like port — the histogram's empty state became the Score's, and that
// is covered where the Score lives (components/Score.test.tsx).
//
// What belongs HERE is the part of Phase D that is the Dashboard's own job: it
// owns the wire→Mark normalisation feeding the Score, and several decisions in
// that mapping are easy to get wrong and invisible when they are.

const NOW = Date.parse("2026-07-27T19:05:00Z");

// The mapping under test, mirroring Dashboard's useMemo. Kept local rather than
// exported from the view: the view's copy is a dozen lines of obvious code, and
// exporting it purely for a test would invert the dependency.
interface WireRun {
  jobName?: string;
  status?: string;
  startedAt?: string | null;
  durationMs?: number | null;
  kind?: string;
  traceId?: string;
}
interface WireUpcoming {
  ownerKind?: string;
  ownerName?: string;
  at?: string;
}

function toMarks(runs: WireRun[], upcoming: WireUpcoming[]): Mark[] {
  const out: Mark[] = [];
  for (const r of runs) {
    const at = r.startedAt ? Date.parse(r.startedAt) : NaN;
    if (Number.isNaN(at) || r.kind === "ssh-test") continue;
    out.push({ at, tense: "past", jobName: r.jobName ?? "—", status: r.status, durationMs: r.durationMs ?? null, traceId: r.traceId });
  }
  for (const u of upcoming) {
    const at = u.at ? Date.parse(u.at) : NaN;
    if (Number.isNaN(at)) continue;
    out.push({ at, tense: "future", jobName: u.ownerName ?? "—", ownerKind: u.ownerKind });
  }
  return out;
}

describe("Dashboard → Score normalisation", () => {
  it("plots a run by its START, not its completion", () => {
    // The old histogram bucketed on completedAt. A timeline must not: a
    // 90-minute run that finished a minute ago belongs where it began, or its
    // stem — which encodes duration — sits at the wrong end of what it measures.
    const marks = toMarks([{ jobName: "long", status: "success", startedAt: "2026-07-27T17:35:00Z", durationMs: 5_400_000 }], []);
    expect(marks[0].at).toBe(Date.parse("2026-07-27T17:35:00Z"));
  });

  it("drops a run that never started, rather than plotting it at the epoch", () => {
    // A queued run has a null startedAt, and Date.parse(null) is NaN — which
    // becomes a mark pinned to the far left if nothing filters it out.
    expect(toMarks([{ jobName: "queued", status: "queued", startedAt: null }], [])).toHaveLength(0);
  });

  it("excludes ssh-test probes, which are diagnostics rather than job runs", () => {
    const marks = toMarks(
      [
        { jobName: "real", status: "success", startedAt: "2026-07-27T18:00:00Z" },
        { jobName: "probe", status: "success", startedAt: "2026-07-27T18:00:00Z", kind: "ssh-test" },
      ],
      [],
    );
    expect(marks).toHaveLength(1);
    expect(marks[0].jobName).toBe("real");
  });

  it("splits the two feeds by tense, which is the whole point of the component", () => {
    const marks = toMarks(
      [{ jobName: "ran", status: "success", startedAt: "2026-07-27T18:00:00Z" }],
      [{ ownerName: "will-run", ownerKind: "job", at: "2026-07-27T20:00:00Z" }],
    );
    expect(marks.find((m) => m.jobName === "ran")?.tense).toBe("past");
    expect(marks.find((m) => m.jobName === "will-run")?.tense).toBe("future");
  });

  it("keeps a run that started inside the window but was queued before it", () => {
    // /runs filters on created_at (enqueue), NOT startedAt — which is why the
    // request asks for an hour more than the window displays. This is the case
    // that margin exists for.
    const w = scoreWindow(NOW);
    const marks = toMarks([{ jobName: "slow-start", status: "success", startedAt: new Date(NOW - 23.9 * 3600_000).toISOString() }], []);
    expect(inWindow(w, marks[0].at)).toBe(true);
  });

  it("hands the Score a set in which a coincident failure survives grouping", () => {
    // The end-to-end form of VU-21: two jobs share a cron minute and one fails.
    // Through normalisation AND grouping, the surviving mark must be the failure.
    const at = "2026-07-27T06:00:00Z";
    const groups = groupMarks(
      toMarks(
        [
          { jobName: "python-metrics-export", status: "success", startedAt: at },
          { jobName: "win-update-check", status: "danger", startedAt: at },
        ],
        [],
      ),
    );
    expect(groups).toHaveLength(1);
    expect(groups[0].status).toBe("danger");
    expect(groups[0].members).toHaveLength(2);
  });
});

// Feeds the view drives itself from. Mutable so a test can put the dashboard
// into its failure state or its all-clear state without a second mock factory —
// CS-1 and CS-4 differ precisely by which of these is non-empty.
const feeds = vi.hoisted(() => ({
  failed: [] as unknown[],
  warned: [] as unknown[],
  upcoming: [] as unknown[],
}));

// Mutable so a test can exercise the no-jobs state (VU2-1's invitation).
const jobsFeed: Record<string, unknown>[] = [];
const DEFAULT_JOBS = [
  { name: "alpha", type: "bash", schedule: "0 2 * * *" },
  { name: "beta", type: "ansible" },
];

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    api: {
      GET: vi.fn(async (path: string, opts?: { params?: { query?: Record<string, unknown> } }) => {
        if (path === "/jobs") {
          return { data: { items: jobsFeed } };
        }
        if (path === "/schedules/upcoming") {
          return { data: { items: feeds.upcoming } };
        }
        // The dashboard issues several separately-filtered /runs requests; the
        // status in the query is what tells them apart (CC.23's whole point).
        if (path === "/runs") {
          const status = opts?.params?.query?.status;
          if (status === "danger") return { data: { items: feeds.failed, totalItems: feeds.failed.length } };
          if (status === "warning") return { data: { items: feeds.warned, totalItems: feeds.warned.length } };
        }
        return { data: { items: [], totalItems: 0 } };
      }),
    } as unknown as typeof actual.api,
  };
});

/** Renders the current path, so a navigation can be asserted on. */
function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="location">{`${loc.pathname}${loc.search}`}</div>;
}

const renderDashboard = async (withProbe = false) => {
  const { Dashboard } = await import("./Dashboard");
  render(
    <MemoryRouter>
      <Dashboard />
      {withProbe && <LocationProbe />}
    </MemoryRouter>,
  );
  await waitFor(() => expect(screen.getByRole("region", { name: "Score" })).toBeTruthy());
};

beforeEach(() => {
  feeds.failed = [];
  feeds.warned = [];
  feeds.upcoming = [];
  jobsFeed.length = 0;
  jobsFeed.push(...DEFAULT_JOBS);
  // The verdict strip's dismissal is persisted per browser, and jsdom shares one
  // localStorage across the file — without this, the first test that dismisses
  // silences the strip for every test after it.
  localStorage.clear();
});
afterEach(cleanup);

// FX-1 — the Dashboard's own half of the expansion: it owns the expanded state
// and threads the job list into the panel. Score.test.tsx proves the swap; what
// only a render of the view can prove is that the wiring reaches it, and that the
// dashboard now renders ONE Score surface rather than a panel plus a detached
// block with a button stranded between them.
describe("Dashboard → Score panel (FX-1)", () => {

  it("renders one Score surface, with the expand control inside it", async () => {
    await renderDashboard();

    expect(screen.getAllByRole("region", { name: "Score" })).toHaveLength(1);
    const toggle = screen.getByRole("button", { name: /Full score/ });
    // Inside the panel: the button used to live in a row of its own beneath it.
    expect(screen.getByRole("region", { name: "Score" }).contains(toggle)).toBe(true);
    // Counted from /jobs — every job gets a staff, including the unscheduled one.
    expect(toggle.textContent).toContain("Full score (2 jobs)");
  });

  it("expands in place when the control is clicked, and collapses back", async () => {
    await renderDashboard();
    const panel = () => screen.getByRole("region", { name: "Score" });
    expect(screen.getByRole("listbox")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /Full score/ }));

    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    const full = document.querySelector('[aria-label="Full score"]')!;
    expect(panel().contains(full)).toBe(true);
    // The job list reached FullScore: both jobs get a staff, sectioned by type.
    // Neither has a run or a projected fire in this fixture, so both are quiet
    // and arrive behind VU2-2's fold — which is itself the proof the list got
    // through, since the count comes from the job names FullScore was handed.
    fireEvent.click(screen.getByRole("button", { name: /2 quiet jobs/ }));
    expect(document.querySelector('[title="alpha"]')).toBeTruthy();
    expect(document.querySelector('[title="beta"]')).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /Collapse/ }));
    await waitFor(() => expect(screen.getByRole("listbox")).toBeTruthy());
    expect(document.querySelector('[aria-label="Full score"]')).toBeNull();
  });
});

// ── Stat tiles (VU2-2) ───────────────────────────────────────────────────────
// Two of the four tiles were inert while their neighbours lifted on hover and
// navigated, which in one row reads as "this number is not about anything".
// What is worth pinning is the DESTINATION, because ?result=running only works
// by virtue of History's alias folding — a rename there would break this
// silently, and the tile would look fine while going nowhere useful.
describe("Dashboard → stat tiles (VU2-2)", () => {
  const tile = (label: string) => screen.getByText(label).closest('[role="button"]') as HTMLElement;

  it("sends every tile somewhere, including the two that used to be inert", async () => {
    await renderDashboard(true);
    const path = () => screen.getByTestId("location").textContent;

    fireEvent.click(tile("Running now"));
    expect(path()).toBe("/runs?result=running");

    fireEvent.click(tile("Total jobs"));
    expect(path()).toBe("/jobs");

    fireEvent.click(tile("Failed (24h)"));
    expect(path()).toBe("/runs?result=fail");

    fireEvent.click(tile("Active schedules"));
    expect(path()).toBe("/schedules?tab=inventory");
  });

  it("reaches the tiles from the keyboard, not the pointer only", async () => {
    await renderDashboard(true);
    fireEvent.keyDown(tile("Total jobs"), { key: "Enter" });
    expect(screen.getByTestId("location").textContent).toBe("/jobs");
  });
});

// ── Current Status (CS-1 / CS-3 / CS-4) ──────────────────────────────────────
// The Dashboard's answer to "is everything okay?" in words, for the reader who
// will never hover the Score. What these tests pin is the part that is easy to
// regress silently: the banner staying gone, the forward-looking fact being
// present exactly once, and rows pointing at the JOB rather than at every run
// sharing its outcome.

// The view reads the real clock (`now` is quantised Date.now()), and the
// upcoming projection is filtered to instants AFTER it — so these fixtures are
// relative rather than the fixed NOW the normalisation tests above use.
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const ahead = (ms: number) => new Date(Date.now() + ms).toISOString();
const FAILED = { traceId: "t-1", jobName: "k8s-node-drain", status: "danger", startedAt: ago(21 * 60_000), completedAt: ago(20 * 60_000) };
const NEXT_UP = { ownerKind: "job", ownerName: "disk-usage-audit", at: ahead(41 * 60_000) };

// ── The verdict strip (VU2-1) ────────────────────────────────────────────────
// The page's thesis, first. What these pin is the part that inverted before:
// the calm state must be QUIET (the old all-clear was the loudest element on
// the page and fired when nothing was wrong) and the attention state must be
// loud, present without scrolling, and carry the EXACT counts rather than the
// fetched page's length.
describe("Dashboard → verdict strip (VU2-1)", () => {
  const verdict = () => screen.getByLabelText("Status");
  // jsdom re-serialises colours with spaces after the commas, so compare on a
  // whitespace-stripped form rather than on the token's own literal.
  const bg = () => verdict().style.background.replace(/\s+/g, "");
  const DANGER_BG = c.dangerBg.replace(/\s+/g, "");

  it("leads the page — before the tiles and the timeline, not after them", async () => {
    await renderDashboard();
    const panel = screen.getByRole("region", { name: "Score" });
    // DOCUMENT_POSITION_FOLLOWING: the Score comes after the verdict.
    expect(verdict().compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("states the all-clear once, quietly, and drops the card that shouted it", async () => {
    await renderDashboard();
    await screen.findByText(/All 2 jobs healthy\./);
    // Exactly once on the page: the Current-status all-clear card is gone.
    expect(screen.getAllByText(/All 2 jobs healthy\./)).toHaveLength(1);
    // Quiet: no danger tint while nothing is wrong.
    expect(bg()).not.toBe(DANGER_BG);
    expect(verdict().getAttribute("role")).not.toBe("button");
  });

  it("names what is running instead of claiming nothing is", async () => {
    await renderDashboard();
    expect(await screen.findByText("Nothing running.")).toBeTruthy();
  });

  it("turns loud when a job needs attention, and counts from the exact totals", async () => {
    feeds.failed = [FAILED];
    feeds.warned = [{ ...FAILED, traceId: "w-1", jobName: "log-rotate-web", status: "warning" }];
    await renderDashboard();

    await screen.findByText(/2 jobs need attention\./);
    expect(screen.getByText(/1 failed · 1 warned in the last 24 hours\./)).toBeTruthy();
    expect(bg()).toBe(DANGER_BG);
    // Actionable: it points at the evidence further down the same page, via a
    // REAL button (the dismiss control lives beside it, and an interactive
    // element nested in a role="button" div is announced to nobody).
    expect(within(verdict()).getByTitle("Jump to the recent errors").tagName).toBe("BUTTON");
  });

  // The dismissal is bound to the SITUATION, not to the strip: it silences what
  // the operator has seen and cannot swallow what they have not.
  it("can be dismissed, and stays dismissed while nothing changes", async () => {
    feeds.failed = [FAILED];
    await renderDashboard();

    fireEvent.click(await screen.findByLabelText("Dismiss this alert until something changes"));
    expect(screen.queryByText(/needs attention\./)).toBeNull();

    // Re-mounting with the same failures keeps it dismissed — the decision
    // outlives the page, which is the whole point of persisting it.
    cleanup();
    await renderDashboard();
    await screen.findByText("Up next");
    expect(screen.queryByText(/needs attention\./)).toBeNull();
  });

  it("comes back the moment something new breaks", async () => {
    feeds.failed = [FAILED];
    await renderDashboard();
    fireEvent.click(await screen.findByLabelText("Dismiss this alert until something changes"));
    expect(screen.queryByText(/needs attention\./)).toBeNull();

    // A second job fails: a different situation, so the acknowledgement of the
    // first one no longer applies.
    cleanup();
    feeds.failed = [FAILED, { ...FAILED, traceId: "t-9", jobName: "cert-renewal" }];
    await renderDashboard();
    expect(await screen.findByText(/2 jobs need attention\./)).toBeTruthy();
  });

  it("never hides Recent errors — the summary is quietened, not the evidence", async () => {
    feeds.failed = [FAILED];
    await renderDashboard();

    fireEvent.click(await screen.findByLabelText("Dismiss this alert until something changes"));
    expect(screen.queryByText(/needs attention\./)).toBeNull();
    // The job is still named, and still one click from its History.
    expect(screen.getByText("k8s-node-drain")).toBeTruthy();
  });

  it("is not offered at all when there is nothing wrong", async () => {
    await renderDashboard();
    await screen.findByText(/All 2 jobs healthy\./);
    expect(screen.queryByLabelText("Dismiss this alert until something changes")).toBeNull();
  });

  it("invites the first job when there are none", async () => {
    jobsFeed.length = 0;
    await renderDashboard();
    expect(await screen.findByText("No jobs yet.")).toBeTruthy();
    expect(screen.getByText("Create a job in Compose and it will report in here.")).toBeTruthy();
  });
});

describe("Dashboard → Current status", () => {
  it("shows no alert banner even with failures — Current status replaced it (CS-1)", async () => {
    feeds.failed = [FAILED, { ...FAILED, traceId: "t-2" }];
    await renderDashboard();

    await screen.findByText("k8s-node-drain");
    // The banner was the only `alert` role on this view.
    expect(screen.queryByRole("alert")).toBeNull();
    // …and nothing re-implements its copy under another role.
    expect(screen.queryByText(/more in the last 24h/)).toBeNull();
  });

  it("names the next scheduled run, with its zoned stamp (CS-4)", async () => {
    feeds.upcoming = [NEXT_UP];
    await renderDashboard();

    await screen.findByText("Up next");
    expect(screen.getByText("disk-usage-audit")).toBeTruthy();
  });

  // VU2-3 — three fires, not one. CS-4's single-fact rule was about the
  // Dashboard not becoming a second Schedules page; three lines answer "and
  // then?" without browsing, and the card still links to Upcoming.
  it("names the next three scheduled runs, soonest first (VU2-3)", async () => {
    feeds.upcoming = [
      NEXT_UP,
      { ownerKind: "job", ownerName: "nightly-db-backup", at: ahead(90 * 60_000) },
      { ownerKind: "workflow", ownerName: "nightly-maintenance", at: ahead(150 * 60_000) },
      { ownerKind: "job", ownerName: "cert-renewal", at: ahead(300 * 60_000) },
    ];
    await renderDashboard();

    await screen.findByText("Up next");
    expect(screen.getByText("disk-usage-audit")).toBeTruthy();
    expect(screen.getByText("nightly-db-backup")).toBeTruthy();
    expect(screen.getByText("nightly-maintenance")).toBeTruthy();
    // Capped at three: the fourth belongs to Upcoming, which the card links to.
    expect(screen.queryByText("cert-renewal")).toBeNull();
    // The workflow tag still rides along — a workflow's projection is not a job's.
    expect(screen.getByText("workflow")).toBeTruthy();
  });

  it("keeps a past instant out of Up next even when the projection carries one", async () => {
    feeds.upcoming = [{ ownerKind: "job", ownerName: "already-fired", at: ago(5 * 60_000) }, NEXT_UP];
    await renderDashboard();

    await screen.findByText("disk-usage-audit");
    expect(screen.queryByText("already-fired")).toBeNull();
  });

  it("says so plainly when nothing is scheduled ahead — and bounds the claim (RX-18)", async () => {
    await renderDashboard();
    // The sentence is deliberately split so "scheduled" can be emphasised: this
    // tile projects INSTANTS, and a reaction has none. Before reactions
    // existed, "nothing is scheduled" and "nothing will run" were the same
    // statement; they are not any more, and an operator reading the first as
    // the second during a change freeze would be wrong in the dangerous
    // direction.
    expect(await screen.findByText(/Nothing is/)).toBeTruthy();
    expect(screen.getByText(/in the next 24 hours\./)).toBeTruthy();
    expect(
      screen.getByText(/Reactions are not projected here/),
    ).toBeTruthy();
  });

  it("states the next run ONCE — the all-clear no longer repeats it (CS-4)", async () => {
    // Both blocks used to carry it, which in adjacent boxes reads as a bug.
    feeds.upcoming = [NEXT_UP];
    await renderDashboard();

    await screen.findByText(/All 2 jobs healthy\./);
    expect(screen.getByText("Nothing has failed or warned in the last 24 hours.")).toBeTruthy();
    expect(screen.getAllByText("disk-usage-audit")).toHaveLength(1);
  });

  it("sends an attention row to that job's History entry, not to every failure (CS-3)", async () => {
    feeds.failed = [FAILED];
    await renderDashboard(true);

    fireEvent.click(await screen.findByText("k8s-node-drain"));

    await waitFor(() => expect(screen.getByTestId("location").textContent).toBe("/runs?job=k8s-node-drain"));
  });

  it("folds a job's repeated failures into one row carrying the count", async () => {
    feeds.failed = [FAILED, { ...FAILED, traceId: "t-2" }, { ...FAILED, traceId: "t-3" }];
    await renderDashboard();

    await screen.findByText("k8s-node-drain");
    expect(screen.getAllByText("k8s-node-drain")).toHaveLength(1);
    expect(screen.getByText("Failed ×3")).toBeTruthy();
  });

  it("keeps warnings in the list, sorted below failures (CS-Q1)", async () => {
    feeds.failed = [FAILED];
    feeds.warned = [{ traceId: "w-1", jobName: "cert-renewal", status: "warning", startedAt: ago(11 * 60_000), completedAt: ago(10 * 60_000) }];
    await renderDashboard();

    const rows = await screen.findAllByTestId("attention-row");
    expect(rows[0].textContent).toContain("k8s-node-drain");
    // Scoped to the row: "Warn" also appears in the Score's legend above.
    expect(rows[1].textContent).toContain("cert-renewal");
    expect(rows[1].textContent).toContain("Warn");
  });
});

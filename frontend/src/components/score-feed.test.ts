import { describe, expect, it } from "vitest";
import { buildScoreMarks, scoreTruncatedAt } from "./score-feed";

// The Score's marks, as the Dashboard and the Operations overview both build
// them (2.4.4 F4).

const t = (iso: string) => Date.parse(iso);

describe("buildScoreMarks", () => {
  it("plots started runs and future fires, named as the caller says", () => {
    const marks = buildScoreMarks(
      [
        { traceId: "a", jobName: "backup", ownerAgency: "ag-a", startedAt: "2026-10-16T10:00:00Z", status: "success", runnerId: "r1" },
        { traceId: "b", jobName: "queued", startedAt: null },
        { traceId: "c", jobName: "probe", kind: "ssh-test", startedAt: "2026-10-16T10:05:00Z" },
      ],
      [{ ownerName: "nightly", ownerKind: "job", at: "2026-10-16T20:00:00Z" }, { ownerName: "never", at: null }],
      new Map([["r1", "agent-1"]]),
      { run: (r) => `${r.jobName} · ${r.ownerAgency}`, upcoming: (u) => `${u.ownerName} next` },
    );
    expect(marks).toEqual([
      expect.objectContaining({ at: t("2026-10-16T10:00:00Z"), tense: "past", jobName: "backup · ag-a", traceId: "a", runnerName: "agent-1" }),
      expect.objectContaining({ at: t("2026-10-16T20:00:00Z"), tense: "future", jobName: "nightly next", ownerKind: "job" }),
    ]);
  });

  it("says the projection was cut only at the cap", () => {
    expect(scoreTruncatedAt([{ at: "2026-10-16T20:00:00Z" }])).toBeNull();
    const full = Array.from({ length: 500 }, (_, i) => ({ at: `2026-10-16T${String(10 + (i % 10)).padStart(2, "0")}:00:00Z` }));
    expect(scoreTruncatedAt(full)).toBe(Date.parse(full[499].at));
  });
});

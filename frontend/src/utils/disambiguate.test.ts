import { describe, it, expect } from "vitest";
import { ambiguousNames, agencySuffix, disambiguate, type NamedRef } from "./disambiguate";

// R2F-3 — the one rule twelve surfaces share: a name earns its agency suffix
// when, and only when, it is ambiguous within the viewer's visible set.

const label = (row: NamedRef, set: readonly NamedRef[]) => disambiguate(row, ambiguousNames(set));

describe("when a name badges", () => {
  const fin: NamedRef = { uid: "u1", name: "deploy", agencies: ["FIN"] };
  const dss: NamedRef = { uid: "u2", name: "deploy", agencies: ["DSS"] };
  const solo: NamedRef = { uid: "u3", name: "build", agencies: ["FIN"] };

  it("leaves a name alone when nothing collides", () => {
    expect(label(solo, [solo, fin])).toBe("build");
  });

  it("badges both sides of a collision, and only them", () => {
    const set = [fin, dss, solo];
    expect(label(fin, set)).toBe("deploy · FIN");
    expect(label(dss, set)).toBe("deploy · DSS");
    expect(label(solo, set)).toBe("build");
  });

  it("treats one definition appearing many times as one definition", () => {
    // The common case on a history page: twenty runs of the same job.
    const runs = [fin, { ...fin }, { ...fin }];
    expect(label(fin, runs)).toBe("deploy");
  });
});

// The no-oracle property, which is a security property and not a nicety: the
// visible set is what the backend already filtered by scope, so a badge can
// never tell one department that another has a definition by this name.
describe("no oracle", () => {
  it("does not badge when the twin is outside the visible set", () => {
    const mine: NamedRef = { uid: "u1", name: "deploy", agencies: ["FIN"] };
    const theirs: NamedRef = { uid: "u2", name: "deploy", agencies: ["DSS"] };
    // Filtered out before it ever reached the client.
    expect(label(mine, [mine])).toBe("deploy");
    // And with it present — the case an unrestricted admin sees — it badges.
    expect(label(mine, [mine, theirs])).toBe("deploy · FIN");
  });
});

// The honesty property: a row with no identity cannot be attributed, so it
// neither badges nor causes badging.
describe("rows without an identity", () => {
  const legacy: NamedRef = { uid: null, name: "deploy", agencies: ["FIN"] };
  const current: NamedRef = { uid: "u1", name: "deploy", agencies: ["DSS"] };

  it("never badges a uid-less row", () => {
    expect(label(legacy, [legacy, current])).toBe("deploy");
  });

  it("does not let a uid-less row make another row badge", () => {
    // Two rows, one name — but only ONE identity is actually known, so there is
    // no evidence that two definitions exist.
    expect(label(current, [legacy, current])).toBe("deploy");
  });
});

// Surfaces that mix kinds (the reaction picker, the activity feed) must not let
// a job and a workflow of one name badge each other: the "kind ·" prefix beside
// them already tells those apart.
describe("groups", () => {
  const job: NamedRef = { uid: "u1", name: "nightly", agencies: ["FIN"], group: "job" };
  const wf: NamedRef = { uid: "u2", name: "nightly", agencies: ["DSS"], group: "workflow" };
  const otherJob: NamedRef = { uid: "u3", name: "nightly", agencies: ["DSS"], group: "job" };

  it("does not badge across groups", () => {
    expect(label(job, [job, wf])).toBe("nightly");
    expect(label(wf, [job, wf])).toBe("nightly");
  });

  it("still badges within a group", () => {
    expect(label(job, [job, wf, otherJob])).toBe("nightly · FIN");
    expect(label(otherJob, [job, wf, otherJob])).toBe("nightly · DSS");
    expect(label(wf, [job, wf, otherJob])).toBe("nightly");
  });
});

describe("the suffix itself (R2F-Q1)", () => {
  it("caps at two agencies, then counts", () => {
    expect(agencySuffix([])).toBe("");
    expect(agencySuffix(undefined)).toBe("");
    expect(agencySuffix(["FIN"])).toBe(" · FIN");
    expect(agencySuffix(["FIN", "DSS"])).toBe(" · FIN/DSS");
    expect(agencySuffix(["FIN", "DSS", "ENG"])).toBe(" · 3 agencies");
  });

  it("falls back to the source when colliding rows have no agencies", () => {
    const git: NamedRef = { uid: "u1", name: "deploy", source: "git" };
    const ama: NamedRef = { uid: "u2", name: "deploy", source: "cronomicon" };
    expect(label(git, [git, ama])).toBe("deploy · git");
    expect(label(ama, [git, ama])).toBe("deploy · cronomicon");
  });

  it("shows the bare name when there is nothing at all to qualify with", () => {
    // Two identities, one name, no agencies and no sources: a badge would be
    // decoration, and two identical labels are no worse than one wrong one.
    const a: NamedRef = { uid: "u1", name: "deploy" };
    const b: NamedRef = { uid: "u2", name: "deploy" };
    expect(label(a, [a, b])).toBe("deploy");
  });
});

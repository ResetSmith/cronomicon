import { describe, it, expect, beforeEach } from "vitest";
import {
  newJobNode,
  newParallelNode,
  newBranchNode,
  newNode,
  defaultInputKey,
  insertNode,
  appendNode,
  removeNode,
  updateNode,
  moveNode,
  withFreshIds,
  freshId,
  __resetIds,
  type LaneRef,
} from "./canvasEdit";
import { toSteps, type CanvasNode } from "./canvasModel";

const ROOT: LaneRef = { containerId: null, which: "root" };

beforeEach(() => __resetIds(0));

describe("canvasEdit — factories & ids", () => {
  it("creates the right kind with a fresh id", () => {
    expect(newJobNode("build").kind).toBe("job");
    expect(newJobNode("build").name).toBe("build");
    expect(newParallelNode().kind).toBe("parallel");
    expect(newBranchNode().kind).toBe("branch");
    expect(newNode("branch").kind).toBe("branch");
  });
  it("mints unique ids", () => {
    const ids = [freshId(), freshId(), newJobNode().id, newParallelNode().id];
    expect(new Set(ids).size).toBe(ids.length);
  });
  it("a new branch has both arms and a default condition", () => {
    const b = newBranchNode();
    expect(b.pass).toEqual([]);
    expect(b.fail).toEqual([]);
    expect(b.condition).toMatchObject({ type: "job_status", operator: "==" });
  });
});

describe("canvasEdit — defaultInputKey (WC-P4 connect seed)", () => {
  it("derives an UPPER_SNAKE key from the producer name", () => {
    expect(defaultInputKey("alpha-job-01", [])).toBe("ALPHA_JOB_01");
    expect(defaultInputKey("build", [])).toBe("BUILD");
    expect(defaultInputKey("db.backup step", [])).toBe("DB_BACKUP_STEP");
  });
  it("is never blank (so the input draws its edge and survives save)", () => {
    expect(defaultInputKey("", [])).toBe("INPUT");
    expect(defaultInputKey("---", [])).toBe("INPUT");
    expect(defaultInputKey("alpha-job-01", []).length).toBeGreaterThan(0);
  });
  it("suffixes to stay unique against the consumer's existing keys", () => {
    expect(defaultInputKey("build", ["BUILD"])).toBe("BUILD_2");
    expect(defaultInputKey("build", ["BUILD", "BUILD_2"])).toBe("BUILD_3");
  });
});

describe("canvasEdit — insert / append", () => {
  it("inserts into the root lane at an index", () => {
    let tree: CanvasNode[] = [newJobNode("a"), newJobNode("c")];
    tree = insertNode(tree, ROOT, 1, newJobNode("b"));
    expect(tree.map((n) => n.name)).toEqual(["a", "b", "c"]);
  });
  it("appends into a parallel block's jobs", () => {
    const par = newParallelNode();
    let tree: CanvasNode[] = [par];
    tree = appendNode(tree, { containerId: par.id, which: "jobs" }, newJobNode("unit"));
    tree = appendNode(tree, { containerId: par.id, which: "jobs" }, newJobNode("integration"));
    expect(tree[0].jobs?.map((j) => j.name)).toEqual(["unit", "integration"]);
  });
  it("inserts into a branch arm (pass/fail)", () => {
    const br = newBranchNode();
    let tree: CanvasNode[] = [br];
    tree = appendNode(tree, { containerId: br.id, which: "pass" }, newJobNode("deploy"));
    tree = appendNode(tree, { containerId: br.id, which: "fail" }, newJobNode("notify"));
    expect(tree[0].pass?.[0].name).toBe("deploy");
    expect(tree[0].fail?.[0].name).toBe("notify");
  });
  it("clamps an out-of-range index and is a no-op for an unknown container", () => {
    const tree: CanvasNode[] = [newJobNode("a")];
    expect(insertNode(tree, ROOT, 99, newJobNode("z")).map((n) => n.name)).toEqual(["a", "z"]);
    expect(insertNode(tree, { containerId: "nope", which: "jobs" }, 0, newJobNode("z"))).toBe(tree);
  });
});

describe("canvasEdit — remove (cascade)", () => {
  it("removes a top-level node", () => {
    const a = newJobNode("a");
    const b = newJobNode("b");
    expect(removeNode([a, b], a.id).map((n) => n.name)).toEqual(["b"]);
  });
  it("removes a nested arm node", () => {
    const br = newBranchNode();
    const deploy = newJobNode("deploy");
    const tree = appendNode([br], { containerId: br.id, which: "pass" }, deploy);
    const after = removeNode(tree, deploy.id);
    expect(after[0].pass).toEqual([]);
  });
  it("cascades a container's whole subtree", () => {
    const par = newParallelNode();
    let tree = appendNode([newJobNode("a"), par], { containerId: par.id, which: "jobs" }, newJobNode("x"));
    tree = removeNode(tree, par.id);
    expect(tree.map((n) => n.name ?? n.kind)).toEqual(["a"]);
  });
});

describe("canvasEdit — update", () => {
  it("patches a job node's fields anywhere in the tree", () => {
    const br = newBranchNode();
    const deploy = newJobNode("deploy");
    const tree = appendNode([br], { containerId: br.id, which: "pass" }, deploy);
    const after = updateNode(tree, deploy.id, { retries: 3, continueOnError: true });
    expect(after[0].pass?.[0]).toMatchObject({ name: "deploy", retries: 3, continueOnError: true });
  });
  it("patches a branch condition", () => {
    const br = newBranchNode();
    const after = updateNode([br], br.id, { condition: { type: "job_status", jobRef: "build", field: "", operator: "==", value: "" } });
    expect(after[0].condition?.jobRef).toBe("build");
  });
});

describe("canvasEdit — move", () => {
  it("swaps siblings up/down and clamps at the boundary", () => {
    const a = newJobNode("a");
    const b = newJobNode("b");
    const c = newJobNode("c");
    expect(moveNode([a, b, c], b.id, -1).map((n) => n.name)).toEqual(["b", "a", "c"]);
    expect(moveNode([a, b, c], b.id, 1).map((n) => n.name)).toEqual(["a", "c", "b"]);
    expect(moveNode([a, b, c], a.id, -1).map((n) => n.name)).toEqual(["a", "b", "c"]); // no-op at top
  });
  it("moves within a nested arm", () => {
    const br = newBranchNode();
    let tree = appendNode([br], { containerId: br.id, which: "fail" }, newJobNode("x"));
    const y = newJobNode("y");
    tree = appendNode(tree, { containerId: br.id, which: "fail" }, y);
    tree = moveNode(tree, y.id, -1);
    expect(tree[0].fail?.map((j) => j.name)).toEqual(["y", "x"]);
  });
});

describe("canvasEdit — immutability & ids", () => {
  it("never mutates the input tree", () => {
    const a = newJobNode("a");
    const tree: CanvasNode[] = [a];
    const snapshot = JSON.stringify(tree);
    insertNode(tree, ROOT, 0, newJobNode("z"));
    removeNode(tree, a.id);
    updateNode(tree, a.id, { name: "changed" });
    moveNode(tree, a.id, 1);
    expect(JSON.stringify(tree)).toBe(snapshot);
  });
  it("withFreshIds re-keys every node uniquely and preserves structure + names", () => {
    const original: CanvasNode[] = [
      { id: "s0", kind: "job", name: "build" },
      {
        id: "s1",
        kind: "branch",
        condition: { type: "job_status", jobRef: "build", field: "", operator: "==", value: "" },
        pass: [{ id: "s1.pass.0", kind: "job", name: "deploy" }],
        fail: [{ id: "s1.fail.0", kind: "job", name: "notify" }],
      },
    ];
    const fresh = withFreshIds(original);
    const ids: string[] = [];
    const collect = (t: CanvasNode[]) => t.forEach((n) => { ids.push(n.id); if (n.kind === "branch") { collect(n.pass ?? []); collect(n.fail ?? []); } if (n.kind === "parallel") collect(n.jobs ?? []); });
    collect(fresh);
    expect(new Set(ids).size).toBe(ids.length); // all unique
    expect(ids.every((id) => !id.startsWith("s"))).toBe(true); // re-keyed off the path ids
    // structure round-trips identically through the serializer
    expect(toSteps(fresh)).toEqual(toSteps(original));
  });
});

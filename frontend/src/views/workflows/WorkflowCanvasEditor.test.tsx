// @vitest-environment jsdom
import { describe, it, expect, vi, beforeAll, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { WorkflowCanvasEditor, type JobOption } from "./WorkflowCanvasEditor";
import { newJobNode, newBranchNode, appendNode, updateNode, __resetIds } from "./canvasEdit";
import type { CanvasNode } from "./canvasModel";

// React Flow (rendered by the embedded WorkflowCanvas preview) needs a couple of DOM
// APIs jsdom doesn't ship. This is a mount-without-throwing smoke, not a layout test.
beforeAll(() => {
  class RO {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  vi.stubGlobal("ResizeObserver", RO);
  if (!("DOMMatrixReadOnly" in globalThis)) {
    class DOMMatrixReadOnly {
      m22 = 1;
    }
    vi.stubGlobal("DOMMatrixReadOnly", DOMMatrixReadOnly);
  }
});
afterEach(cleanup);

const JOBS: JobOption[] = [{ name: "build" }, { name: "deploy" }, { name: "notify" }, { name: "cleanup" }];

// build → branch(build){ pass: deploy | fail: notify } — a valid nested graph.
function sampleTree(): CanvasNode[] {
  __resetIds(0);
  let tree: CanvasNode[] = [newJobNode("build")];
  const br = newBranchNode();
  tree = [...tree, br];
  tree = appendNode(tree, { containerId: br.id, which: "pass" }, newJobNode("deploy"));
  tree = appendNode(tree, { containerId: br.id, which: "fail" }, newJobNode("notify"));
  return updateNode(tree, br.id, { condition: { type: "job_status", jobRef: "build", field: "", operator: "==", value: "" } });
}

describe("WorkflowCanvasEditor (jsdom smoke)", () => {
  it("mounts a nested tree without throwing and renders the canvas + lane controls", () => {
    render(<WorkflowCanvasEditor value={sampleTree()} onChange={() => {}} jobs={JOBS} />);
    // The embedded React Flow canvas mounted (WorkflowCanvas's role="img" container).
    expect(screen.getByRole("img", { name: /workflow graph/i })).toBeTruthy();
    // Validation summary + per-lane toolbars are present.
    expect(screen.getByText("✓ Valid graph")).toBeTruthy();
    expect(screen.getAllByText("+ Job").length).toBeGreaterThan(0);
    expect(screen.getAllByText("+ Branch").length).toBeGreaterThan(0);
  });

  it("emits a longer tree when a lane's '+ Job' is clicked (controlled edit)", () => {
    const onChange = vi.fn();
    render(<WorkflowCanvasEditor value={sampleTree()} onChange={onChange} jobs={JOBS} />);
    const addJob = screen.getAllByText("+ Job");
    fireEvent.click(addJob[addJob.length - 1]); // the root lane's toolbar renders last
    expect(onChange).toHaveBeenCalledTimes(1);
    const next = onChange.mock.calls[0][0] as CanvasNode[];
    expect(next.length).toBe(3); // build, branch, + the new job
    expect(next[2].kind).toBe("job");
  });

  it("surfaces validation issues on an invalid tree", () => {
    __resetIds(100);
    render(<WorkflowCanvasEditor value={[newJobNode("")]} onChange={() => {}} jobs={JOBS} />);
    expect(screen.getByText(/issue/i)).toBeTruthy(); // "1 issue — see the highlighted steps above"
  });

  it("shows an empty-state hint when there are no steps (WC-P5)", () => {
    render(<WorkflowCanvasEditor value={[]} onChange={() => {}} jobs={JOBS} />);
    expect(screen.getByText(/No steps yet/i)).toBeTruthy();
  });

  it("labels node action buttons for keyboard / assistive tech (WC-P5)", () => {
    render(<WorkflowCanvasEditor value={sampleTree()} onChange={() => {}} jobs={JOBS} />);
    expect(screen.getAllByLabelText(/Delete .* step/i).length).toBeGreaterThan(0);
    expect(screen.getAllByLabelText(/Move .* (up|down)/i).length).toBeGreaterThan(0);
  });
});

// WorkflowCanvas.tsx — WC-P2: a READ-ONLY React Flow render of a workflow's steps.
// Derives the flow graph from the canvas tree (canvasModel + canvasFlow), lays it
// out top-down with dagre, and draws it with React Flow (pan/zoom/minimap for free).
// Read-only here; structural editing lands in WC-P3+. Theme-reactive: every color is
// read from `c.*` at render (never frozen in a module const — the Light-Mode gotcha);
// dagre computes only geometry, which is theme-independent and memoized on `steps`.
import "@xyflow/react/dist/style.css";
import { useEffect, useMemo, type CSSProperties } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MiniMap,
  Handle,
  Position,
  MarkerType,
  useNodesState,
  type Node,
  type Edge,
  type NodeProps,
} from "@xyflow/react";
import dagre from "@dagrejs/dagre";
import { c } from "../../theme";
import { statusTone } from "../../components/ui";
import { toCanvasTree, type DefStep, type LayoutMap } from "./canvasModel";
import { treeToFlow, isStructuralEdge, type FlowEdgeKind, type FlowNodeKind } from "./canvasFlow";

// Node footprints (used by dagre AND the node components, so they stay in sync).
const SIZE: Record<FlowNodeKind, { w: number; h: number }> = {
  start: { w: 72, h: 30 },
  end: { w: 72, h: 30 },
  job: { w: 158, h: 42 },
  fork: { w: 128, h: 24 },
  join: { w: 30, h: 16 },
  condition: { w: 168, h: 46 },
  // SW: wider than a job node — it carries both the step name and the
  // sub-workflow it runs, and a footprint that lies makes dagre overlap it.
  subworkflow: { w: 178, h: 48 },
};

const MINIMAP_THRESHOLD = 14; // show the minimap once the graph gets busy

interface PositionedNode {
  id: string;
  kind: FlowNodeKind;
  label: string;
  sublabel?: string;
  canvasId?: string;
  x: number;
  y: number;
}

// A stable empty-layout reference so read-only callers (no `layout` prop) don't
// invalidate the geometry memo every render.
const NO_LAYOUT: LayoutMap = {};

/** Pure: steps → flow graph → dagre positions, with `saved` hand-arranged positions
 *  (WC-P7) overriding dagre per node id. Geometry only (no colors), so it is safe to
 *  memoize on `steps`+`saved` without freezing the theme. */
function computeGeometry(steps: DefStep[], saved: LayoutMap): { nodes: PositionedNode[]; edges: ReturnType<typeof treeToFlow>["edges"] } {
  const { nodes, edges } = treeToFlow(toCanvasTree(steps));
  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "TB", nodesep: 26, ranksep: 46, marginx: 18, marginy: 18 });
  g.setDefaultEdgeLabel(() => ({}));
  for (const n of nodes) g.setNode(n.id, { width: SIZE[n.kind].w, height: SIZE[n.kind].h });
  for (const e of edges) if (isStructuralEdge(e.kind)) g.setEdge(e.source, e.target); // A12 overlay excluded from ranking
  dagre.layout(g);
  const positioned = nodes.map((n) => {
    const pin = saved[n.id];
    if (pin) return { ...n, x: pin.x, y: pin.y }; // hand-arranged position wins over dagre
    const p = g.node(n.id) as { x: number; y: number } | undefined;
    const s = SIZE[n.kind];
    return { ...n, x: (p?.x ?? 0) - s.w / 2, y: (p?.y ?? 0) - s.h / 2 };
  });
  return { nodes: positioned, edges };
}

/** One PositionedNode → a React Flow node. Draggable only in the arrange (WC-P7) mode.
 *  `status` (WC-R1, run views) rides in data so JobNode can tint by outcome. */
function toRfNode(n: PositionedNode, arrangeable: boolean, status?: string): Node {
  return {
    id: n.id,
    type: n.kind,
    position: { x: n.x, y: n.y },
    data: { label: n.label, sublabel: n.sublabel, status },
    draggable: arrangeable,
    selectable: false,
    connectable: false,
  };
}

// ── Edge colors (read at render — theme-reactive) ─────────────────────────────────

function edgeColor(kind: FlowEdgeKind): string {
  switch (kind) {
    case "pass":
      return c.success;
    case "fail":
      return c.danger;
    case "data":
      return c.accent;
    default:
      return c.textSec;
  }
}

// ── Custom node components (each reads `c.*` at render) ───────────────────────────

const hiddenHandle = { width: 6, height: 6, background: "transparent", border: "none", opacity: 0 } as const;

function JobNode({ data, isConnectable }: NodeProps) {
  const d = data as { label?: string; status?: string };
  // In connect mode (WC-P4 A12 wiring) the handles become visible grab targets; in
  // read-only mode they stay invisible (they only anchor the structural edges).
  const hs: CSSProperties = isConnectable ? { width: 9, height: 9, background: c.accent, border: `1.5px solid ${c.panel}` } : hiddenHandle;
  // WC-R1: on a run view each job node is tinted by its child run's outcome (the
  // same statusTone vocabulary as every badge); definition views carry no status
  // and keep the neutral primary tint.
  const clr = d.status ? statusTone(d.status).color : c.primary;
  return (
    <div
      style={{
        width: SIZE.job.w,
        height: SIZE.job.h,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        gap: 6,
        padding: "0 10px",
        boxSizing: "border-box",
        background: `${clr}14`,
        border: `1px solid ${clr}55`,
        borderRadius: c.radiusSurface,
        color: c.text,
        fontSize: c.fontSm,
        fontWeight: 600,
        overflow: "hidden",
        whiteSpace: "nowrap",
        textOverflow: "ellipsis",
        opacity: d.status === "skipped" ? 0.55 : 1,
      }}
      title={d.status ? `${d.label} — ${d.status}` : d.label}
    >
      <Handle type="target" position={Position.Top} style={hs} isConnectable={isConnectable} />
      <span style={{ width: 6, height: 6, borderRadius: "50%", background: clr, flexShrink: 0 }} />
      <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>{d.label}</span>
      <Handle type="source" position={Position.Bottom} style={hs} isConnectable={isConnectable} />
    </div>
  );
}

function ConditionNode({ data }: NodeProps) {
  const d = data as { label?: string };
  return (
    <div
      style={{
        width: SIZE.condition.w,
        height: SIZE.condition.h,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        padding: "0 12px",
        boxSizing: "border-box",
        background: `${c.warning}1c`,
        border: `1px solid ${c.warning}66`,
        borderRadius: c.radiusSurface,
        color: c.warning,
        fontSize: c.fontXs,
        fontWeight: 700,
        textAlign: "center",
        overflow: "hidden",
      }}
      title={d.label}
    >
      <Handle type="target" position={Position.Top} style={hiddenHandle} isConnectable={false} />
      <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>◆ {d.label}</span>
      <Handle type="source" position={Position.Bottom} style={hiddenHandle} isConnectable={false} />
    </div>
  );
}

function ForkNode({ data }: NodeProps) {
  const d = data as { sublabel?: string };
  return (
    <div
      style={{
        width: SIZE.fork.w,
        height: SIZE.fork.h,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        boxSizing: "border-box",
        background: c.panel,
        border: `1px dashed ${c.border}`,
        borderRadius: c.radiusChip,
        color: c.textSec,
        fontSize: c.fontXs,
        fontWeight: 700,
        letterSpacing: 0.4,
        textTransform: "uppercase",
      }}
    >
      <Handle type="target" position={Position.Top} style={hiddenHandle} isConnectable={false} />
      {d.sublabel || "∥"}
      <Handle type="source" position={Position.Bottom} style={hiddenHandle} isConnectable={false} />
    </div>
  );
}

function JoinNode() {
  return (
    <div
      style={{
        width: SIZE.join.w,
        height: SIZE.join.h,
        boxSizing: "border-box",
        background: c.panel,
        border: `1px dashed ${c.border}`,
        borderRadius: c.radiusChip,
      }}
    >
      <Handle type="target" position={Position.Top} style={hiddenHandle} isConnectable={false} />
      <Handle type="source" position={Position.Bottom} style={hiddenHandle} isConnectable={false} />
    </div>
  );
}

function AnchorNode({ data }: NodeProps) {
  const d = data as { label?: string };
  return (
    <div
      style={{
        width: SIZE.start.w,
        height: SIZE.start.h,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        boxSizing: "border-box",
        background: c.panel2,
        border: `1px solid ${c.border}`,
        // Start/end are flowchart TERMINATORS: the stadium outline is what tells
        // them apart from a job node, so this is diagram semantics, not chrome.
        // radiusPill keeps that shape exactly; a surface radius would flatten it
        // into another rectangle (VU-6 — the one non-status use of the pill).
        borderRadius: c.radiusPill,
        color: c.textSec,
        fontSize: c.fontXs,
        fontWeight: 700,
        textTransform: "uppercase",
        letterSpacing: 0.5,
      }}
    >
      <Handle type="target" position={Position.Top} style={hiddenHandle} isConnectable={false} />
      {d.label}
      <Handle type="source" position={Position.Bottom} style={hiddenHandle} isConnectable={false} />
    </div>
  );
}

// Stable nodeTypes reference (React Flow warns if recreated each render). The
// components themselves read `c.*` at render, so the palette stays theme-reactive.
const NODE_TYPES = { start: AnchorNode, end: AnchorNode, job: JobNode, fork: ForkNode, join: JoinNode, condition: ConditionNode };

export function WorkflowCanvas({
  steps,
  height = 460,
  connectable = false,
  onConnectEdge,
  isValidEdge,
  layout = NO_LAYOUT,
  onLayoutChange,
  statusById,
}: {
  steps: DefStep[];
  height?: number;
  /** WC-P4: enable A12 handle-drawing. Job nodes expose grab handles; dragging
   *  producer → consumer fires `onConnectEdge`, gated live by `isValidEdge`. */
  connectable?: boolean;
  onConnectEdge?: (source: string, target: string) => void;
  isValidEdge?: (source: string, target: string) => boolean;
  /** WC-P7: advisory hand-arranged node positions to restore over dagre. */
  layout?: LayoutMap;
  /** WC-P7: when provided, nodes become draggable; a drag commits the moved node's
   *  new position into a fresh LayoutMap. Presence of this callback = arrange mode. */
  onLayoutChange?: (next: LayoutMap) => void;
  /** WC-R1: run-view status per canvas path id (graphView.normalizeGraph). Job
   *  nodes tint by outcome; absent ⇒ the neutral definition rendering. */
  statusById?: Record<string, string>;
}) {
  const arrangeable = !!onLayoutChange;
  const geometry = useMemo(() => computeGeometry(steps, layout), [steps, layout]);

  // React Flow runs controlled: nodes are re-derived from geometry (steps + saved
  // layout) and re-synced whenever it changes. In arrange mode `onNodesChange` also
  // absorbs live drag moves; `onNodeDragStop` persists the final position upward.
  // statusById is in the resync deps: a live run's poll refreshes node tints.
  const [rfNodes, setRfNodes, onNodesChange] = useNodesState(geometry.nodes.map((n) => toRfNode(n, arrangeable, statusById?.[n.id])));
  useEffect(() => {
    setRfNodes(geometry.nodes.map((n) => toRfNode(n, arrangeable, statusById?.[n.id])));
  }, [geometry, arrangeable, statusById, setRfNodes]);

  const rfEdges: Edge[] = geometry.edges.map((e) => {
    const col = edgeColor(e.kind);
    return {
      id: e.id,
      source: e.source,
      target: e.target,
      type: e.kind === "data" ? "default" : "smoothstep",
      label: e.label,
      style: { stroke: col, strokeWidth: 1.5, ...(e.kind === "data" ? { strokeDasharray: "5 4" } : {}) },
      markerEnd: { type: MarkerType.ArrowClosed, color: col, width: 15, height: 15 },
      labelStyle: { fill: c.textSec, fontSize: c.fontXs, fontWeight: 600 },
      labelBgStyle: { fill: c.panel, fillOpacity: 0.85 },
      labelBgPadding: [3, 2] as [number, number],
    };
  });

  const jobCount = geometry.nodes.filter((n) => n.kind === "job").length;

  return (
    <div
      role="img"
      aria-label={`Workflow graph — ${jobCount} job step${jobCount === 1 ? "" : "s"}, top to bottom`}
      style={{ height, border: `1px solid ${c.border}`, borderRadius: c.radiusSurface, background: c.panel2, overflow: "hidden" }}
    >
      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        nodeTypes={NODE_TYPES}
        onNodesChange={onNodesChange}
        fitView
        fitViewOptions={{ padding: 0.2 }}
        minZoom={0.2}
        maxZoom={1.6}
        nodesDraggable={arrangeable}
        nodesConnectable={connectable}
        elementsSelectable={false}
        panOnScroll={false}
        onNodeDragStop={arrangeable && onLayoutChange ? (_e, node) => onLayoutChange({ ...layout, [node.id]: { x: Math.round(node.position.x), y: Math.round(node.position.y) } }) : undefined}
        onConnect={connectable && onConnectEdge ? (conn) => conn.source && conn.target && onConnectEdge(conn.source, conn.target) : undefined}
        isValidConnection={connectable && isValidEdge ? (conn) => !!conn.source && !!conn.target && isValidEdge(conn.source, conn.target) : undefined}
        proOptions={{ hideAttribution: false }}
      >
        <Background color={c.border} gap={18} size={1} />
        <Controls showInteractive={false} position="bottom-right" />
        {rfNodes.length > MINIMAP_THRESHOLD && (
          // Theme the minimap by hand: xyflow's default is a white card, which
          // reads as a hole in the dark theme. Props are read at render, so the
          // palette follows a theme toggle like every other c.* consumer.
          <MiniMap
            pannable
            zoomable
            position="top-right"
            style={{ width: 130, height: 92 }}
            bgColor={c.panel}
            maskColor={`${c.panel2}b8`}
            nodeColor={() => `${c.primary}66`}
            nodeStrokeColor={() => c.border}
          />
        )}
      </ReactFlow>
    </div>
  );
}

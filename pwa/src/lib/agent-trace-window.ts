import type { AgentTraceItem, AgentTracePage } from "./operations";
import { ProtocolError } from "./protocol/errors";

export const MAX_CURSOR_TRACE_PAGES = 32;

export type AgentTracePresentationPolicy = "disjoint" | "owning-user";

export type AgentTracePaneMetadata = {
  agent: string;
  workspaceId?: string;
  tabId?: string;
  cwd: string;
};

export type CursorTraceWindow = {
  items: AgentTraceItem[];
  nextCursor: string | null;
  pageDepth: number;
  signature: string;
  tail: number;
  truncated: boolean;
  seenCursors: string[];
};

const MAX_PENDING_ALIGNMENT_CANDIDATES = 64;

export function agentTracePresentationPolicy(
  agent: Pick<AgentTracePaneMetadata, "agent"> | null | undefined,
): AgentTracePresentationPolicy {
  return agent?.agent === "Cursor ACP" ? "disjoint" : "owning-user";
}

/** Labels and status are intentionally absent: they may change without changing the source pane. */
export function agentTracePaneIdentity(agent: AgentTracePaneMetadata | null | undefined): string {
  if (!agent) return "";
  return JSON.stringify([agent.agent, agent.workspaceId || "", agent.tabId || "", agent.cwd]);
}

function fingerprint(items: AgentTraceItem[]): string {
  return JSON.stringify(items);
}

/** Output and streaming prose may grow without changing the source event position. */
export function sameAgentTracePosition(before: AgentTraceItem, after: AgentTraceItem): boolean {
  if (before.type !== after.type) return false;
  if (before.type === "tool" && after.type === "tool") {
    return before.name === after.name && before.input === after.input && before.text === after.text;
  }
  const left = before.text || "";
  const right = after.text || "";
  return left === right || ((before.type === "assistant" || before.type === "thinking") && right.startsWith(left));
}

/**
 * Locate the end of the retained optimistic baseline after a disjoint window
 * shifts or prepends. Ambiguous single-event matches cannot prove a new echo.
 */
export function cursorPendingBoundary(
  baseline: AgentTraceItem[],
  items: AgentTraceItem[],
  pendingText = "",
  completeWindow = false,
): number {
  if (!baseline.length) return 0;
  if (!items.length) return 0;
  let bestScore = 0;
  let bestBoundary = items.length;
  let candidates = 0;
  const consider = (offset: number): void => {
    if (candidates >= MAX_PENDING_ALIGNMENT_CANDIDATES) return;
    candidates += 1;
    const baselineStart = Math.max(0, -offset);
    const itemStart = Math.max(0, offset);
    const overlap = Math.min(baseline.length - baselineStart, items.length - itemStart);
    if (overlap <= 0) return;
    for (let index = 0; index < overlap; index += 1) {
      if (!sameAgentTracePosition(baseline[baselineStart + index], items[itemStart + index])) return;
    }
    const boundary = Math.min(items.length, Math.max(0, offset + baseline.length));
    if (overlap > bestScore || (overlap === bestScore && boundary > bestBoundary)) {
      bestScore = overlap;
      bestBoundary = boundary;
    }
  };

  consider(0);
  for (let index = 1; index < baseline.length && candidates < MAX_PENDING_ALIGNMENT_CANDIDATES; index += 1) {
    if (sameAgentTracePosition(baseline[index], items[0])) consider(-index);
  }
  for (let index = 1; index < items.length && candidates < MAX_PENDING_ALIGNMENT_CANDIDATES; index += 1) {
    if (sameAgentTracePosition(baseline[0], items[index])) consider(index);
  }
  if (bestScore >= 2) return bestBoundary;
  const anchor = baseline.length === 1 ? baseline[0] : undefined;
  const uniqueSamePositionAnchor = completeWindow && anchor?.type === "user" &&
    anchor.text !== pendingText && sameAgentTracePosition(anchor, items[0]) &&
    items.filter((item) => sameAgentTracePosition(anchor, item)).length === 1;
  return uniqueSamePositionAnchor ? 1 : items.length;
}

function flattenNewestFirst(pages: AgentTracePage[]): AgentTraceItem[] {
  const items: AgentTraceItem[] = [];
  for (let index = pages.length - 1; index >= 0; index -= 1) items.push(...pages[index].items);
  return items;
}

function needsOlderOwningUser(items: AgentTraceItem[], nextCursor: string | null): boolean {
  if (!nextCursor || !items.length) return false;
  return items[0]?.type !== "user";
}

/**
 * Re-read a bounded, contiguous Cursor chain. Nothing is returned until every
 * requested page succeeds, so callers can replace the visible window once.
 */
export async function rebuildCursorTraceWindow(
  read: (cursor: string | null) => Promise<AgentTracePage>,
  previousDepth: number,
  isCurrent: () => boolean = () => true,
): Promise<CursorTraceWindow | null> {
  const targetDepth = Math.min(MAX_CURSOR_TRACE_PAGES, Math.max(1, Math.floor(previousDepth) || 1));
  const pages: AgentTracePage[] = [];
  const seen = new Set<string>();
  const requestedCursors: string[] = [];
  let cursor: string | null = null;
  let nextCursor: string | null = null;

  const pull = async (): Promise<boolean> => {
    if (cursor) requestedCursors.push(cursor);
    const page = await read(cursor);
    if (!isCurrent()) return false;
    pages.push(page);
    nextCursor = page.nextCursor;
    if (!nextCursor) return true;
    if (seen.has(nextCursor)) {
      throw new ProtocolError("bad_message", "AgentTrace cursor repeated");
    }
    seen.add(nextCursor);
    cursor = nextCursor;
    return true;
  };

  while (pages.length < targetDepth) {
    if (!(await pull())) return null;
    if (!nextCursor) break;
  }

  let items = flattenNewestFirst(pages);
  if (
    pages.length < MAX_CURSOR_TRACE_PAGES &&
    needsOlderOwningUser(items, nextCursor) &&
    nextCursor
  ) {
    if (!(await pull())) return null;
    items = flattenNewestFirst(pages);
  }

  return {
    items,
    nextCursor,
    pageDepth: pages.length,
    signature: fingerprint(pages[0]?.items ?? []),
    tail: pages[0]?.items.length ?? 0,
    truncated: pages.some((page) => page.truncated),
    seenCursors: requestedCursors,
  };
}

/** Exact disjoint pagination: the older source page precedes the current chain. */
export function prependCursorTracePage(
  current: CursorTraceWindow,
  page: AgentTracePage,
  requestedCursor: string,
): CursorTraceWindow {
  const repeated = page.nextCursor !== null && (
    page.nextCursor === requestedCursor || current.seenCursors.includes(page.nextCursor)
  );
  if (repeated) throw new ProtocolError("bad_message", "AgentTrace cursor repeated");
  return {
    items: [...page.items, ...current.items],
    nextCursor: page.nextCursor,
    pageDepth: current.pageDepth + 1,
    signature: current.signature,
    tail: current.tail,
    truncated: current.truncated || page.truncated,
    seenCursors: [...current.seenCursors, requestedCursor],
  };
}

import { forgetAgentTrace, cacheAgentTrace, cachedAgentTrace } from "../lib/agent-trace-cache";
import {
  agentTracePaneIdentity,
  agentTracePresentationPolicy,
  prependCursorTracePage,
  rebuildCursorTraceWindow,
  type AgentTracePresentationPolicy,
  type CursorTraceWindow,
} from "../lib/agent-trace-window";
import { firstTurnNeedsUser, mergeAgentTraceSegments } from "../lib/agent-trace-view";
import type { AgentTraceItem, AgentTracePage } from "../lib/operations";
import { messageOf } from "../lib/notices";
import { ProtocolError } from "../lib/protocol/errors";
import { track } from "../lib/telemetry";
import { render } from "../paint";
import { selectedAgent, state } from "../state";

const TRACE_PAGE = 200;
const OLDER_FILL_MAX = 4;
let traceRequest = 0;

type TraceBinding = { policy: AgentTracePresentationPolicy; identity: string };

export type AgentTraceRefreshUI = {
  absorbPending: (items: AgentTraceItem[]) => void;
  atBottom: (element: HTMLElement) => boolean;
  patch: (options?: { follow?: boolean; older?: boolean; top?: number; height?: number }) => boolean;
  stream: () => HTMLElement | null;
  syncOlder: () => void;
  unavailableNote: () => string;
};

function fingerprint(items: AgentTraceItem[]): string {
  return JSON.stringify(items);
}

function bindingNow(): TraceBinding | null {
  const selected = selectedAgent();
  if (!selected) return null;
  return {
    policy: agentTracePresentationPolicy(selected),
    identity: agentTracePaneIdentity(selected),
  };
}

function clearTraceView(): void {
  state.agentTraceItems = [];
  state.agentTraceNext = null;
  state.agentTraceNote = "";
  state.agentTraceTruncated = false;
  state.agentTraceSig = "";
  state.agentTraceLoadState = "cold";
  state.agentTraceTail = 0;
  state.agentTracePageDepth = 0;
  state.agentTraceSeenCursors = [];
  state.agentTracePending = "";
  state.agentTracePendingBase = [];
}

/** Bind presentation data to immutable public pane metadata, never to authorization. */
export function syncAgentTraceBinding(): TraceBinding | null {
  const binding = bindingNow();
  if (!binding) return null;
  const changed = state.agentTraceIdentity && (
    state.agentTraceIdentity !== binding.identity || state.agentTracePolicy !== binding.policy
  );
  if (changed) {
    traceRequest += 1;
    state.agentTraceBusy = false;
    forgetAgentTrace(state.paneId);
    clearTraceView();
  }
  state.agentTracePolicy = binding.policy;
  state.agentTraceIdentity = binding.identity;
  return binding;
}

function isCurrent(
  request: number,
  session: NonNullable<typeof state.live>,
  paneId: string,
  binding: TraceBinding,
): boolean {
  const current = bindingNow();
  return request === traceRequest && state.live === session && state.paneId === paneId &&
    state.agentChat && session.isConnected() && current?.policy === binding.policy &&
    current.identity === binding.identity && state.agentTracePolicy === binding.policy &&
    state.agentTraceIdentity === binding.identity;
}

function rememberTrace(paneId: string): void {
  cacheAgentTrace(paneId, {
    items: state.agentTraceItems,
    nextCursor: state.agentTraceNext,
    note: state.agentTraceNote,
    truncated: state.agentTraceTruncated,
    signature: state.agentTraceSig,
    tail: state.agentTraceTail,
    policy: state.agentTracePolicy,
    identity: state.agentTraceIdentity,
    pageDepth: state.agentTracePageDepth,
    seenCursors: state.agentTraceSeenCursors,
  });
}

export function restoreAgentTraceState(paneId: string): boolean {
  if (state.paneId !== paneId) return false;
  const binding = bindingNow();
  const entry = cachedAgentTrace(paneId);
  if (!binding || !entry || entry.policy !== binding.policy || entry.identity !== binding.identity) {
    if (entry) forgetAgentTrace(paneId);
    return false;
  }
  state.agentTraceItems = entry.items;
  state.agentTraceNext = entry.nextCursor;
  state.agentTraceNote = entry.note;
  state.agentTraceTruncated = entry.truncated;
  state.agentTraceSig = entry.signature;
  state.agentTraceTail = entry.tail;
  state.agentTracePolicy = entry.policy;
  state.agentTraceIdentity = entry.identity;
  state.agentTracePageDepth = entry.pageDepth;
  state.agentTraceSeenCursors = entry.seenCursors;
  state.agentTraceLoadState = "ready";
  return true;
}

function applyLegacyTracePage(page: AgentTracePage, older: boolean, absorbPending: (items: AgentTraceItem[]) => void): boolean {
  if (older) {
    state.agentTracePageDepth = Math.max(1, state.agentTracePageDepth) + 1;
    if (!page.items.length) {
      const changed = state.agentTraceNext !== page.nextCursor;
      state.agentTraceNext = page.nextCursor;
      return changed;
    }
    const tailWasWholeView = state.agentTraceItems.length === state.agentTraceTail;
    const merged = mergeAgentTraceSegments(page.items, state.agentTraceItems);
    state.agentTraceItems = merged.items;
    if (tailWasWholeView) state.agentTraceTail = Math.max(0, state.agentTraceTail - merged.overlap);
    state.agentTraceNext = page.nextCursor;
    absorbPending(state.agentTraceItems);
    return true;
  }
  const sig = fingerprint(page.items);
  state.agentTracePageDepth = Math.max(1, state.agentTracePageDepth);
  if (sig === state.agentTraceSig) {
    const changed = state.agentTraceItems.length === state.agentTraceTail && state.agentTraceNext !== page.nextCursor;
    if (state.agentTraceItems.length === state.agentTraceTail) state.agentTraceNext = page.nextCursor;
    absorbPending(page.items);
    return changed;
  }
  const kept = Math.max(0, state.agentTraceItems.length - state.agentTraceTail);
  const prefix = kept > 0 ? state.agentTraceItems.slice(0, kept) : [];
  const merged = mergeAgentTraceSegments(prefix, page.items);
  state.agentTraceItems = merged.items;
  state.agentTraceTail = page.items.length - merged.overlap;
  state.agentTraceSig = sig;
  if (prefix.length === 0) state.agentTraceNext = page.nextCursor;
  absorbPending(state.agentTraceItems);
  return true;
}

function currentCursorWindow(): CursorTraceWindow {
  return {
    items: state.agentTraceItems,
    nextCursor: state.agentTraceNext,
    pageDepth: Math.max(1, state.agentTracePageDepth),
    signature: state.agentTraceSig,
    tail: state.agentTraceTail,
    truncated: state.agentTraceTruncated,
    seenCursors: state.agentTraceSeenCursors,
  };
}

function applyCursorWindow(window: CursorTraceWindow, absorbPending: (items: AgentTraceItem[]) => void): boolean {
  const changed = fingerprint(state.agentTraceItems) !== fingerprint(window.items) ||
    state.agentTraceNext !== window.nextCursor || state.agentTracePageDepth !== window.pageDepth ||
    state.agentTraceTruncated !== window.truncated;
  state.agentTraceItems = window.items;
  state.agentTraceNext = window.nextCursor;
  state.agentTracePageDepth = window.pageDepth;
  state.agentTraceSig = window.signature;
  state.agentTraceTail = window.tail;
  state.agentTraceTruncated = window.truncated;
  state.agentTraceSeenCursors = window.seenCursors;
  absorbPending(window.items);
  return changed;
}

function traceLatencyBucket(ms: number): string {
  if (ms < 100) return "lt_100ms";
  if (ms < 500) return "lt_500ms";
  if (ms < 2_000) return "lt_2s";
  return "gte_2s";
}

function sameRequestTarget(
  request: number,
  session: NonNullable<typeof state.live>,
  paneId: string,
  binding: TraceBinding,
): boolean {
  const current = bindingNow();
  return request === traceRequest && state.live === session && state.paneId === paneId &&
    current?.policy === binding.policy && current.identity === binding.identity &&
    state.agentTracePolicy === binding.policy && state.agentTraceIdentity === binding.identity;
}

async function refreshCursorTrace(
  older: boolean,
  session: NonNullable<typeof state.live>,
  paneId: string,
  request: number,
  binding: TraceBinding,
): Promise<CursorTraceWindow | null> {
  if (older) {
    const requestedCursor = state.agentTraceNext;
    if (!requestedCursor) return null;
    const page = await session.agentTrace(paneId, requestedCursor, TRACE_PAGE);
    if (!isCurrent(request, session, paneId, binding)) return null;
    return prependCursorTracePage(currentCursorWindow(), page, requestedCursor);
  }
  const window = await rebuildCursorTraceWindow(
    (cursor) => session.agentTrace(paneId, cursor, TRACE_PAGE),
    state.agentTracePageDepth,
    () => isCurrent(request, session, paneId, binding),
  );
  return window;
}

export async function refreshAgentTraceState(older: boolean, ui: AgentTraceRefreshUI): Promise<boolean> {
  const session = state.live;
  const paneId = state.paneId;
  const binding = syncAgentTraceBinding();
  if (!session || !paneId || !binding || !state.agentChat || !session.isConnected()) return false;
  if (state.agentTraceBusy || (older && !state.agentTraceNext)) return false;
  const request = ++traceRequest;
  const measureColdLoad = !older && state.agentTraceLoadState === "cold" && !state.agentTraceItems.length;
  const startedAt = Date.now();
  let measured = false;
  const stream = ui.stream();
  const follow = !older && (!stream || ui.atBottom(stream));
  const top = stream?.scrollTop ?? 0;
  const height = stream?.scrollHeight ?? 0;
  state.agentTraceBusy = true;
  if (!older && !state.agentTraceItems.length && !state.agentTracePending) state.agentTraceLoadState = "loading";
  stream?.setAttribute("aria-busy", "true");
  ui.syncOlder();
  if (!older) {
    state.agentTraceNote = "";
    if (binding.policy === "owning-user") state.agentTraceTruncated = false;
  }
  let changed = false;
  try {
    if (binding.policy === "disjoint") {
      const window = await refreshCursorTrace(older, session, paneId, request, binding);
      if (!window || !isCurrent(request, session, paneId, binding)) return false;
      changed = applyCursorWindow(window, ui.absorbPending);
      state.agentTraceLoadState = "ready";
      rememberTrace(paneId);
      if (measureColdLoad) {
        track("pwa_agent_trace", {
          result: state.agentTraceItems.length ? "content" : "empty",
          extra: traceLatencyBucket(Date.now() - startedAt),
        });
        measured = true;
      }
      if (changed || !older) {
        if (!ui.patch({ follow, older, top, height })) render();
      }
    } else {
      let cursor: string | null = older ? state.agentTraceNext : null;
      let pulls = 0;
      const filling = !older;
      while (pulls < (filling ? OLDER_FILL_MAX : 1)) {
        const page = await session.agentTrace(paneId, cursor, TRACE_PAGE);
        if (!isCurrent(request, session, paneId, binding)) return false;
        if (measureColdLoad && !measured) {
          track("pwa_agent_trace", { result: page.items.length ? "content" : "empty", extra: traceLatencyBucket(Date.now() - startedAt) });
          measured = true;
        }
        const applied = applyLegacyTracePage(page, cursor !== null, ui.absorbPending);
        if (page.truncated) state.agentTraceTruncated = true;
        changed = applied || changed;
        pulls += 1;
        state.agentTraceLoadState = "ready";
        rememberTrace(paneId);
        if (applied || pulls === 1) {
          const current = ui.stream();
          const pageTop = cursor === null ? top : current?.scrollTop ?? top;
          const pageHeight = cursor === null ? height : current?.scrollHeight ?? height;
          if (!ui.patch({ follow, older: cursor !== null, top: pageTop, height: pageHeight })) render();
        }
        if (cursor !== null && !applied) break;
        if (!filling || !firstTurnNeedsUser(state.agentTraceItems, state.agentTraceNext) || !state.agentTraceNext) break;
        cursor = state.agentTraceNext;
      }
      state.agentTraceLoadState = "ready";
      rememberTrace(paneId);
    }
    ui.syncOlder();
    return changed;
  } catch (error) {
    if (!isCurrent(request, session, paneId, binding)) return false;
    const code = error instanceof ProtocolError ? error.code : "";
    if (measureColdLoad && !measured) {
      track("pwa_agent_trace", { result: code || "failed", extra: traceLatencyBucket(Date.now() - startedAt) });
    }
    state.agentTraceLoadState = "error";
    if (code === "transcript_unavailable" && !state.agentTraceItems.length) {
      state.agentTraceNext = null;
      state.agentTraceSig = "";
      state.agentTraceTail = 0;
      state.agentTracePageDepth = 0;
      state.agentTraceTruncated = false;
      state.agentTraceNote = state.agentTracePending ? "" : ui.unavailableNote();
    } else {
      state.agentTraceNote = messageOf(error, "read");
    }
    if (!ui.patch({ follow })) render();
    return false;
  } finally {
    if (sameRequestTarget(request, session, paneId, binding)) {
      state.agentTraceBusy = false;
      ui.stream()?.setAttribute("aria-busy", "false");
      ui.syncOlder();
    }
  }
}

export function cancelAgentTraceRefresh(): void {
  traceRequest += 1;
  state.agentTraceBusy = false;
}

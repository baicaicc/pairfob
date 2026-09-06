import { Window } from "happy-dom";
import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import type { AgentTracePage } from "../lib/operations";

const happy = new Window({ url: "https://pairfob.com/pair", width: 390, height: 844 });
const g = globalThis as unknown as Record<string, unknown>;
for (const key of [
  "window", "document", "navigator", "HTMLElement", "HTMLButtonElement", "HTMLTextAreaElement",
  "HTMLDetailsElement", "HTMLDialogElement", "Node", "DocumentFragment", "ResizeObserver",
  "MutationObserver", "DOMParser", "localStorage", "sessionStorage",
] as const) g[key] = (happy as unknown as Record<string, unknown>)[key];
g.location = happy.location;
g.history = happy.history;
g.getComputedStyle = happy.getComputedStyle.bind(happy);
g.matchMedia = happy.matchMedia.bind(happy);
g.requestAnimationFrame = happy.requestAnimationFrame.bind(happy);
g.cancelAnimationFrame = happy.cancelAnimationFrame.bind(happy);
g.visualViewport = happy.visualViewport;
happy.document.body.innerHTML = '<main id="app"></main>';

const { app, state } = await import("../state.ts");
const { clearAgentTraceCache } = await import("../lib/agent-trace-cache.ts");
const { setRenderer } = await import("../paint.ts");
const { renderPane } = await import("./pane.ts");
const { patchAgentChat, refreshAgentTrace, restoreAgentTrace } = await import("./agent-chat.ts");

const page = (items: AgentTracePage["items"], nextCursor: string | null, truncated = false): AgentTracePage => ({
  items, nextCursor, truncated,
});
const turn = (name: string, answer = `${name}-answer`): AgentTracePage["items"] => [
  { type: "user", text: name }, { type: "assistant", text: answer },
];

function live(read: (cursor: string | null) => Promise<AgentTracePage>) {
  return {
    agentTrace: (_paneId: string, cursor: string | null = null) => read(cursor),
    agentTraceDetail: async (_paneId: string, detailRef: string) => ({ detailRef, truncated: false }),
    promptAgent: async () => ({ operation_id: "op_AAECAwQFBgcICQoL", pane_id: "p1", agent_status: "working", outcome: "applied" }),
    sendKeys: async () => undefined,
    sendText: async () => undefined,
    isConnected: () => true,
    onEvent: () => () => undefined,
    reconnectNow: () => undefined,
    close: () => undefined,
  };
}

function boot(read: (cursor: string | null) => Promise<AgentTracePage>): void {
  state.phase = "live";
  state.screen = "pane";
  state.paneId = "p1";
  state.operationCapabilities = { ...state.operationCapabilities, prompt_agent: true, history: true };
  state.agents = [{
    paneId: "p1", agent: "Cursor ACP", hasAgent: true, status: "idle", workspaceLabel: "demo",
    workspaceId: "w1", tabId: "t1", cwd: "/repo", historyAvailable: true,
  }];
  state.live = live(read) as typeof state.live;
  state.fullTerminal = false;
  state.agentChat = true;
  state.agentTraceItems = [];
  state.agentTraceNext = null;
  state.agentTraceBusy = false;
  state.agentTraceNote = "";
  state.agentTraceTruncated = false;
  state.agentTraceSig = "";
  state.agentTraceTail = 0;
  state.agentTracePolicy = "owning-user";
  state.agentTraceIdentity = "";
  state.agentTracePageDepth = 0;
  state.agentTraceSeenCursors = [];
  state.agentTraceLoadState = "ready";
  state.agentTracePending = "";
  state.agentTracePendingBase = [];
  state.agentTraceFollow = true;
  state.agentTraceUnread = false;
  renderPane();
}

beforeAll(() => setRenderer(() => renderPane()));

afterEach(() => {
  state.agentChat = false;
  state.live = null;
  state.paneId = "";
  state.screen = "home";
  state.agents = [];
  state.agentTraceItems = [];
  state.agentTraceIdentity = "";
  state.agentTracePageDepth = 0;
  state.agentTraceSeenCursors = [];
  state.agentTraceBusy = false;
  clearAgentTraceCache();
  app.replaceChildren();
});

describe("Cursor disjoint trace refresh", () => {
  test("keeps identical user events when the older one has no answer", async () => {
    boot(async (cursor) => cursor
      ? page([{ type: "user", text: "继续" }], null)
      : page([{ type: "user", text: "继续" }, { type: "assistant", text: "第二轮完成" }], "older"));
    await refreshAgentTrace();
    await refreshAgentTrace(true);
    expect(state.agentTraceItems).toEqual([
      { type: "user", text: "继续" },
      { type: "user", text: "继续" },
      { type: "assistant", text: "第二轮完成" },
    ]);
  });

  test("rebuilds A/B/C into the contiguous B/C/D chain", async () => {
    let moving = false;
    boot(async (cursor) => {
      if (!moving) return cursor
        ? page(turn("A"), null)
        : page([...turn("B"), ...turn("C")], "older-a");
      return cursor
        ? page(turn("B"), "older-a")
        : page([...turn("C"), ...turn("D")], "older-b");
    });

    await refreshAgentTrace();
    await refreshAgentTrace(true);
    expect(state.agentTraceItems.filter((item) => item.type === "user").map((item) => item.text)).toEqual(["A", "B", "C"]);
    expect(state.agentTracePageDepth).toBe(2);

    moving = true;
    await refreshAgentTrace();
    expect(state.agentTraceItems.filter((item) => item.type === "user").map((item) => item.text)).toEqual(["B", "C", "D"]);
    expect(state.agentTraceNext).toBe("older-a");
    expect(state.agentTracePageDepth).toBe(2);
  });

  test("updates growing output and appends a new turn across multiple loaded pages", async () => {
    let latest = page([...turn("C", "partial")], "older");
    boot(async (cursor) => cursor ? page(turn("B"), "oldest") : latest);
    await refreshAgentTrace();
    await refreshAgentTrace(true);

    latest = page([...turn("C", "partial completed"), ...turn("D")], "older");
    await refreshAgentTrace();
    expect(state.agentTraceItems.map((item) => item.text)).toEqual([
      "B", "B-answer", "C", "partial completed", "D", "D-answer",
    ]);
    expect(state.agentTraceNext).toBe("oldest");
  });

  test("keeps a repeated optimistic prompt through a window shift until its real echo arrives", async () => {
    let fresh = false;
    let echoed = false;
    const recent = (answer: string): AgentTracePage["items"] => [
      { type: "user", text: "repeat" }, { type: "assistant", text: "historical reply" },
      { type: "user", text: "C" }, { type: "assistant", text: answer },
      ...(echoed ? [{ type: "user" as const, text: "repeat" }] : []),
    ];
    boot(async (cursor) => {
      if (!fresh) return cursor ? page(turn("A"), null) : page(recent("partial"), "old-a");
      return cursor ? page([], null) : page(recent("partial completed"), "old-empty");
    });
    await refreshAgentTrace();
    await refreshAgentTrace(true);
    state.agentTracePendingBase = structuredClone(state.agentTraceItems);
    state.agentTracePending = "repeat";
    fresh = true;

    await refreshAgentTrace();
    expect(state.agentTracePending).toBe("repeat");
    expect([...app.querySelectorAll(".agent-user-text")].map((item) => item.textContent)).toEqual([
      "repeat", "C", "repeat",
    ]);

    echoed = true;
    await refreshAgentTrace();
    expect(state.agentTracePending).toBe("");
    expect([...app.querySelectorAll(".agent-user-text")].map((item) => item.textContent)).toEqual([
      "repeat", "C", "repeat",
    ]);
  });

  test("a distinct prompt clears from a unique one-event anchor in a complete window", async () => {
    let arrived = false;
    boot(async () => page(arrived
      ? [
          { type: "user", text: "A" }, { type: "assistant", text: "A done" },
          { type: "user", text: "B" }, { type: "assistant", text: "B done" },
        ]
      : [{ type: "user", text: "A" }], null));
    await refreshAgentTrace();
    state.agentTracePendingBase = structuredClone(state.agentTraceItems);
    state.agentTracePending = "B";
    arrived = true;

    await refreshAgentTrace();
    expect(state.agentTracePending).toBe("");
    expect([...app.querySelectorAll(".agent-user-text")].map((item) => item.textContent)).toEqual(["A", "B"]);
    await refreshAgentTrace();
    expect(state.agentTracePending).toBe("");
    expect([...app.querySelectorAll(".agent-user-text")].map((item) => item.textContent)).toEqual(["A", "B"]);
  });

  test("prepending an older identical prompt does not consume the optimistic prompt", async () => {
    boot(async (cursor) => cursor
      ? page([{ type: "user", text: "repeat" }, { type: "assistant", text: "older reply" }], null)
      : page([
          { type: "user", text: "repeat" }, { type: "assistant", text: "recent reply" },
          { type: "user", text: "C" }, { type: "assistant", text: "C done" },
        ], "older"));
    await refreshAgentTrace();
    state.agentTracePendingBase = structuredClone(state.agentTraceItems);
    state.agentTracePending = "repeat";

    await refreshAgentTrace(true);
    expect(state.agentTracePending).toBe("repeat");
    expect([...app.querySelectorAll(".agent-user-text")].map((item) => item.textContent)).toEqual([
      "repeat", "repeat", "C", "repeat",
    ]);
  });

  test("manual older reads cross empty source pages and count their depth", async () => {
    boot(async (cursor) => {
      if (!cursor) return page(turn("C"), "empty");
      if (cursor === "empty") return page([], "older");
      return page(turn("B"), null);
    });
    await refreshAgentTrace();
    await refreshAgentTrace(true);
    expect(state.agentTraceItems.map((item) => item.text)).toEqual(["C", "C-answer"]);
    expect(state.agentTraceNext).toBe("older");
    expect(state.agentTracePageDepth).toBe(2);

    await refreshAgentTrace(true);
    expect(state.agentTraceItems.map((item) => item.text)).toEqual(["B", "B-answer", "C", "C-answer"]);
    expect(state.agentTracePageDepth).toBe(3);
  });

  test("a failed later rebuild page retains the prior whole view and cache", async () => {
    let fail = false;
    boot(async (cursor) => {
      if (fail && cursor) throw new Error("later failed");
      return cursor ? page(turn("B"), "oldest") : page(turn(fail ? "D" : "C"), "older");
    });
    await refreshAgentTrace();
    await refreshAgentTrace(true);
    const before = structuredClone(state.agentTraceItems);
    const beforeCursor = state.agentTraceNext;
    fail = true;

    expect(await refreshAgentTrace()).toBe(false);
    expect(state.agentTraceItems).toEqual(before);
    expect(state.agentTraceNext).toBe(beforeCursor);
    expect(state.agentTracePageDepth).toBe(2);
    expect(state.agentTraceLoadState).toBe("error");

    state.agentTraceItems = [];
    state.agentTracePageDepth = 0;
    expect(restoreAgentTrace("p1")).toBe(true);
    expect(state.agentTraceItems).toEqual(before);
    expect(state.agentTracePageDepth).toBe(2);
  });

  test("a repeated cursor inside a rebuild is a read failure and preserves the old cache", async () => {
    let cyclic = false;
    boot(async (cursor) => {
      if (cyclic) return cursor
        ? page(turn("fresh-older"), "cycle")
        : page(turn("fresh"), "cycle");
      return cursor ? page(turn("B"), "oldest") : page(turn("C"), "older");
    });
    await refreshAgentTrace();
    await refreshAgentTrace(true);
    const before = structuredClone(state.agentTraceItems);
    const beforeCursor = state.agentTraceNext;
    cyclic = true;

    expect(await refreshAgentTrace()).toBe(false);
    expect(state.agentTraceItems).toEqual(before);
    expect(state.agentTraceNext).toBe(beforeCursor);
    expect(state.agentTracePageDepth).toBe(2);
    expect(state.agentTraceLoadState).toBe("error");
    state.agentTraceItems = [];
    expect(restoreAgentTrace("p1")).toBe(true);
    expect(state.agentTraceItems).toEqual(before);
  });

  test("a cursor cycle across manual taps fails without publishing the cyclic page", async () => {
    boot(async (cursor) => {
      if (!cursor) return page(turn("C"), "a");
      if (cursor === "a") return page(turn("B"), "b");
      return page(turn("A"), "a");
    });
    await refreshAgentTrace();
    await refreshAgentTrace(true);
    const before = structuredClone(state.agentTraceItems);
    expect(state.agentTraceNext).toBe("b");

    expect(await refreshAgentTrace(true)).toBe(false);
    expect(state.agentTraceItems).toEqual(before);
    expect(state.agentTraceNext).toBe("b");
    expect(state.agentTracePageDepth).toBe(2);
    expect(state.agentTraceLoadState).toBe("error");
  });

  test.each(["disconnect", "metadata", "session"] as const)(
    "%s at the helper-return microtask boundary cannot publish or cache the staged window",
    async (boundary) => {
      let fresh = false;
      boot(async (cursor) => cursor
        ? page(turn(fresh ? "fresh-older" : "B"), null)
        : page(turn(fresh ? "fresh" : "C"), "older"));
      await refreshAgentTrace();
      await refreshAgentTrace(true);
      const before = structuredClone(state.agentTraceItems);
      const beforeCursor = state.agentTraceNext;
      fresh = true;
      let connected = true;
      let checks = 0;
      const current = state.live!;
      const replacement = { ...current, isConnected: () => true } as typeof state.live;
      state.live = {
        ...current,
        isConnected: () => {
          checks += 1;
          if (checks === 3) queueMicrotask(() => {
            if (boundary === "disconnect") connected = false;
            else if (boundary === "metadata") state.agents[0].cwd = "/changed-at-boundary";
            else state.live = replacement;
          });
          return connected;
        },
      } as typeof state.live;

      expect(await refreshAgentTrace()).toBe(false);
      expect(state.agentTraceItems).toEqual(before);
      expect(state.agentTraceNext).toBe(beforeCursor);
      expect(state.agentTracePageDepth).toBe(2);
      if (boundary === "metadata") {
        patchAgentChat();
        expect(state.agentTraceItems).toEqual([]);
      } else {
        state.live = current;
        expect(restoreAgentTrace("p1")).toBe(true);
        expect(state.agentTraceItems).toEqual(before);
      }
    },
  );

  test("metadata changes during a rebuild discard staged pages and invalidate the cache", async () => {
    let finishOlder: ((value: AgentTracePage) => void) | undefined;
    boot(async (cursor) => {
      if (!cursor) return page(turn("new"), "older");
      return new Promise((resolve) => { finishOlder = resolve; });
    });
    state.agentTraceItems = turn("old");
    state.agentTraceNext = "old-cursor";
    state.agentTracePageDepth = 2;
    patchAgentChat();
    const pending = refreshAgentTrace();
    await Promise.resolve();
    await Promise.resolve();
    state.agents[0].cwd = "/other";
    patchAgentChat();
    finishOlder?.(page(turn("middle"), null));

    expect(await pending).toBe(false);
    expect(state.agentTraceItems).toEqual([]);
    expect(state.agentTracePageDepth).toBe(0);
    expect(restoreAgentTrace("p1")).toBe(false);
  });

  test("navigation and disconnect during a rebuild never publish staged pages", async () => {
    for (const interrupt of ["navigate", "disconnect"] as const) {
      let connected = true;
      let finishOlder: ((value: AgentTracePage) => void) | undefined;
      boot(async (cursor) => cursor
        ? new Promise((resolve) => { finishOlder = resolve; })
        : page(turn("new"), "older"));
      state.agentTraceItems = turn("old");
      state.agentTracePageDepth = 2;
      patchAgentChat();
      const session = state.live!;
      state.live = { ...session, isConnected: () => connected } as typeof state.live;
      const pending = refreshAgentTrace();
      await Promise.resolve();
      await Promise.resolve();
      if (interrupt === "navigate") state.agentChat = false;
      else connected = false;
      finishOlder?.(page(turn("middle"), null));
      expect(await pending).toBe(false);
      expect(state.agentTraceItems).toEqual(turn("old"));
      state.agentChat = false;
      clearAgentTraceCache();
    }
  });

  test("cache restore validates identity and keeps loaded page depth", async () => {
    boot(async (cursor) => cursor ? page(turn("B"), null) : page(turn("C"), "older"));
    await refreshAgentTrace();
    await refreshAgentTrace(true);
    state.agentTraceItems = [];
    state.agentTracePageDepth = 0;
    expect(restoreAgentTrace("p1")).toBe(true);
    expect(state.agentTracePageDepth).toBe(2);

    state.agents[0].tabId = "t2";
    state.agentTraceItems = [];
    expect(restoreAgentTrace("p1")).toBe(false);
    expect(restoreAgentTrace("p1")).toBe(false);
  });
});

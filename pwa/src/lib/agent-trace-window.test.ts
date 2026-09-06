import { describe, expect, test } from "bun:test";
import type { AgentTracePage } from "./operations";
import { ProtocolError } from "./protocol/errors";
import {
  MAX_CURSOR_TRACE_PAGES,
  agentTracePaneIdentity,
  agentTracePresentationPolicy,
  cursorPendingBoundary,
  prependCursorTracePage,
  rebuildCursorTraceWindow,
} from "./agent-trace-window";

const page = (
  items: AgentTracePage["items"],
  nextCursor: string | null,
  truncated = false,
): AgentTracePage => ({ items, nextCursor, truncated });

describe("Cursor trace presentation policy", () => {
  test("uses exact canonical agent metadata and excludes mutable labels and status", () => {
    expect(agentTracePresentationPolicy({ agent: "Cursor ACP" })).toBe("disjoint");
    expect(agentTracePresentationPolicy({ agent: "cursor acp" })).toBe("owning-user");
    expect(agentTracePresentationPolicy({ agent: "codex" })).toBe("owning-user");
    expect(agentTracePaneIdentity({
      agent: "Cursor ACP", workspaceId: "w1", tabId: "t1", cwd: "/repo",
    })).toBe('["Cursor ACP","w1","t1","/repo"]');
  });

  test("concatenates disjoint pages exactly, including identical unanswered prompts", async () => {
    const pages = new Map<string | null, AgentTracePage>([
      [null, page([{ type: "user", text: "继续" }, { type: "assistant", text: "第二轮完成" }], "older")],
      ["older", page([{ type: "user", text: "继续" }], null)],
    ]);
    const result = await rebuildCursorTraceWindow(async (cursor) => pages.get(cursor)!, 2);
    expect(result?.items).toEqual([
      { type: "user", text: "继续" },
      { type: "user", text: "继续" },
      { type: "assistant", text: "第二轮完成" },
    ]);
  });

  test("preserves an operation split across source pages", async () => {
    const pages = new Map<string | null, AgentTracePage>([
      [null, page([
        { type: "tool", name: "Read", output: "ok" },
        { type: "assistant", text: "done" },
      ], "older")],
      ["older", page([
        { type: "user", text: "inspect" },
        { type: "thinking", text: "reading" },
      ], null)],
    ]);
    const result = await rebuildCursorTraceWindow(async (cursor) => pages.get(cursor)!, 1);
    expect(result?.items.map((item) => item.type)).toEqual(["user", "thinking", "tool", "assistant"]);
    expect(result?.pageDepth).toBe(2);
  });

  test("counts empty progressing pages", async () => {
    const cursors: Array<string | null> = [];
    const pages = new Map<string | null, AgentTracePage>([
      [null, page([{ type: "user", text: "new" }], "empty")],
      ["empty", page([], "older")],
      ["older", page([{ type: "user", text: "old" }], null)],
    ]);
    const result = await rebuildCursorTraceWindow(async (cursor) => {
      cursors.push(cursor);
      return pages.get(cursor)!;
    }, 8);
    expect(cursors).toEqual([null, "empty", "older"]);
    expect(result?.items.map((item) => item.text)).toEqual(["old", "new"]);
    expect(result?.pageDepth).toBe(3);
    expect(result?.nextCursor).toBeNull();
  });

  test("rejects a repeated cursor within one rebuild", async () => {
    const pages = new Map<string | null, AgentTracePage>([
      [null, page([{ type: "user", text: "new" }], "cycle")],
      ["cycle", page([{ type: "user", text: "old" }], "cycle")],
    ]);
    await expect(rebuildCursorTraceWindow(async (cursor) => pages.get(cursor)!, 2))
      .rejects.toBeInstanceOf(ProtocolError);
  });

  test("clamps a rebuild to 32 pages and leaves the oldest fresh cursor accessible", async () => {
    const cursors: Array<string | null> = [];
    const result = await rebuildCursorTraceWindow(async (cursor) => {
      cursors.push(cursor);
      const index = cursor ? Number(cursor.slice(1)) : 0;
      return page([{ type: "user", text: String(index) }], `c${index + 1}`);
    }, 99);
    expect(cursors).toHaveLength(MAX_CURSOR_TRACE_PAGES);
    expect(result?.pageDepth).toBe(MAX_CURSOR_TRACE_PAGES);
    expect(result?.nextCursor).toBe("c32");
    expect(result?.items.at(0)?.text).toBe("31");
    expect(result?.items.at(-1)?.text).toBe("0");
  });

  test("manual pagination rejects a cursor already visited by an earlier tap", () => {
    const current = {
      items: [{ type: "user" as const, text: "new" }],
      nextCursor: "same",
      pageDepth: 32,
      signature: "new",
      tail: 1,
      truncated: false,
      seenCursors: ["older"],
    };
    expect(() => prependCursorTracePage(current, page([], "older"), "same"))
      .toThrow("AgentTrace cursor repeated");
  });
});

describe("Cursor optimistic baseline alignment", () => {
  test("aligns a retained suffix after the oldest page leaves the window", () => {
    const baseline = [
      ...page([{ type: "user", text: "A" }, { type: "assistant", text: "A done" }], null).items,
      { type: "user" as const, text: "repeat" }, { type: "assistant" as const, text: "old reply" },
      { type: "user" as const, text: "C" }, { type: "assistant" as const, text: "partial" },
    ];
    const shifted = baseline.slice(2).map((item) => ({ ...item }));
    shifted[3] = { type: "assistant", text: "partial completed" };
    expect(cursorPendingBoundary(baseline, shifted)).toBe(4);
    expect(cursorPendingBoundary(baseline, [...shifted, { type: "user", text: "repeat" }])).toBe(4);
  });

  test("aligns the old baseline after an exact older-page prepend", () => {
    const baseline = [
      { type: "user" as const, text: "repeat" }, { type: "assistant" as const, text: "recent" },
      { type: "user" as const, text: "C" }, { type: "assistant" as const, text: "done" },
    ];
    const prepended = [
      { type: "user" as const, text: "repeat" }, { type: "assistant" as const, text: "older" },
      ...baseline,
    ];
    expect(cursorPendingBoundary(baseline, prepended)).toBe(prepended.length);
  });

  test("keeps ambiguous single-event overlap conservative", () => {
    const baseline = [{ type: "user" as const, text: "repeat" }];
    expect(cursorPendingBoundary(baseline, [{ type: "user", text: "repeat" }])).toBe(1);
    expect(cursorPendingBoundary(baseline, [
      { type: "user", text: "repeat" },
      { type: "assistant", text: "historical" },
      { type: "user", text: "repeat" },
    ], "repeat", true)).toBe(3);
  });

  test("accepts one unique same-position anchor in a complete window for a distinct prompt", () => {
    const baseline = [{ type: "user" as const, text: "A" }];
    const complete = [
      { type: "user" as const, text: "A" }, { type: "assistant" as const, text: "A done" },
      { type: "user" as const, text: "B" }, { type: "assistant" as const, text: "B done" },
    ];
    expect(cursorPendingBoundary(baseline, complete, "B", true)).toBe(1);
    expect(cursorPendingBoundary(baseline, complete, "B", false)).toBe(complete.length);
  });
});

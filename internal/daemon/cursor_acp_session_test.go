//go:build darwin || linux

package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"pairfob/internal/journal"
	"pairfob/internal/runtime"
)

// configureCursorRuntimeSession rewrites the fixture's private config with a
// named Herdr session (the config's runtimeSession field) and reloads it.
func configureCursorRuntimeSession(t *testing.T, f *cursorResolverFixture, name string) {
	t.Helper()
	config := *f.config
	config.RuntimeSession = name
	path := filepath.Join(filepath.Dir(config.RegistryRoot), "config.json")
	writeDaemonJSON(t, path, config)
	loaded, err := journal.LoadCursorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	f.config = loaded
}

func assertCursorSessions(t *testing.T, seen []runtime.SessionRef, want runtime.SessionRef) {
	t.Helper()
	if len(seen) == 0 {
		t.Fatal("runtime was not consulted")
	}
	for _, session := range seen {
		if session != want {
			t.Fatalf("runtime call used session %+v, want %+v (all=%+v)", session, want, seen)
		}
	}
}

func TestCursorResolverHonorsConfiguredRuntimeSession(t *testing.T) {
	f := newCursorResolverFixture(t)
	configureCursorRuntimeSession(t, &f, "alpha")
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok || f.runtime.totalCalls() != 0 || f.identityCalls != 0 {
		t.Fatal("default session resolved or probed a registration configured for a named session")
	}
	if _, ok := engine.selectJournalRef(context.Background(), runtime.NamedSession("alpha"), f.pane); !ok {
		t.Fatal("configured named session did not resolve")
	}
	assertCursorSessions(t, f.runtime.sessions, runtime.NamedSession("alpha"))
}

func TestCursorRPCsFollowThePhoneSelectedSession(t *testing.T) {
	fixture, engine, client := newCursorPromptRPCFixture(t)
	configureCursorRuntimeSession(t, fixture.cursor, "alpha")
	engine.CursorConfig, engine.Journal.CursorACP = fixture.cursor.config, fixture.cursor.config

	if raw, err := client.RPC("Snapshot", map[string]any{}); err != nil || strings.Contains(string(raw), `"history_available":true`) || strings.Contains(string(raw), "Cursor ACP") {
		t.Fatalf("default session exposed a named-session Cursor owner: %s err=%v", raw, err)
	}
	if _, err := client.RPC("History", map[string]any{"pane_id": fixture.cursor.pane.PaneID, "limit": 10}); err == nil || err.Error() != "transcript_unavailable" {
		t.Fatalf("default session History=%v", err)
	}
	fixture.cursor.runtime.sessions = nil
	raw, err := client.RPC("Snapshot", map[string]any{"session": "alpha"})
	if err != nil || !strings.Contains(string(raw), `"history_available":true`) || !strings.Contains(string(raw), `"agent":"Cursor ACP"`) {
		t.Fatalf("named session snapshot=%s err=%v", raw, err)
	}
	assertCursorSessions(t, fixture.cursor.runtime.sessions, runtime.NamedSession("alpha"))

	fixture.cursor.runtime.sessions = nil
	result, err := client.RPC("PromptAgent", map[string]any{"session": "alpha", "operation_id": "op_cursornamedsess01", "pane_id": fixture.cursor.pane.PaneID, "text": "named"})
	if err != nil || !strings.Contains(string(result), `"outcome":"applied"`) || fixture.requestCount() != 1 || fixture.cursor.runtime.executeCalls != 0 {
		t.Fatalf("named session prompt=%s err=%v owner=%d execute=%d", result, err, fixture.requestCount(), fixture.cursor.runtime.executeCalls)
	}
	assertCursorSessions(t, fixture.cursor.runtime.sessions, runtime.NamedSession("alpha"))
	history, err := client.RPC("History", map[string]any{"session": "alpha", "pane_id": fixture.cursor.pane.PaneID, "limit": 10})
	if err != nil || !strings.Contains(string(history), "named") || !strings.Contains(string(history), "owner answer") {
		t.Fatalf("named session History=%s err=%v", history, err)
	}
}

// With Cursor ACP configured, PromptAgent answers from the ledger first; a pane
// that is not a registered owner still reaches the runtime's own agent.prompt.
func TestCursorConfiguredPromptKeepsBuiltinAgentsOnRuntime(t *testing.T) {
	f := newCursorResolverFixture(t)
	fake := runtime.NewFake()
	engine, client := runtimeRPCClient(t, fake)
	engine.CursorConfig, engine.Journal.CursorACP = f.config, f.config
	engine.cursorIdentity = f.identityReader()

	raw, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cfgbuiltinprompt1", "pane_id": "w0:p1", "text": "/quit"})
	if err != nil || !strings.Contains(string(raw), `"agent_status":"unknown"`) || !strings.Contains(string(raw), `"outcome":"applied"`) {
		t.Fatalf("configured builtin prompt=%s err=%v", raw, err)
	}
	fake.Snap.Panes[0].TerminalID = "unregistered-terminal"
	if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cfgbuiltinprompt2", "pane_id": "w0:p1", "text": "again"}); err != nil {
		t.Fatalf("unregistered terminal did not reach the runtime: %v", err)
	}
	if texts := fake.Panes["w0:p1"].Texts; len(texts) != 2 || texts[0] != "/quit" || texts[1] != "again" {
		t.Fatalf("runtime prompts=%q", texts)
	}
	if f.identityCalls != 0 {
		t.Fatalf("unregistered panes probed process identity: %d", f.identityCalls)
	}
	fake.Snap.Panes[0].Agent = ""
	if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cfgbuiltinprompt3", "pane_id": "w0:p1", "text": "nobody"}); err == nil || err.Error() != "agent_not_found" {
		t.Fatalf("agentless pane=%v", err)
	}
	if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cfgbuiltinprompt4", "pane_id": "w9:p9", "text": "gone"}); err == nil || err.Error() != "agent_not_found" {
		t.Fatalf("missing pane=%v", err)
	}
}

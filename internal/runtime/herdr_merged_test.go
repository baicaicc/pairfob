package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// mergedFixture is a Herdr config root with a running default session, a
// running named session "work" and a stopped named session "old" (a session
// directory that outlived its server).
type mergedFixture struct {
	merged *Merged
	root   *requestLog
	work   *requestLog
}

func sessionSnapshotReply(label, cwd string) scriptedReply {
	return scriptedReply{Result: map[string]any{
		"type": "session_snapshot",
		"snapshot": map[string]any{
			"version": "0.8.2", "protocol": 20,
			"focused_workspace_id": "w1", "focused_tab_id": "w1:t1", "focused_pane_id": "w1:p1",
			"workspaces": []any{map[string]any{"workspace_id": "w1", "number": 1, "label": label, "agent_status": "idle"}},
			"tabs":       []any{map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "label": "main"}},
			"panes": []any{map[string]any{
				"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1", "cwd": cwd,
				"agent": "codex", "agent_status": "idle", "terminal_id": "term_" + label,
			}},
		},
	}}
}

func startMergedFixture(t *testing.T, root, work func(scriptedRequest) scriptedReply) mergedFixture {
	t.Helper()
	configRoot := shortTestDir(t)
	for _, name := range []string{"work", "old"} {
		if err := os.MkdirAll(filepath.Join(configRoot, "sessions", name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := mergedFixture{}
	if root != nil {
		fixture.root = startScriptedHerdrAt(t, filepath.Join(configRoot, "herdr.sock"), root)
	}
	if work != nil {
		fixture.work = startScriptedHerdrAt(t, filepath.Join(configRoot, "sessions", "work", "herdr.sock"), work)
	}
	h := NewHerdr(filepath.Join(configRoot, "herdr.sock"))
	h.ConfigRoot = configRoot
	fixture.merged = NewMerged(h)
	return fixture
}

func mergedReply(label, cwd string) func(scriptedRequest) scriptedReply {
	return func(request scriptedRequest) scriptedReply {
		if request.Method == "session.snapshot" {
			return sessionSnapshotReply(label, cwd)
		}
		return standardReply(request)
	}
}

func mergedSnapshot(t *testing.T, m *Merged) Snapshot {
	t.Helper()
	view, err := m.Observe(context.Background(), DefaultSession(), SnapshotQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return view.(SnapshotView).Snapshot
}

func TestMergedSnapshotListsEveryRunningSession(t *testing.T) {
	fixture := startMergedFixture(t, mergedReply("home", "/home"), mergedReply("lab", "/lab"))
	snapshot := mergedSnapshot(t, fixture.merged)

	var workspaces, panes, tabs []string
	for _, workspace := range snapshot.Workspaces {
		workspaces = append(workspaces, workspace.WorkspaceID+"@"+workspace.Session)
	}
	for _, tab := range snapshot.Tabs {
		tabs = append(tabs, tab.TabID+"/"+tab.WorkspaceID)
	}
	for _, pane := range snapshot.Panes {
		panes = append(panes, pane.PaneID+"/"+pane.WorkspaceID+"/"+pane.TabID+"/"+pane.TerminalID)
	}
	// The default session keeps its ids; the named one is qualified, and the
	// stopped "old" session contributes nothing.
	if want := []string{"w1@", "work::w1@work"}; !reflect.DeepEqual(workspaces, want) {
		t.Fatalf("workspaces = %v, want %v", workspaces, want)
	}
	if want := []string{"w1:t1/w1", "work::w1:t1/work::w1"}; !reflect.DeepEqual(tabs, want) {
		t.Fatalf("tabs = %v, want %v", tabs, want)
	}
	if want := []string{"w1:p1/w1/w1:t1/term_home", "work::w1:p1/work::w1/work::w1:t1/term_lab"}; !reflect.DeepEqual(panes, want) {
		t.Fatalf("panes = %v, want %v", panes, want)
	}
	if snapshot.Session != nil || snapshot.Focused["pane_id"] != "w1:p1" {
		t.Fatalf("the merged view is the default session focused on its pane: session=%v focused=%v", snapshot.Session, snapshot.Focused)
	}
	raw, err := json.Marshal(snapshot.Workspaces[0])
	if err != nil || string(raw) != `{"workspace_id":"w1","number":1,"label":"home","cwd":"","agent_status":"idle"}` {
		t.Fatalf("a default-session workspace keeps its exact wire shape: %s %v", raw, err)
	}
}

func TestMergedSnapshotFollowsTheDefaultSessionAvailability(t *testing.T) {
	// A named session that cannot answer is left out of this snapshot.
	broken := func(request scriptedRequest) scriptedReply {
		return scriptedReply{Error: &scriptedError{Code: "internal", Message: "broken"}}
	}
	fixture := startMergedFixture(t, mergedReply("home", "/home"), broken)
	if snapshot := mergedSnapshot(t, fixture.merged); len(snapshot.Panes) != 1 || snapshot.Panes[0].PaneID != "w1:p1" {
		t.Fatalf("want only the default pane, got %+v", snapshot.Panes)
	}

	// With the default server stopped, a running named session still shows.
	fixture = startMergedFixture(t, nil, mergedReply("lab", "/lab"))
	snapshot := mergedSnapshot(t, fixture.merged)
	if len(snapshot.Panes) != 1 || snapshot.Panes[0].PaneID != "work::w1:p1" || snapshot.Focused["pane_id"] != "work::w1:p1" {
		t.Fatalf("want the named session alone, got %+v focused=%v", snapshot.Panes, snapshot.Focused)
	}

	// Nothing running reports the default session's own fault.
	fixture = startMergedFixture(t, nil, nil)
	_, err := fixture.merged.Observe(context.Background(), DefaultSession(), SnapshotQuery{})
	if fault, ok := AsFault(err); !ok || fault.Code != CodeOffline {
		t.Fatalf("want the default session offline fault, got %v", err)
	}
}

func TestMergedSnapshotRefusesAnAmbiguousDefaultID(t *testing.T) {
	ambiguous := func(request scriptedRequest) scriptedReply {
		reply := sessionSnapshotReply("home", "/home")
		snapshot := reply.Result.(map[string]any)["snapshot"].(map[string]any)
		snapshot["panes"].([]any)[0].(map[string]any)["pane_id"] = "work::w1:p9"
		return reply
	}
	fixture := startMergedFixture(t, ambiguous, mergedReply("lab", "/lab"))
	snapshot := mergedSnapshot(t, fixture.merged)
	for _, pane := range snapshot.Panes {
		if pane.PaneID == "work::w1:p9" {
			t.Fatal("a default-session id containing the separator must not enter the merged view")
		}
	}
	if len(snapshot.Panes) != 1 || snapshot.Panes[0].PaneID != "work::w1:p1" {
		t.Fatalf("want only the named session, got %+v", snapshot.Panes)
	}
}

func TestMergedRoutesByQualifiedID(t *testing.T) {
	fixture := startMergedFixture(t, standardReply, standardReply)
	ctx := context.Background()

	if _, err := fixture.merged.Observe(ctx, DefaultSession(), PaneReadQuery{PaneID: "work::w1:p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.merged.Observe(ctx, DefaultSession(), PaneReadQuery{PaneID: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	assertOnlyRead := func(log *requestLog, want string) {
		t.Helper()
		requests := log.snapshot()
		if len(requests) != 1 || requests[0].Method != "pane.read" {
			t.Fatalf("want one pane.read, got %+v", requests)
		}
		var params struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(requests[0].Params, &params)
		if params.PaneID != want {
			t.Fatalf("Herdr received pane_id %q, want bare %q", params.PaneID, want)
		}
	}
	assertOnlyRead(fixture.work, "w1:p1")
	assertOnlyRead(fixture.root, "w1:p1")
}

func TestMergedRefusesBadRoutesBeforeCallingHerdr(t *testing.T) {
	fixture := startMergedFixture(t, standardReply, standardReply)
	ctx := context.Background()
	for _, id := range []string{"::w1:p1", "work::", "bad name::w1:p1", "..::w1:p1", "work::w1::p1"} {
		_, err := fixture.merged.Observe(ctx, DefaultSession(), PaneReadQuery{PaneID: id})
		if fault, ok := AsFault(err); !ok || fault.Code != CodeInvalid {
			t.Fatalf("%q: want invalid, got %v", id, err)
		}
	}
	receipt, err := fixture.merged.Execute(ctx, DefaultSession(), "op_1", SplitPaneCommand{
		WorkspaceID: "work::w1", TargetPaneID: "w1:p1", Direction: SplitRight,
	})
	if fault, ok := AsFault(err); !ok || fault.Code != CodeInvalid || receipt.Outcome != OutcomeNotApplied {
		t.Fatalf("ids from two sessions must not meet in one call: %+v %v", receipt, err)
	}
	type unknownCommand struct{ SendTextCommand }
	if _, err := fixture.merged.Execute(ctx, DefaultSession(), "op_2", unknownCommand{}); err == nil {
		t.Fatal("an unknown command must fail closed")
	}
	if requests := append(fixture.root.snapshot(), fixture.work.snapshot()...); len(requests) != 0 {
		t.Fatalf("no request may reach Herdr, got %+v", requests)
	}
}

func TestMergedRefusesNamedSessionRefs(t *testing.T) {
	fixture := startMergedFixture(t, standardReply, standardReply)
	ctx := context.Background()
	named := NamedSession("work")
	unsupportedFault := func(err error) bool {
		fault, ok := AsFault(err)
		return ok && fault.Code == CodeUnsupported
	}
	if _, err := fixture.merged.Describe(ctx, named); !unsupportedFault(err) {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := fixture.merged.Observe(ctx, named, SnapshotQuery{}); !unsupportedFault(err) {
		t.Fatalf("Observe: %v", err)
	}
	if _, err := fixture.merged.Execute(ctx, named, "op_1", SendTextCommand{PaneID: "w1:p1", Text: "x"}); !unsupportedFault(err) {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := fixture.merged.SubscribeEvents(ctx, named, EventSubscription{}); !unsupportedFault(err) {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	if _, err := fixture.merged.OpenTerminal(ctx, TerminalOpen{Session: named, PaneID: "w1:p1", Cols: 80, Rows: 24}); !unsupportedFault(err) {
		t.Fatalf("OpenTerminal: %v", err)
	}
	var rt Runtime = fixture.merged
	if _, ok := rt.(SessionLister); ok {
		t.Fatal("a merged view must not offer the per-session switcher")
	}
}

func TestMergedExecuteQualifiesReceipts(t *testing.T) {
	fixture := startMergedFixture(t, standardReply, standardReply)
	ctx := context.Background()

	receipt, err := fixture.merged.Execute(ctx, DefaultSession(), "op_1", CreateTabCommand{WorkspaceID: "work::w1", Label: "two"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range receipt.Created {
		if (ref.Kind == EntityTab || ref.Kind == EntityPane) && ref.ID[:len("work::")] != "work::" {
			t.Fatalf("created %s %q must be qualified", ref.Kind, ref.ID)
		}
	}
	if len(fixture.work.snapshot()) == 0 || len(fixture.root.snapshot()) != 0 {
		t.Fatal("a tab in a named workspace must be created in that session only")
	}

	receipt, err = fixture.merged.Execute(ctx, DefaultSession(), "op_2", PromptAgentCommand{Target: "work::w1:p1", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Updated) != 1 || receipt.Updated[0] != (EntityRef{Kind: EntityAgent, ID: "codex-test"}) {
		t.Fatalf("an agent name is not a route and stays bare: %+v", receipt.Updated)
	}

	receipt, err = fixture.merged.Execute(ctx, DefaultSession(), "op_3", CreateConversationCommand{CWD: "/tmp", Label: "new"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range receipt.Created {
		if ref.Kind == EntityWorkspace && ref.ID != "w2" {
			t.Fatalf("a new conversation opens in the default session with a bare id, got %+v", receipt.Created)
		}
	}
}

func TestMergedQualifiesFaultTargetsAndWorktrees(t *testing.T) {
	missing := func(request scriptedRequest) scriptedReply {
		if request.Method == "pane.close" {
			return scriptedReply{Error: &scriptedError{Code: "pane_not_found", Message: "gone"}}
		}
		return standardReply(request)
	}
	fixture := startMergedFixture(t, standardReply, missing)
	ctx := context.Background()
	fault := &Fault{Code: CodeNotFound, Target: &EntityRef{Kind: EntityPane, ID: "w1:p1"}}
	qualified, _ := AsFault(qualifyFault(NamedSession("work"), fault))
	if qualified.Target.ID != "work::w1:p1" || fault.Target.ID != "w1:p1" {
		t.Fatalf("fault target must be qualified on a copy: %+v / %+v", qualified.Target, fault.Target)
	}
	if _, err := fixture.merged.Execute(ctx, DefaultSession(), "op_1", ClosePaneCommand{PaneID: "work::w1:p9"}); err == nil {
		t.Fatal("want the named session's not-found fault")
	}

	view, err := fixture.merged.Observe(ctx, DefaultSession(), WorktreeListQuery{WorkspaceID: "work::w1", CWD: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	list := view.(WorktreeListView)
	if list.Source.SourceWorkspace != "work::w1" {
		t.Fatalf("worktree source workspace must be qualified, got %q", list.Source.SourceWorkspace)
	}
}

func TestMergedDescribeIntersectsCapabilitiesAndUnionsAgents(t *testing.T) {
	old := func(request scriptedRequest) scriptedReply {
		switch request.Method {
		case "session.snapshot":
			return standardReply(request) // protocol 19, Herdr 0.8.0
		case "server.agent_manifests":
			return scriptedReply{Result: map[string]any{"type": "agent_manifest_status", "manifests": []any{map[string]any{"agent": "grok"}}}}
		}
		return standardReply(request)
	}
	fixture := startMergedFixture(t, mergedReply("home", "/home"), old)
	descriptor, err := fixture.merged.Describe(context.Background(), DefaultSession())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Protocol != 20 || descriptor.Supports(FeatureAgentInspect) {
		t.Fatalf("agent inspect needs Herdr 0.8.2 in every session: %+v", descriptor)
	}
	var kinds []string
	for _, kind := range descriptor.AgentKinds {
		kinds = append(kinds, kind.Kind)
	}
	if want := []string{"claude", "codex", "grok"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("agent kinds = %v, want %v", kinds, want)
	}
}

func TestQualifySnapshotRewritesLayoutsWithoutTouchingTheSource(t *testing.T) {
	source := Snapshot{
		Focused:    map[string]string{"workspace_id": "w1", "tab_id": "w1:t1", "pane_id": "w1:p1"},
		Workspaces: []Workspace{{WorkspaceID: "w1"}},
		Layouts: []TabLayout{{
			WorkspaceID: "w1", TabID: "w1:t1", FocusedPaneID: "w1:p1",
			Panes: []LayoutPane{{PaneID: "w1:p1"}}, Splits: []LayoutSplit{{ID: "s1"}},
		}},
	}
	out := qualifySnapshot(NamedSession("work"), source)
	layout := out.Layouts[0]
	if layout.WorkspaceID != "work::w1" || layout.TabID != "work::w1:t1" || layout.FocusedPaneID != "work::w1:p1" || layout.Panes[0].PaneID != "work::w1:p1" {
		t.Fatalf("layout ids must be qualified: %+v", layout)
	}
	if out.Focused["tab_id"] != "work::w1:t1" || out.Workspaces[0].Session != "work" {
		t.Fatalf("focus and session marker: %+v %+v", out.Focused, out.Workspaces)
	}
	if source.Layouts[0].Panes[0].PaneID != "w1:p1" || source.Focused["pane_id"] != "w1:p1" || source.Workspaces[0].Session != "" {
		t.Fatal("the session's own snapshot must not change")
	}
	if same := qualifySnapshot(DefaultSession(), source); !reflect.DeepEqual(same, source) {
		t.Fatal("the default session passes through unchanged")
	}
}

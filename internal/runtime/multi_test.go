package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sessionReply scripts one Herdr session whose single workspace carries the
// given label, so two sessions with identical bare ids stay distinguishable.
func sessionReply(label string) func(scriptedRequest) scriptedReply {
	return func(request scriptedRequest) scriptedReply {
		if request.Method != "session.snapshot" {
			return standardReply(request)
		}
		reply := standardReply(request)
		snapshot := reply.Result.(map[string]any)["snapshot"].(map[string]any)
		snapshot["workspaces"] = []any{map[string]any{"workspace_id": "w1", "number": 1, "label": label, "agent_status": "idle"}}
		return reply
	}
}

// multiRoot builds a short config root (unix socket paths are length-limited)
// with the named sessions live and returns the aggregate over it.
func multiRoot(t *testing.T, sessions map[string]func(scriptedRequest) scriptedReply) (*Multi, map[string]*requestLog) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pfm-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	logs := map[string]*requestLog{}
	for name, handler := range sessions {
		dir := filepath.Join(root, "sessions", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		logs[name] = startScriptedHerdrAt(t, filepath.Join(dir, "herdr.sock"), handler)
	}
	m := NewMulti(root)
	m.ttl = 0
	return m, logs
}

func methods(log *requestLog) []string {
	out := []string{}
	for _, request := range log.snapshot() {
		out = append(out, request.Method)
	}
	return out
}

func paramsOf(t *testing.T, log *requestLog, method string) map[string]any {
	t.Helper()
	for _, request := range log.snapshot() {
		if request.Method != method {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		return params
	}
	t.Fatalf("no %s request recorded", method)
	return nil
}

func TestMultiListsLiveSessionsWithCurrentFirst(t *testing.T) {
	m, _ := multiRoot(t, map[string]func(scriptedRequest) scriptedReply{"alpha": sessionReply("A"), "beta": sessionReply("B")})
	// A dead socket file must not be listed.
	dead := filepath.Join(m.herdr.ConfigRoot, "sessions", "gone")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dead, "herdr.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := m.Sessions(context.Background())
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("sessions = %v, want [alpha beta]", got)
	}
	m.current = "beta"
	got = m.Sessions(context.Background())
	if len(got) != 2 || got[0] != "beta" || got[1] != "alpha" {
		t.Fatalf("sessions with current=beta = %v, want [beta alpha]", got)
	}
}

func TestMultiSnapshotQualifiesEveryIDAndKeepsSessions(t *testing.T) {
	m, _ := multiRoot(t, map[string]func(scriptedRequest) scriptedReply{"alpha": sessionReply("A"), "beta": sessionReply("B")})
	view, err := m.Observe(context.Background(), DefaultSession(), SnapshotQuery{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := view.(SnapshotView).Snapshot
	if snapshot.HerdrVersion != "0.8.0" || snapshot.HerdrProtocol != 19 || snapshot.Session != nil {
		t.Fatalf("unexpected header: %+v", snapshot)
	}
	if len(snapshot.Workspaces) != 2 || snapshot.Workspaces[0].WorkspaceID != "alpha::w1" || snapshot.Workspaces[1].WorkspaceID != "beta::w1" {
		t.Fatalf("workspaces = %+v", snapshot.Workspaces)
	}
	if snapshot.Workspaces[0].Label != "A" || snapshot.Workspaces[1].Label != "B" || snapshot.Workspaces[0].Session != "alpha" || snapshot.Workspaces[1].Session != "beta" {
		t.Fatalf("labels/sessions = %+v", snapshot.Workspaces)
	}
	if len(snapshot.Tabs) != 2 || snapshot.Tabs[1].TabID != "beta::w1:t1" || snapshot.Tabs[1].WorkspaceID != "beta::w1" || snapshot.Tabs[1].Session != "beta" {
		t.Fatalf("tabs = %+v", snapshot.Tabs)
	}
	if len(snapshot.Panes) != 2 || snapshot.Panes[0].PaneID != "alpha::w1:p1" || snapshot.Panes[0].TabID != "alpha::w1:t1" || snapshot.Panes[0].WorkspaceID != "alpha::w1" || snapshot.Panes[0].Session != "alpha" {
		t.Fatalf("panes = %+v", snapshot.Panes)
	}
	if !snapshot.Panes[0].HistoryAvailable || snapshot.Panes[0].AgentSession == nil {
		t.Fatalf("pane transcript ref lost in merge: %+v", snapshot.Panes[0])
	}
	if snapshot.Focused["pane_id"] != "alpha::w1:p1" || snapshot.Focused["workspace_id"] != "alpha::w1" {
		t.Fatalf("focused = %v", snapshot.Focused)
	}
	if len(snapshot.Sessions) != 2 || snapshot.Sessions[0] != (SessionInfo{Name: "alpha", Workspaces: 1, Panes: 1}) || snapshot.Sessions[1].Name != "beta" {
		t.Fatalf("sessions = %+v", snapshot.Sessions)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["sessions"]; !ok {
		t.Fatalf("sessions missing on the wire: %s", raw)
	}
}

func TestMultiRoutesCommandsToTheNamedSessionOnly(t *testing.T) {
	m, logs := multiRoot(t, map[string]func(scriptedRequest) scriptedReply{"alpha": sessionReply("A"), "beta": sessionReply("B")})
	ctx := context.Background()
	receipt, err := m.Execute(ctx, DefaultSession(), "op_1234567890abcdef", SendTextCommand{PaneID: "beta::w1:p1", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if params := paramsOf(t, logs["beta"], "pane.send_text"); params["pane_id"] != "w1:p1" {
		t.Fatalf("beta received pane_id %v, want bare w1:p1", params["pane_id"])
	}
	for _, method := range methods(logs["alpha"]) {
		if method == "pane.send_text" {
			t.Fatal("alpha must not receive beta's mutation")
		}
	}
	if len(receipt.Updated) != 1 || receipt.Updated[0].ID != "beta::w1:p1" {
		t.Fatalf("receipt = %+v, want re-qualified pane id", receipt)
	}

	view, err := m.Observe(ctx, DefaultSession(), PaneReadQuery{PaneID: "alpha::w1:p1"})
	if err != nil {
		t.Fatal(err)
	}
	if view.(PaneReadView).Text != "ready" {
		t.Fatalf("pane read = %+v", view)
	}
	if params := paramsOf(t, logs["alpha"], "pane.read"); params["pane_id"] != "w1:p1" {
		t.Fatalf("alpha pane.read pane_id = %v", params["pane_id"])
	}

	created, err := m.Execute(ctx, DefaultSession(), "op_1234567890abcdef", CreateTabCommand{WorkspaceID: "alpha::w1", CWD: "/tmp/lab", Label: "two"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[EntityKind]string{}
	for _, ref := range created.Created {
		ids[ref.Kind] = ref.ID
	}
	if ids[EntityTab] != "alpha::w1:t2" || ids[EntityPane] != "alpha::w1:p2" {
		t.Fatalf("created refs = %+v", created.Created)
	}

	worktrees, err := m.Observe(ctx, DefaultSession(), WorktreeListQuery{WorkspaceID: "beta::w1"})
	if err != nil {
		t.Fatal(err)
	}
	list := worktrees.(WorktreeListView)
	if list.Source.SourceWorkspace != "beta::w1" {
		t.Fatalf("worktree source workspace = %q", list.Source.SourceWorkspace)
	}
}

func TestMultiRejectsBareAndUnknownIDs(t *testing.T) {
	m, logs := multiRoot(t, map[string]func(scriptedRequest) scriptedReply{"alpha": sessionReply("A")})
	ctx := context.Background()
	_, err := m.Execute(ctx, DefaultSession(), "op_1234567890abcdef", ClosePaneCommand{PaneID: "w1:p1"})
	if fault, ok := err.(*Fault); !ok || fault.Code != CodeInvalid {
		t.Fatalf("bare id error = %v, want invalid fault", err)
	}
	_, err = m.Execute(ctx, DefaultSession(), "op_1234567890abcdef", ClosePaneCommand{PaneID: "nope::w1:p1"})
	if fault, ok := err.(*Fault); !ok || fault.Code != CodeNotFound {
		t.Fatalf("unknown session error = %v, want not found fault", err)
	}
	_, err = m.Execute(ctx, DefaultSession(), "op_1234567890abcdef", ClosePaneCommand{PaneID: "../x::w1:p1"})
	if fault, ok := err.(*Fault); !ok || fault.Code != CodeInvalid {
		t.Fatalf("traversal error = %v, want invalid fault", err)
	}
	for _, method := range methods(logs["alpha"]) {
		if method == "pane.close" {
			t.Fatal("no close may reach Herdr for a rejected id")
		}
	}
}

func TestMultiDescribeUnionsAgentKindsAndIntersectsCapabilities(t *testing.T) {
	old := func(request scriptedRequest) scriptedReply {
		reply := standardReply(request)
		switch request.Method {
		case "session.snapshot":
			reply.Result.(map[string]any)["snapshot"].(map[string]any)["protocol"] = 18
		case "server.agent_manifests":
			reply.Result = map[string]any{"type": "agent_manifest_status", "manifests": []any{map[string]any{"agent": "grok"}}}
		}
		return reply
	}
	m, _ := multiRoot(t, map[string]func(scriptedRequest) scriptedReply{"alpha": sessionReply("A"), "old": old})
	descriptor, err := m.Describe(context.Background(), DefaultSession())
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, kind := range descriptor.AgentKinds {
		kinds = append(kinds, kind.Kind)
	}
	if len(kinds) != 3 || kinds[0] != "claude" || kinds[1] != "codex" || kinds[2] != "grok" {
		t.Fatalf("agent kinds = %v", kinds)
	}
	if descriptor.Capabilities[FeatureSnapshot].Available != true {
		t.Fatalf("snapshot must stay available: %+v", descriptor.Capabilities[FeatureSnapshot])
	}
	if descriptor.Capabilities[FeatureCreateTab].Available {
		t.Fatalf("create tab must be withheld when one session is protocol 18: %+v", descriptor.Capabilities[FeatureCreateTab])
	}
}

func TestMultiWithoutLiveSessionsIsOffline(t *testing.T) {
	m, _ := multiRoot(t, nil)
	_, err := m.Observe(context.Background(), DefaultSession(), SnapshotQuery{})
	if fault, ok := err.(*Fault); !ok || fault.Code != CodeOffline {
		t.Fatalf("error = %v, want offline fault", err)
	}
}

func TestSessionForSocketOnlyAcceptsPathsInsideTheRoot(t *testing.T) {
	root := "/home/u/.config/herdr"
	cases := map[string]string{
		"":                                       "",
		root + "/herdr.sock":                     rootSessionName,
		root + "/sessions/first_dev/herdr.sock":  "first_dev",
		root + "/sessions/first_dev/other.sock":  "",
		root + "/sessions/default/herdr.sock":    "",
		"/elsewhere/sessions/x/herdr.sock":       "",
		root + "/sessions/../../evil/herdr.sock": "",
	}
	for socket, want := range cases {
		if got := sessionForSocket(root, socket); got != want {
			t.Errorf("sessionForSocket(%q) = %q, want %q", socket, got, want)
		}
	}
}

func TestMultiDiscoveryCacheExpires(t *testing.T) {
	m, _ := multiRoot(t, map[string]func(scriptedRequest) scriptedReply{"alpha": sessionReply("A")})
	m.ttl = time.Hour
	if got := m.Sessions(context.Background()); len(got) != 1 {
		t.Fatalf("sessions = %v", got)
	}
	dir := filepath.Join(m.herdr.ConfigRoot, "sessions", "late")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	startScriptedHerdrAt(t, filepath.Join(dir, "herdr.sock"), sessionReply("L"))
	if got := m.Sessions(context.Background()); len(got) != 1 {
		t.Fatalf("cached sessions = %v, want the stale single entry", got)
	}
	// A qualified id for the new session forces a rescan instead of a false 404.
	if _, err := m.Observe(context.Background(), DefaultSession(), PaneReadQuery{PaneID: "late::w1:p1"}); err != nil {
		t.Fatalf("fresh session must be reachable: %v", err)
	}
}

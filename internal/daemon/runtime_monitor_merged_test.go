package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"pairfob/internal/runtime"
)

// serveMergedSession answers session.snapshot on socket with one Codex pane
// whose status is read from status on every call.
func serveMergedSession(t *testing.T, socket, label string, status *atomic.Value) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				scanner := bufio.NewScanner(conn)
				if !scanner.Scan() {
					return
				}
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if json.Unmarshal(scanner.Bytes(), &request) != nil {
					return
				}
				reply := map[string]any{"id": request.ID, "error": map[string]string{"code": "unknown_method", "message": request.Method}}
				if request.Method == "session.snapshot" {
					reply = map[string]any{"id": request.ID, "result": map[string]any{
						"type": "session_snapshot",
						"snapshot": map[string]any{
							"version": "0.8.2", "protocol": 20,
							"workspaces": []any{map[string]any{"workspace_id": "w1", "number": 1, "label": label, "agent_status": status.Load()}},
							"tabs":       []any{map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "label": "main"}},
							"panes": []any{map[string]any{
								"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1", "cwd": "/" + label,
								"agent": "codex", "agent_status": status.Load(),
							}},
						},
					}}
				}
				raw, _ := json.Marshal(reply)
				_, _ = conn.Write(append(raw, '\n'))
			}(conn)
		}
	}()
}

// With merged sessions every running session is part of the default view, so
// the default monitor, the only one allowed to push, notifies for a pane of a
// named session too, and the notification names the merged-view pane id the
// phone resolves.
func TestMergedSessionsNotifyFromEveryRunningSession(t *testing.T) {
	configRoot, err := os.MkdirTemp("", "pfm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(configRoot) })
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	var home, work atomic.Value
	home.Store("idle")
	work.Store("working")
	serveMergedSession(t, filepath.Join(configRoot, "herdr.sock"), "home", &home)
	serveMergedSession(t, filepath.Join(configRoot, "sessions", "work", "herdr.sock"), "lab", &work)
	h := runtime.NewHerdr(filepath.Join(configRoot, "herdr.sock"))
	h.ConfigRoot = configRoot
	engine := NewEngine(nil, nil, runtime.NewMerged(h))
	engine.PushEnabled = true

	var pushes []HerdPush
	emitPush := func(event HerdPush) { pushes = append(pushes, event) }
	state := sessionMonitorState{panes: map[string]monitoredPane{}}
	engine.reconcileRuntimeSession(context.Background(), runtime.DefaultSession(), true, &state, func(string, string) {}, emitPush)
	if _, ok := state.panes["work::w1:p1"]; !ok || len(state.panes) != 2 {
		t.Fatalf("the default monitor must watch both sessions, got %v", paneMembership(state.panes))
	}
	if len(pushes) != 0 {
		t.Fatalf("first observation must not notify: %+v", pushes)
	}

	work.Store("blocked")
	engine.reconcileRuntimeSession(context.Background(), runtime.DefaultSession(), true, &state, func(string, string) {}, emitPush)
	if len(pushes) != 1 || pushes[0].HerdID != "work::w1:p1" || pushes[0].Kind != PushNeedsYou || pushes[0].WorkspaceLabel != "lab" {
		t.Fatalf("want one needs-you push for the named session's pane, got %+v", pushes)
	}

	home.Store("blocked")
	engine.reconcileRuntimeSession(context.Background(), runtime.DefaultSession(), true, &state, func(string, string) {}, emitPush)
	if len(pushes) != 2 || pushes[1].HerdID != "w1:p1" {
		t.Fatalf("the default session keeps its bare pane id, got %+v", pushes)
	}
	if engine.RuntimeKind() != "herdr" {
		t.Fatalf("RuntimeKind = %q", engine.RuntimeKind())
	}
}

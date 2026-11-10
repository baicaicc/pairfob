package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHerdrPaneProcessInfoDecodesRealShapeExactly(t *testing.T) {
	wantArgv := []string{"/opt/node/bin/node", "/repo/cursor-agent.js", "binding-7"}
	socket, log := startScriptedHerdr(t, func(request scriptedRequest) scriptedReply {
		return scriptedReply{Result: map[string]any{
			"type": "pane_process_info",
			"process_info": map[string]any{
				"pane_id": "w4:p1", "shell_pid": 72066, "foreground_process_group_id": 84702,
				"foreground_processes": []any{map[string]any{
					"pid": 84702, "argv": wantArgv, "cwd": "/repo/cursor",
					"name": "node", "argv0": "node", "cmdline": strings.Join(wantArgv, " "),
				}},
			},
		}}
	})

	view, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w4:p1"})
	if err != nil {
		t.Fatal(err)
	}
	info, ok := view.(PaneProcessInfoView)
	if !ok || info.PaneID != "w4:p1" || pointerIntValue(info.ShellPID) != 72066 || pointerIntValue(info.ForegroundProcessGroupID) != 84702 || len(info.ForegroundProcesses) != 1 {
		t.Fatalf("view=%T %+v", view, view)
	}
	process := info.ForegroundProcesses[0]
	if process.PID != 84702 || !reflect.DeepEqual(process.Argv, wantArgv) || process.Cwd == nil || *process.Cwd != "/repo/cursor" {
		t.Fatalf("process=%+v", process)
	}
	requests := log.snapshot()
	if len(requests) != 1 || requests[0].ID != "pairfob" || requests[0].Method != "pane.process_info" {
		t.Fatalf("requests=%+v", requests)
	}
	var params map[string]any
	if err := json.Unmarshal(requests[0].Params, &params); err != nil || len(params) != 1 || params["pane_id"] != "w4:p1" {
		t.Fatalf("params=%s err=%v", requests[0].Params, err)
	}
}

func TestHerdrPaneProcessInfoPreservesIncompleteStates(t *testing.T) {
	tests := []struct {
		name      string
		processes any
		check     func(*testing.T, PaneProcessInfoView)
	}{
		{name: "omitted process list", check: func(t *testing.T, view PaneProcessInfoView) {
			if view.ShellPID != nil || view.ForegroundProcessGroupID != nil || view.ForegroundProcesses != nil {
				t.Fatalf("unknown state changed: %+v", view)
			}
		}},
		{name: "empty process list", processes: []any{}, check: func(t *testing.T, view PaneProcessInfoView) {
			if view.ForegroundProcesses == nil || len(view.ForegroundProcesses) != 0 {
				t.Fatalf("empty process list changed: %#v", view.ForegroundProcesses)
			}
		}},
		{name: "nullable process fields", processes: []any{map[string]any{"pid": 1, "argv": nil, "cwd": nil}}, check: func(t *testing.T, view PaneProcessInfoView) {
			process := view.ForegroundProcesses[0]
			if process.Argv != nil || process.Cwd != nil {
				t.Fatalf("nullable process state changed: %+v", process)
			}
		}},
		{name: "empty argv", processes: []any{map[string]any{"pid": 1, "argv": []string{}, "cwd": "/"}}, check: func(t *testing.T, view PaneProcessInfoView) {
			process := view.ForegroundProcesses[0]
			if process.Argv == nil || len(process.Argv) != 0 || process.Cwd == nil || *process.Cwd != "/" {
				t.Fatalf("empty argv changed: %+v", process)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket, _ := startScriptedHerdr(t, func(scriptedRequest) scriptedReply {
				body := map[string]any{"pane_id": "w1:p1"}
				if test.processes != nil {
					body["foreground_processes"] = test.processes
				}
				return scriptedReply{Result: map[string]any{"type": "pane_process_info", "process_info": body}}
			})
			view, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w1:p1"})
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, view.(PaneProcessInfoView))
		})
	}
}

func TestHerdrPaneProcessInfoRejectsInvalidQueryBeforeCall(t *testing.T) {
	socket, log := startScriptedHerdr(t, func(request scriptedRequest) scriptedReply {
		t.Fatalf("unexpected request: %+v", request)
		return scriptedReply{}
	})
	for _, paneID := range []string{"", "bad/id", strings.Repeat("p", 257)} {
		_, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: paneID})
		assertProcessInfoFault(t, err, CodeInvalid)
	}
	if requests := log.snapshot(); len(requests) != 0 {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestHerdrPaneProcessInfoRejectsInvalidEnvelopeShape(t *testing.T) {
	tests := []struct {
		name   string
		result any
	}{
		{name: "wrong discriminator", result: map[string]any{"type": "pane_info", "process_info": map[string]any{"pane_id": "w1:p1"}}},
		{name: "missing body", result: map[string]any{"type": "pane_process_info"}},
		{name: "null body", result: map[string]any{"type": "pane_process_info", "process_info": nil}},
		{name: "wrong target", result: processInfoResult("w1:p2", nil)},
		{name: "invalid target", result: processInfoResult("bad/id", nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket, _ := startScriptedHerdr(t, func(scriptedRequest) scriptedReply { return scriptedReply{Result: test.result} })
			_, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w1:p1"})
			assertProcessInfoFault(t, err, CodeInternal)
		})
	}
}

func TestHerdrPaneProcessInfoEnforcesIdentityBounds(t *testing.T) {
	manyProcesses := make([]any, maxPaneForegroundProcesses+1)
	for i := range manyProcesses {
		manyProcesses[i] = map[string]any{"pid": i + 1}
	}
	manyArgs := make([]string, maxPaneProcessArgv+1)
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "zero shell pid", body: map[string]any{"shell_pid": 0}},
		{name: "negative shell pid", body: map[string]any{"shell_pid": -1}},
		{name: "large shell pid", body: map[string]any{"shell_pid": int64(maxPaneProcessPID) + 1}},
		{name: "zero process group", body: map[string]any{"foreground_process_group_id": 0}},
		{name: "negative process group", body: map[string]any{"foreground_process_group_id": -1}},
		{name: "large process group", body: map[string]any{"foreground_process_group_id": int64(maxPaneProcessPID) + 1}},
		{name: "missing process pid", body: foregroundBody(map[string]any{})},
		{name: "zero process pid", body: foregroundBody(map[string]any{"pid": 0})},
		{name: "negative process pid", body: foregroundBody(map[string]any{"pid": -1})},
		{name: "large process pid", body: foregroundBody(map[string]any{"pid": int64(maxPaneProcessPID) + 1})},
		{name: "duplicate process pid", body: foregroundBody(map[string]any{"pid": 7}, map[string]any{"pid": 7})},
		{name: "too many processes", body: map[string]any{"foreground_processes": manyProcesses}},
		{name: "too many arguments", body: foregroundBody(map[string]any{"pid": 1, "argv": manyArgs})},
		{name: "oversized argument", body: foregroundBody(map[string]any{"pid": 1, "argv": []string{strings.Repeat("a", maxPaneProcessArgBytes+1)}})},
		{name: "oversized total arguments", body: foregroundBody(map[string]any{"pid": 1, "argv": []string{strings.Repeat("a", maxPaneProcessArgBytes), strings.Repeat("b", maxPaneProcessArgBytes), strings.Repeat("c", maxPaneProcessArgBytes), strings.Repeat("d", maxPaneProcessArgBytes), "e"}})},
		{name: "nul argument", body: foregroundBody(map[string]any{"pid": 1, "argv": []string{"a\x00b"}})},
		{name: "empty cwd", body: foregroundBody(map[string]any{"pid": 1, "cwd": ""})},
		{name: "relative cwd", body: foregroundBody(map[string]any{"pid": 1, "cwd": "repo"})},
		{name: "unclean cwd", body: foregroundBody(map[string]any{"pid": 1, "cwd": "/repo/../other"})},
		{name: "oversized cwd", body: foregroundBody(map[string]any{"pid": 1, "cwd": "/" + strings.Repeat("a", maxPaneProcessCwdBytes)})},
		{name: "nul cwd", body: foregroundBody(map[string]any{"pid": 1, "cwd": "/repo\x00bad"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.body["pane_id"] = "w1:p1"
			socket, _ := startScriptedHerdr(t, func(scriptedRequest) scriptedReply {
				return scriptedReply{Result: map[string]any{"type": "pane_process_info", "process_info": test.body}}
			})
			_, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w1:p1"})
			assertProcessInfoFault(t, err, CodeInternal)
		})
	}
}

func TestHerdrPaneProcessInfoAcceptsExactBounds(t *testing.T) {
	processes := make([]any, maxPaneForegroundProcesses)
	for i := range processes {
		process := map[string]any{"pid": i + 1}
		if i == 0 {
			args := make([]string, maxPaneProcessArgv)
			for j := 0; j < 4; j++ {
				args[j] = strings.Repeat(string(rune('a'+j)), maxPaneProcessArgBytes)
			}
			process["argv"] = args
			process["cwd"] = "/" + strings.Repeat("c", maxPaneProcessCwdBytes-1)
		}
		processes[i] = process
	}
	body := map[string]any{
		"pane_id": "w1:p1", "shell_pid": maxPaneProcessPID,
		"foreground_process_group_id": maxPaneProcessPID, "foreground_processes": processes,
	}
	socket, _ := startScriptedHerdr(t, func(scriptedRequest) scriptedReply {
		return scriptedReply{Result: map[string]any{"type": "pane_process_info", "process_info": body}}
	})
	view, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w1:p1"})
	if err != nil || len(view.(PaneProcessInfoView).ForegroundProcesses) != maxPaneForegroundProcesses {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestPaneProcessInfoIdentityIsPrivateJSON(t *testing.T) {
	cwd := "/secret/repository"
	values := []any{
		PaneProcessInfoQuery{PaneID: "w9:p1"},
		PaneForegroundProcess{PID: 22, Argv: []string{"secret-token"}, Cwd: &cwd},
		PaneProcessInfoView{PaneID: "w9:p1", ShellPID: pointerToInt(11), ForegroundProcessGroupID: pointerToInt(22), ForegroundProcesses: []PaneForegroundProcess{{PID: 22, Argv: []string{"secret-token"}, Cwd: &cwd}}},
	}
	for _, value := range values {
		raw, err := json.Marshal(value)
		if err != nil || string(raw) != "{}" {
			t.Fatalf("%T leaked private JSON: %s err=%v", value, raw, err)
		}
	}
	var query PaneProcessInfoQuery
	var process PaneForegroundProcess
	var view PaneProcessInfoView
	for _, target := range []any{&query, &process, &view} {
		if err := json.Unmarshal([]byte(`{"PaneID":"attacker","PID":9,"Argv":["attacker"],"Cwd":"/attacker","ShellPID":8,"ForegroundProcessGroupID":9,"ForegroundProcesses":[{"PID":9}]}`), target); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(query, PaneProcessInfoQuery{}) || !reflect.DeepEqual(process, PaneForegroundProcess{}) || !reflect.DeepEqual(view, PaneProcessInfoView{}) {
		t.Fatalf("public JSON populated private identity: query=%+v process=%+v view=%+v", query, process, view)
	}
}

func TestFakePaneProcessInfoIsExplicitlyUnsupported(t *testing.T) {
	view, err := NewFake().Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w0:p1"})
	if view != nil {
		t.Fatalf("view=%+v", view)
	}
	fault := assertProcessInfoFault(t, err, CodeUnsupported)
	if fault.Operation != "pane.process_info" {
		t.Fatalf("fault=%+v", fault)
	}
}

// The terminal core runtime has no pane.process_info; it must refuse the
// query rather than invent an empty observation.
func TestCorePaneProcessInfoIsUnsupported(t *testing.T) {
	view, err := NewCore("/nonexistent/hc").Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w0:p1"})
	if view != nil {
		t.Fatalf("view=%+v", view)
	}
	assertProcessInfoFault(t, err, CodeUnsupported)
}

func TestHerdrPaneProcessInfoPreservesTransportContextAndSessionSemantics(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		socket := shortTestSocket(t)
		_, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), PaneProcessInfoQuery{PaneID: "w1:p1"})
		fault := assertProcessInfoFault(t, err, CodeOffline)
		if fault.Operation != "pane.process_info" || fault.Outcome != OutcomeNotApplied || fault.Retry != RetryReadSafe {
			t.Fatalf("fault=%+v", fault)
		}
	})
	t.Run("context deadline", func(t *testing.T) {
		socket, _ := startScriptedHerdr(t, func(scriptedRequest) scriptedReply {
			return scriptedReply{Result: processInfoResult("w1:p1", nil), Delay: 100 * time.Millisecond}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := NewHerdr(socket).Observe(ctx, DefaultSession(), PaneProcessInfoQuery{PaneID: "w1:p1"})
		fault := assertProcessInfoFault(t, err, CodeTimeout)
		if fault.Operation != "pane.process_info" || fault.Outcome != OutcomeNotApplied || fault.Retry != RetryReadSafe {
			t.Fatalf("fault=%+v", fault)
		}
	})
	t.Run("named session", func(t *testing.T) {
		root, err := os.MkdirTemp("", "pfi-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		socket := filepath.Join(root, "sessions", "alpha", "herdr.sock")
		if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
			t.Fatal(err)
		}
		log := startScriptedHerdrAt(t, socket, func(scriptedRequest) scriptedReply {
			return scriptedReply{Result: processInfoResult("w1:p1", nil)}
		})
		herdr := NewHerdr(filepath.Join(root, "unused.sock"))
		herdr.Multi = true
		herdr.ConfigRoot = root
		if _, err := herdr.Observe(context.Background(), NamedSession("alpha"), PaneProcessInfoQuery{PaneID: "w1:p1"}); err != nil {
			t.Fatal(err)
		}
		requests := log.snapshot()
		if len(requests) != 1 || strings.Contains(string(requests[0].Params), "alpha") {
			t.Fatalf("requests=%+v", requests)
		}
	})
	t.Run("canceled before call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := NewHerdr(shortTestSocket(t)).Observe(ctx, DefaultSession(), PaneProcessInfoQuery{PaneID: "w1:p1"})
		fault := assertProcessInfoFault(t, err, CodeOffline)
		if !errors.Is(fault.Cause, context.Canceled) {
			t.Fatalf("fault=%+v", fault)
		}
	})
}

func processInfoResult(paneID string, processes any) map[string]any {
	body := map[string]any{"pane_id": paneID}
	if processes != nil {
		body["foreground_processes"] = processes
	}
	return map[string]any{"type": "pane_process_info", "process_info": body}
}

func foregroundBody(processes ...any) map[string]any {
	return map[string]any{"foreground_processes": processes}
}

func assertProcessInfoFault(t *testing.T, err error, code ErrorCode) *Fault {
	t.Helper()
	fault, ok := AsFault(err)
	if !ok || fault.Code != code {
		t.Fatalf("err=%v fault=%+v want code=%s", err, fault, code)
	}
	return fault
}

func pointerIntValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func pointerToInt(value int) *int {
	return &value
}

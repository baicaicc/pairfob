package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHerdrTerminalIDSurvivesNormalizationAndClone(t *testing.T) {
	const terminalID = "term_01:trusted"
	socket, _ := startScriptedHerdr(t, func(request scriptedRequest) scriptedReply {
		reply := standardReply(request)
		if request.Method == "session.snapshot" {
			setTestSnapshotTerminalID(reply.Result, pointerToString(terminalID))
		}
		return reply
	})

	view, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), SnapshotQuery{})
	if err != nil {
		t.Fatal(err)
	}
	pane := view.(SnapshotView).Snapshot.Panes[0]
	if pane.TerminalID != terminalID || pane.PaneID != "w1:p1" || pane.Agent != "codex" || pane.Cwd != "/tmp/lab" {
		t.Fatalf("normalized pane=%+v", pane)
	}
	for _, value := range []any{pane, view.(SnapshotView).Snapshot} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "terminal_id") || strings.Contains(string(encoded), terminalID) {
			t.Fatalf("trusted terminal identity leaked in JSON: %s", encoded)
		}
	}
	var public Pane
	if err := json.Unmarshal([]byte(`{"pane_id":"w1:p1","terminal_id":"attacker"}`), &public); err != nil {
		t.Fatal(err)
	}
	if public.TerminalID != "" {
		t.Fatalf("public JSON populated trusted terminal identity: %+v", public)
	}

	fake := NewFake()
	fake.Snap.Panes[0].TerminalID = terminalID
	cloned := observeFakeSnapshot(t, fake)
	if cloned.Panes[0].TerminalID != terminalID {
		t.Fatalf("snapshot clone lost terminal identity: %+v", cloned.Panes[0])
	}
	cloned.Panes[0].TerminalID = "external"
	current := observeFakeSnapshot(t, fake)
	if current.Panes[0].TerminalID != terminalID {
		t.Fatalf("snapshot clone mutation escaped: %+v", current.Panes[0])
	}
}

func TestHerdrTerminalIDOptionalForOlderSnapshots(t *testing.T) {
	cases := []struct {
		name string
		id   *string
	}{
		{name: "omitted"},
		{name: "empty", id: pointerToString("")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			socket, _ := startScriptedHerdr(t, func(request scriptedRequest) scriptedReply {
				reply := standardReply(request)
				if request.Method == "session.snapshot" {
					setTestSnapshotTerminalID(reply.Result, test.id)
				}
				return reply
			})
			view, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), SnapshotQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if got := view.(SnapshotView).Snapshot.Panes[0].TerminalID; got != "" {
				t.Fatalf("terminal identity=%q", got)
			}
		})
	}
}

func TestHerdrRejectsInvalidTerminalID(t *testing.T) {
	for _, terminalID := range []string{"bad/id?", strings.Repeat("a", 257)} {
		t.Run(terminalID, func(t *testing.T) {
			socket, _ := startScriptedHerdr(t, func(request scriptedRequest) scriptedReply {
				reply := standardReply(request)
				if request.Method == "session.snapshot" {
					setTestSnapshotTerminalID(reply.Result, &terminalID)
				}
				return reply
			})
			_, err := NewHerdr(socket).Observe(context.Background(), DefaultSession(), SnapshotQuery{})
			fault, ok := AsFault(err)
			if !ok || fault.Code != CodeInternal || !strings.Contains(fault.SafeMessage, "terminal id") {
				t.Fatalf("err=%v fault=%+v", err, fault)
			}
		})
	}
}

func setTestSnapshotTerminalID(result any, terminalID *string) {
	if terminalID == nil {
		return
	}
	response := result.(map[string]any)
	snapshot := response["snapshot"].(map[string]any)
	pane := snapshot["panes"].([]any)[0].(map[string]any)
	pane["terminal_id"] = *terminalID
}

func pointerToString(value string) *string {
	return &value
}

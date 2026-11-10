//go:build darwin || linux

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pairfob/internal/phone"
	"pairfob/internal/runtime"
	"pairfob/internal/state"
)

type cursorOwnerHTTPFixture struct {
	t           *testing.T
	cursor      *cursorResolverFixture
	server      *http.Server
	listener    net.Listener
	mu          sync.Mutex
	calls       int
	requests    []map[string]any
	respond     func(http.ResponseWriter, map[string]any)
	beforeReply func()
}

func newCursorPromptRPCFixture(t *testing.T) (*cursorOwnerHTTPFixture, *Engine, *phone.Client) {
	t.Helper()
	f := newCursorResolverFixture(t)
	fixture := &cursorOwnerHTTPFixture{t: t, cursor: &f}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.listener = listener
	fixture.server = &http.Server{Handler: http.HandlerFunc(fixture.handle)}
	go func() { _ = fixture.server.Serve(listener) }()
	t.Cleanup(func() { _ = fixture.server.Close() })
	endpoint := "http://" + listener.Addr().String() + "/control"
	writeDaemonJSON(t, filepath.Join(f.manifest.OwnerDir, "intent.json"), map[string]any{
		"kind": "allinone-cursor-owner-v1", "ownerId": f.manifest.OwnerID, "caseId": f.manifest.CaseID,
		"cwd": f.manifest.Cwd, "nonce": "nonce-one", "credential": "credential-one", "actualExtra": map[string]any{"accepted": true},
	})
	writeDaemonJSON(t, filepath.Join(f.manifest.OwnerDir, "owner.json"), map[string]any{
		"ownerId": f.manifest.OwnerID, "nonce": "nonce-one", "endpoint": endpoint, "identity": map[string]any{
			"pid": f.manifest.OwnerIdentity.PID, "ppid": 7, "startTime": f.manifest.OwnerIdentity.StartTime,
			"bootId": f.manifest.OwnerIdentity.BootID, "executable": f.manifest.OwnerIdentity.Executable,
			"commandLine": f.manifest.OwnerIdentity.CommandLine,
		}, "publishedAt": "2026-09-06T12:00:00Z",
	})
	fixture.respond = fixture.success
	engine, client := runtimeRPCClient(t, f.runtime)
	engine.CursorConfig, engine.Journal.CursorACP = f.config, f.config
	engine.cursorIdentity = f.identityReader()
	engine.cursorHTTPTimeout = 2 * time.Second
	return fixture, engine, client
}

func (f *cursorOwnerHTTPFixture) handle(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.RequestURI() != "/control" || request.Host != f.listener.Addr().String() ||
		request.Header.Get("Authorization") != "Bearer credential-one" || request.Header.Get("Content-Type") != "application/json" ||
		request.Header.Get("Origin") != "" || request.ContentLength <= 0 || request.ContentLength > cursorControlRequestMax {
		f.t.Errorf("invalid owner request headers: method=%s uri=%s host=%s headers=%v length=%d", request.Method, request.URL.RequestURI(), request.Host, request.Header, request.ContentLength)
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		f.t.Error(err)
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || body["ownerId"] != f.cursor.manifest.OwnerID || body["caseId"] != f.cursor.manifest.CaseID ||
		body["cwd"] != f.cursor.manifest.Cwd || body["method"] != "prompt" {
		f.t.Errorf("invalid owner request body: %s", raw)
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	params, ok := body["params"].(map[string]any)
	operationID, operationOK := params["operationId"].(string)
	operationHash := sha256.Sum256([]byte(operationID))
	inputPath := filepath.Join(f.cursor.manifest.OwnerDir, "public-inputs", hex.EncodeToString(operationHash[:])+".json")
	if info, err := os.Stat(inputPath); !ok || !operationOK || err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		f.t.Errorf("public input was not durable before HTTP send: %s err=%v", inputPath, err)
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.calls++
	f.requests = append(f.requests, body)
	respond := f.respond
	f.mu.Unlock()
	respond(response, body)
}

func (f *cursorOwnerHTTPFixture) success(response http.ResponseWriter, body map[string]any) {
	params := body["params"].(map[string]any)
	operation := map[string]any{
		"ownerId": f.cursor.manifest.OwnerID, "caseId": f.cursor.manifest.CaseID, "cwd": f.cursor.manifest.Cwd,
		"nativeSessionId": f.cursor.manifest.NativeSessionID, "operationId": params["operationId"], "runId": params["runId"],
		"text": params["text"], "publicText": "owner answer", "status": "completed", "toolCalls": []any{}, "stopReason": "end_turn",
	}
	writeDaemonJSON(f.t, f.cursor.sessionPath, map[string]any{"schemaVersion": 1, "revision": 2, "savedAt": "2026-09-06T12:00:01Z", "session": map[string]any{
		"schemaVersion": 1, "ownerId": f.cursor.manifest.OwnerID, "cwd": f.cursor.manifest.Cwd,
		"nativeSessionId": f.cursor.manifest.NativeSessionID, "status": "ready", "operations": []any{operation},
	}})
	if f.beforeReply != nil {
		f.beforeReply()
	}
	f.writeEnvelope(response, map[string]any{"ok": true, "recorded": true, "operationId": params["operationId"], "operation": operation, "revision": 2, "savedAt": "2026-09-06T12:00:01Z"})
}

func (f *cursorOwnerHTTPFixture) writeEnvelope(response http.ResponseWriter, extra map[string]any) {
	identity := f.cursor.manifest.OwnerIdentity
	body := map[string]any{
		"ownerId": f.cursor.manifest.OwnerID, "caseId": f.cursor.manifest.CaseID, "cwd": f.cursor.manifest.Cwd,
		"identity": map[string]any{"pid": identity.PID, "startTime": identity.StartTime, "bootId": identity.BootID, "executable": identity.Executable, "commandLine": identity.CommandLine},
	}
	for key, value := range extra {
		body[key] = value
	}
	raw, _ := json.Marshal(body)
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	_, _ = response.Write(raw)
}

func (f *cursorOwnerHTTPFixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *cursorOwnerHTTPFixture) changeControlRecord() {
	identity := f.cursor.manifest.OwnerIdentity
	writeDaemonJSON(f.t, filepath.Join(f.cursor.manifest.OwnerDir, "owner.json"), map[string]any{
		"ownerId": f.cursor.manifest.OwnerID, "nonce": "replacement", "endpoint": "http://" + f.listener.Addr().String() + "/control",
		"identity": map[string]any{"pid": identity.PID, "ppid": 999, "startTime": identity.StartTime, "bootId": identity.BootID, "executable": identity.Executable, "commandLine": identity.CommandLine},
	})
}

func TestCursorPromptUsesOwnerControlAndPublishesLiteralInput(t *testing.T) {
	fixture, _, client := newCursorPromptRPCFixture(t)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	display, err := client.RPC("Snapshot", map[string]any{})
	if err != nil || !strings.Contains(string(display), `"agent":"Cursor ACP"`) {
		t.Fatalf("custom display precondition=%s err=%v", display, err)
	}
	text := "/quit\n/cancel\x00keep this literal\nsecond line"
	params := map[string]any{"operation_id": "op_cursorprompt000001", "pane_id": fixture.cursor.pane.PaneID, "text": text}
	first, err := client.RPC("PromptAgent", params)
	// Upstream acknowledges a prompt without inventing a working status.
	if err != nil || !strings.Contains(string(first), `"agent_status":"unknown"`) || !strings.Contains(string(first), `"outcome":"applied"`) {
		t.Fatalf("prompt result=%s err=%v", first, err)
	}
	if fixture.requestCount() != 1 || fixture.cursor.runtime.executeCalls != 0 {
		t.Fatalf("owner calls=%d runtime executes=%d", fixture.requestCount(), fixture.cursor.runtime.executeCalls)
	}
	request := fixture.requests[0]["params"].(map[string]any)
	if request["text"] != text || strings.HasPrefix(request["operationId"].(string), "op_") || !strings.HasPrefix(request["runId"].(string), "pairfob_owner_run_") {
		t.Fatalf("owner received altered or unsafe identity: %#v", request)
	}
	history, err := client.RPC("History", map[string]any{"pane_id": fixture.cursor.pane.PaneID, "limit": 10})
	if err != nil || !strings.Contains(string(history), "keep this literal") || !strings.Contains(string(history), "owner answer") {
		t.Fatalf("history=%s err=%v", history, err)
	}
	reads := fixture.cursor.runtime.snapshotCalls
	fixture.cursor.runtime.snapshot.Panes = nil
	replayed, err := client.RPC("PromptAgent", params)
	if err != nil || string(replayed) != string(first) || fixture.requestCount() != 1 || fixture.cursor.runtime.snapshotCalls != reads {
		t.Fatalf("replay did live work: first=%s replay=%s calls=%d snapshots=%d/%d err=%v", first, replayed, fixture.requestCount(), reads, fixture.cursor.runtime.snapshotCalls, err)
	}
	params["text"] = "conflicting prompt"
	if _, err := client.RPC("PromptAgent", params); err == nil || err.Error() != "conflict" || fixture.cursor.runtime.snapshotCalls != reads {
		t.Fatalf("conflict did live work: %v snapshots=%d/%d", err, reads, fixture.cursor.runtime.snapshotCalls)
	}
}

func TestCursorPromptKnownRefusalLeavesUnprojectedPublicInput(t *testing.T) {
	fixture, _, client := newCursorPromptRPCFixture(t)
	fixture.respond = func(response http.ResponseWriter, _ map[string]any) {
		fixture.writeEnvelope(response, map[string]any{"ok": false, "recorded": false, "error": map[string]any{
			"code": "agent_busy", "message": "private owner detail", "notSent": true, "outcomeUnknown": false,
		}})
	}
	if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cursorbusy0000001", "pane_id": fixture.cursor.pane.PaneID, "text": "literal"}); err == nil || err.Error() != "conflict" || strings.Contains(err.Error(), "private") {
		t.Fatalf("busy refusal was not sanitized: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.cursor.manifest.OwnerDir, "public-inputs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one durable orphan: entries=%d err=%v", len(entries), err)
	}
	history, err := client.RPC("History", map[string]any{"pane_id": fixture.cursor.pane.PaneID, "limit": 10})
	if err != nil || strings.Contains(string(history), "literal") {
		t.Fatalf("orphan was projected: %s err=%v", history, err)
	}
}

func TestCursorPromptNullRefusalFlagsAreUnknown(t *testing.T) {
	for _, field := range []string{"notSent", "outcomeUnknown"} {
		t.Run(field, func(t *testing.T) {
			fixture, _, client := newCursorPromptRPCFixture(t)
			fixture.respond = func(response http.ResponseWriter, _ map[string]any) {
				failure := map[string]any{"code": "agent_busy", "notSent": true, "outcomeUnknown": false}
				failure[field] = nil
				fixture.writeEnvelope(response, map[string]any{"ok": false, "recorded": false, "error": failure})
			}
			if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cursornullflag001", "pane_id": fixture.cursor.pane.PaneID, "text": "literal"}); err == nil || err.Error() != "unknown_outcome" {
				t.Fatalf("null %s result=%v", field, err)
			}
		})
	}
	for _, field := range []string{"ok", "recorded"} {
		t.Run(field, func(t *testing.T) {
			fixture, _, client := newCursorPromptRPCFixture(t)
			fixture.respond = func(response http.ResponseWriter, _ map[string]any) {
				envelope := map[string]any{"ok": false, "recorded": false, "error": map[string]any{"code": "agent_busy", "notSent": true, "outcomeUnknown": false}}
				envelope[field] = nil
				fixture.writeEnvelope(response, envelope)
			}
			if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cursornulltop0001", "pane_id": fixture.cursor.pane.PaneID, "text": "literal"}); err == nil || err.Error() != "unknown_outcome" {
				t.Fatalf("null %s result=%v", field, err)
			}
		})
	}
}

func TestCursorPromptTransportUncertaintyAndBuiltins(t *testing.T) {
	t.Run("lost acknowledgement is unknown", func(t *testing.T) {
		fixture, _, client := newCursorPromptRPCFixture(t)
		fixture.respond = func(response http.ResponseWriter, _ map[string]any) {
			hijacker := response.(http.Hijacker)
			connection, _, _ := hijacker.Hijack()
			_ = connection.Close()
		}
		if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cursorlostack0001", "pane_id": fixture.cursor.pane.PaneID, "text": "once"}); err == nil || err.Error() != "unknown_outcome" || fixture.requestCount() != 1 {
			t.Fatalf("lost ack result: %v calls=%d", err, fixture.requestCount())
		}
	})
	t.Run("redirect is not followed", func(t *testing.T) {
		fixture, _, client := newCursorPromptRPCFixture(t)
		var followed atomic.Bool
		target := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) })
		redirect := &http.Server{Handler: target}
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = redirect.Serve(listener) }()
		t.Cleanup(func() { _ = redirect.Close() })
		fixture.respond = func(response http.ResponseWriter, _ map[string]any) {
			response.Header().Set("Location", "http://"+listener.Addr().String()+"/stolen")
			response.WriteHeader(http.StatusTemporaryRedirect)
		}
		if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cursorredirect001", "pane_id": fixture.cursor.pane.PaneID, "text": "once"}); err == nil || err.Error() != "unknown_outcome" || followed.Load() {
			t.Fatalf("redirect result=%v followed=%v", err, followed.Load())
		}
	})
	t.Run("timeout after headers is unknown", func(t *testing.T) {
		fixture, engine, client := newCursorPromptRPCFixture(t)
		engine.cursorHTTPTimeout = 30 * time.Millisecond
		fixture.respond = func(_ http.ResponseWriter, _ map[string]any) { time.Sleep(80 * time.Millisecond) }
		if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cursortimeout0001", "pane_id": fixture.cursor.pane.PaneID, "text": "once"}); err == nil || err.Error() != "unknown_outcome" {
			t.Fatalf("timeout result=%v", err)
		}
	})
	for _, test := range []struct {
		name    string
		respond func(http.ResponseWriter)
	}{
		{name: "invalid response", respond: func(response http.ResponseWriter) { _, _ = response.Write([]byte("{")) }},
		{name: "oversize response", respond: func(response http.ResponseWriter) {
			_, _ = response.Write(make([]byte, cursorControlResponseMax+1))
		}},
	} {
		t.Run(test.name+" is unknown", func(t *testing.T) {
			fixture, _, client := newCursorPromptRPCFixture(t)
			fixture.respond = func(response http.ResponseWriter, _ map[string]any) { test.respond(response) }
			if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_cursorbadreply001", "pane_id": fixture.cursor.pane.PaneID, "text": "once"}); err == nil || err.Error() != "unknown_outcome" {
				t.Fatalf("bad response result=%v", err)
			}
		})
	}
	t.Run("raw builtin stays on runtime", func(t *testing.T) {
		fake := runtime.NewFake()
		fake.Snap.Panes[0].AgentSession = &runtime.AgentSessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "native"}
		_, client := runtimeRPCClient(t, fake)
		if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_builtinprompt0001", "pane_id": fake.Snap.Panes[0].PaneID, "text": "/quit"}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCursorPromptRestoredLedgerNeverReentersOwnerPath(t *testing.T) {
	for _, status := range []string{"pending", "completed"} {
		t.Run(status, func(t *testing.T) {
			fixture, engine, client := newCursorPromptRPCFixture(t)
			operationID := "op_restoredprompt001"
			command := runtime.PromptAgentCommand{Target: fixture.cursor.pane.PaneID, Text: "restored"}
			fingerprint, err := operationFingerprint(runtime.DefaultSession(), command)
			if err != nil {
				t.Fatal(err)
			}
			row := state.Operation{DeviceID: client.DeviceID, OperationID: operationID, Fingerprint: fingerprint, Status: status, CreatedAt: time.Now().Unix()}
			if status == "completed" {
				row.CompletedAt = time.Now().Unix()
				row.Receipt, _ = json.Marshal(runtime.Receipt{OperationID: operationID, Outcome: runtime.OutcomeApplied, Updated: []runtime.EntityRef{{Kind: runtime.EntityAgent, ID: command.Target}}})
			}
			if err := engine.restoreOperations([]state.Operation{row}); err != nil {
				t.Fatal(err)
			}
			result, rpcErr := client.RPC("PromptAgent", map[string]any{"operation_id": operationID, "pane_id": command.Target, "text": command.Text})
			if status == "pending" {
				if rpcErr == nil || rpcErr.Error() != "unknown_outcome" {
					t.Fatalf("pending result=%s err=%v", result, rpcErr)
				}
			} else if rpcErr != nil || !strings.Contains(string(result), `"outcome":"applied"`) {
				t.Fatalf("completed result=%s err=%v", result, rpcErr)
			}
			if fixture.requestCount() != 0 || fixture.cursor.runtime.totalCalls() != 0 {
				t.Fatalf("restored %s reentered live path: owner=%d runtime=%d", status, fixture.requestCount(), fixture.cursor.runtime.totalCalls())
			}
		})
	}
}

func TestCursorPromptIDsAreDeviceScopedAndStable(t *testing.T) {
	f := newCursorResolverFixture(t)
	registration, err := f.config.LoadRegistration(f.manifest.TerminalID)
	if err != nil || registration == nil {
		t.Fatal(err)
	}
	native, err := f.config.LoadSession(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	op1, run1 := cursorPromptIDs("dev_aaaaaaaa", "op_same000000000001", runtime.DefaultSession(), *registration, native)
	op2, run2 := cursorPromptIDs("dev_bbbbbbbb", "op_same000000000001", runtime.DefaultSession(), *registration, native)
	op1Again, run1Again := cursorPromptIDs("dev_aaaaaaaa", "op_same000000000001", runtime.DefaultSession(), *registration, native)
	if op1 == op2 || run1 == run2 || op1 != op1Again || run1 != run1Again || strings.HasPrefix(op1, "op_") || strings.HasPrefix(run1, "run_") {
		t.Fatalf("bad native IDs: %q %q %q %q", op1, run1, op2, run2)
	}
}

func TestCursorPromptRegisteredInvalidCannotFallBack(t *testing.T) {
	fixture, _, client := newCursorPromptRPCFixture(t)
	if err := os.Chmod(fixture.cursor.manifestPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_invalidowner00001", "pane_id": fixture.cursor.pane.PaneID, "text": "do not type"}); err == nil || err.Error() != "conflict" || fixture.cursor.runtime.executeCalls != 0 || fixture.requestCount() != 0 {
		t.Fatalf("invalid registration fell back: err=%v execute=%d owner=%d", err, fixture.cursor.runtime.executeCalls, fixture.requestCount())
	}
}

func TestCursorPromptFencesChangesBeforeAndAfterNetwork(t *testing.T) {
	t.Run("control changed after publication", func(t *testing.T) {
		fixture, _, client := newCursorPromptRPCFixture(t)
		fixture.cursor.runtime.afterSnapshot = func() {
			if fixture.cursor.runtime.snapshotCalls == 4 {
				fixture.changeControlRecord()
			}
		}
		if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_prefencechange001", "pane_id": fixture.cursor.pane.PaneID, "text": "published only"}); err == nil || err.Error() != "conflict" || fixture.requestCount() != 0 {
			t.Fatalf("pre-network fence result=%v calls=%d", err, fixture.requestCount())
		}
		entries, err := os.ReadDir(filepath.Join(fixture.cursor.manifest.OwnerDir, "public-inputs"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("publication timing not exercised: entries=%d err=%v", len(entries), err)
		}
	})
	for _, test := range []struct {
		name   string
		mutate func(*cursorOwnerHTTPFixture)
	}{
		{name: "control", mutate: func(f *cursorOwnerHTTPFixture) { f.changeControlRecord() }},
		{name: "claim", mutate: func(f *cursorOwnerHTTPFixture) {
			claim := f.cursor.claim
			claim.ClaimID = "replacement"
			writeDaemonJSON(f.t, f.cursor.claimPath, claim)
		}},
		{name: "native", mutate: func(f *cursorOwnerHTTPFixture) {
			writeDaemonJSON(f.t, f.cursor.sessionPath, f.cursor.sessionRecord("replacement-native", 2))
		}},
		{name: "registration", mutate: func(f *cursorOwnerHTTPFixture) {
			manifest := f.cursor.manifest
			manifest.CaseID = "replacement-case"
			writeDaemonJSON(f.t, f.cursor.manifestPath, manifest)
		}},
		{name: "owner identity", mutate: func(f *cursorOwnerHTTPFixture) {
			identity := f.cursor.identities[f.cursor.manifest.OwnerIdentity.PID]
			identity.StartTime = "replacement-start"
			f.cursor.identities[identity.PID] = identity
		}},
	} {
		t.Run(test.name+" changed after send", func(t *testing.T) {
			fixture, _, client := newCursorPromptRPCFixture(t)
			fixture.beforeReply = func() { test.mutate(fixture) }
			if _, err := client.RPC("PromptAgent", map[string]any{"operation_id": "op_postfencechange01", "pane_id": fixture.cursor.pane.PaneID, "text": "admitted once"}); err == nil || err.Error() != "unknown_outcome" || fixture.requestCount() != 1 {
				t.Fatalf("post-network fence result=%v calls=%d", err, fixture.requestCount())
			}
		})
	}
}

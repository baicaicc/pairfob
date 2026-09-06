package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pairfob/internal/journal"
	"pairfob/internal/runtime"
	"pairfob/internal/state"
)

func TestCursorResolverReturnsDigestBoundPrivateRef(t *testing.T) {
	f := newCursorResolverFixture(t)
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	ref, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane)
	if !ok || ref.Source != "allinone:cursor-acp" || ref.Agent != "cursor" || ref.Kind != "path" || ref.Value != f.manifestPath || len(ref.ManifestDigest) != 64 {
		t.Fatalf("unexpected Cursor ref: %#v, %v", ref, ok)
	}
	if f.runtime.snapshotCalls != 1 || f.runtime.processCalls != 2 || f.identityCalls != 4 {
		t.Fatalf("unexpected verification calls: snapshot=%d process=%d identity=%d", f.runtime.snapshotCalls, f.runtime.processCalls, f.identityCalls)
	}
	encoded, err := json.Marshal(struct {
		Pane runtime.Pane `json:"pane"`
		Ref  journal.Ref  `json:"ref"`
	}{Pane: runtime.Pane{AgentSession: &runtime.AgentSessionRef{Source: ref.Source, Value: ref.Value, ManifestDigest: ref.ManifestDigest}}, Ref: ref})
	if err != nil || string(encoded) != `{"pane":{"pane_id":"","workspace_id":"","tab_id":"","cwd":"","agent":"","agent_status":"","label":null,"history_available":false,"scroll":null},"ref":{}}` {
		t.Fatalf("private resolver values escaped JSON: %s, %v", encoded, err)
	}
}

func TestCursorResolverBuiltinAndUnregisteredCandidatesDoNoLiveProbing(t *testing.T) {
	f := newCursorResolverFixture(t)
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	builtin := f.pane
	builtin.AgentSession = &runtime.AgentSessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session_12345678"}
	ref, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), builtin)
	if !ok || ref.Source != "herdr:codex" || f.runtime.totalCalls() != 0 || f.identityCalls != 0 {
		t.Fatalf("builtin selection probed Cursor state: %#v", ref)
	}
	other := f.pane
	other.TerminalID = "unregistered-terminal"
	if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), other); ok || f.runtime.totalCalls() != 0 || f.identityCalls != 0 {
		t.Fatal("unregistered terminal triggered live probing")
	}
}

func TestCursorResolverRejectsMidResolutionPaneAndClaimChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*cursorResolverFixture)
	}{
		{name: "pane moved", mutate: func(f *cursorResolverFixture) {
			f.runtime.afterFirstProcess = func() { f.runtime.snapshot.Panes[0].WorkspaceID = "other-workspace" }
		}},
		{name: "extra foreground", mutate: func(f *cursorResolverFixture) {
			f.runtime.afterFirstProcess = func() {
				f.runtime.process.ForegroundProcesses = append(f.runtime.process.ForegroundProcesses, runtime.PaneForegroundProcess{PID: 303})
			}
		}},
		{name: "claim replaced", mutate: func(f *cursorResolverFixture) {
			f.runtime.afterFirstProcess = func() { claim := f.claim; claim.ClaimID = "replacement"; writeDaemonJSON(f.t, f.claimPath, claim) }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorResolverFixture(t)
			test.mutate(&f)
			engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
			engine.cursorIdentity = f.identityReader()
			if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok {
				t.Fatal("changed resolver evidence was accepted")
			}
		})
	}
}

func TestCursorResolverRetriesOnlyExactClaimedClientAmongTransientExtras(t *testing.T) {
	f := newCursorResolverFixture(t)
	group := 202
	sole := f.runtime.process
	sole.ForegroundProcessGroupID = &group
	withExtra := sole
	withExtra.ForegroundProcesses = append(append([]runtime.PaneForegroundProcess(nil), sole.ForegroundProcesses...), runtime.PaneForegroundProcess{PID: 303, Argv: []string{"/usr/sbin/sysctl", "-n", "kern.boottime"}, Cwd: &f.manifest.Cwd})
	f.runtime.processSequence = []runtime.PaneProcessInfoView{withExtra, sole, withExtra, sole}
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); !ok {
		t.Fatal("transient foreground helper prevented resolution")
	}
	if f.runtime.processCalls != 4 || f.runtime.executeCalls != 0 {
		t.Fatalf("unexpected bounded read-only calls: process=%d execute=%d", f.runtime.processCalls, f.runtime.executeCalls)
	}
}

func TestCursorResolverBoundsPersistentForegroundExtras(t *testing.T) {
	f := newCursorResolverFixture(t)
	sole := f.runtime.process
	withExtra := sole
	withExtra.ForegroundProcesses = append(append([]runtime.PaneForegroundProcess(nil), sole.ForegroundProcesses...), runtime.PaneForegroundProcess{PID: 303, Argv: []string{"helper"}, Cwd: &f.manifest.Cwd})
	f.runtime.processSequence = []runtime.PaneProcessInfoView{sole, withExtra}
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok {
		t.Fatal("persistent foreground extras were accepted")
	}
	if f.runtime.processCalls != cursorProcessSamples+1 || f.runtime.executeCalls != 0 {
		t.Fatalf("persistent extras were not bounded/read-only: process=%d execute=%d", f.runtime.processCalls, f.runtime.executeCalls)
	}
}

func TestCursorResolverFailsImmediatelyWhenClaimedClientIsNotExact(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*cursorResolverFixture)
	}{
		{name: "missing", mutate: func(f *cursorResolverFixture) { f.runtime.process.ForegroundProcesses[0].PID = 303 }},
		{name: "wrong argv", mutate: func(f *cursorResolverFixture) { f.runtime.process.ForegroundProcesses[0].Argv[1] = "/wrong-entry" }},
		{name: "duplicate candidate", mutate: func(f *cursorResolverFixture) {
			duplicate := f.runtime.process.ForegroundProcesses[0]
			duplicate.PID = 303
			f.runtime.process.ForegroundProcesses = append(f.runtime.process.ForegroundProcesses, duplicate)
		}},
		{name: "observe error", mutate: func(f *cursorResolverFixture) { f.runtime.processErr = errors.New("observe failed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorResolverFixture(t)
			test.mutate(&f)
			engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
			engine.cursorIdentity = f.identityReader()
			if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok || f.runtime.processCalls != 1 || f.runtime.executeCalls != 0 {
				t.Fatalf("invalid claimed client did not fail immediately: process=%d execute=%d", f.runtime.processCalls, f.runtime.executeCalls)
			}
		})
	}
}

func TestCursorResolverCancelsDuringForegroundResampleDelay(t *testing.T) {
	f := newCursorResolverFixture(t)
	f.runtime.process.ForegroundProcesses = append(f.runtime.process.ForegroundProcesses, runtime.PaneForegroundProcess{PID: 303, Argv: []string{"helper"}, Cwd: &f.manifest.Cwd})
	ctx, cancel := context.WithCancel(context.Background())
	var timer *time.Timer
	f.runtime.afterFirstProcess = func() { timer = time.AfterFunc(5*time.Millisecond, cancel) }
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	if _, ok := engine.selectJournalRef(ctx, runtime.DefaultSession(), f.pane); ok || f.runtime.processCalls != 1 || f.runtime.executeCalls != 0 {
		t.Fatalf("canceled retry continued: process=%d execute=%d", f.runtime.processCalls, f.runtime.executeCalls)
	}
}

func TestCursorResolverRejectsChangedProcessSampleIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*runtime.PaneProcessInfoView)
	}{
		{name: "shell", mutate: func(view *runtime.PaneProcessInfoView) { shell := 98; view.ShellPID = &shell }},
		{name: "group", mutate: func(view *runtime.PaneProcessInfoView) { group := 999; view.ForegroundProcessGroupID = &group }},
		{name: "pane", mutate: func(view *runtime.PaneProcessInfoView) { view.PaneID = "other:pane" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorResolverFixture(t)
			group := 202
			first := f.runtime.process
			first.ForegroundProcessGroupID = &group
			first.ForegroundProcesses = append(first.ForegroundProcesses, runtime.PaneForegroundProcess{PID: 303, Argv: []string{"helper"}, Cwd: &f.manifest.Cwd})
			second := f.runtime.process
			second.ForegroundProcessGroupID = &group
			test.mutate(&second)
			f.runtime.processSequence = []runtime.PaneProcessInfoView{first, second}
			engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
			engine.cursorIdentity = f.identityReader()
			if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok || f.runtime.processCalls != 2 || f.runtime.executeCalls != 0 {
				t.Fatalf("changed process sample was accepted: process=%d execute=%d", f.runtime.processCalls, f.runtime.executeCalls)
			}
		})
	}
}

func TestCursorResolverRejectsClosingBaselineChangeBeforeRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*runtime.PaneProcessInfoView)
	}{
		{name: "shell", mutate: func(view *runtime.PaneProcessInfoView) { shell := 98; view.ShellPID = &shell }},
		{name: "group", mutate: func(view *runtime.PaneProcessInfoView) { group := 999; view.ForegroundProcessGroupID = &group }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorResolverFixture(t)
			group := 202
			initial := f.runtime.process
			initial.ForegroundProcessGroupID = &group
			closing := initial
			closing.ForegroundProcesses = append(append([]runtime.PaneForegroundProcess(nil), initial.ForegroundProcesses...), runtime.PaneForegroundProcess{PID: 303, Argv: []string{"helper"}, Cwd: &f.manifest.Cwd})
			test.mutate(&closing)
			f.runtime.processSequence = []runtime.PaneProcessInfoView{initial, closing}
			engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
			engine.cursorIdentity = f.identityReader()
			if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok || f.runtime.processCalls != 2 || f.runtime.executeCalls != 0 {
				t.Fatalf("closing baseline change retried: process=%d execute=%d", f.runtime.processCalls, f.runtime.executeCalls)
			}
		})
	}
}

func TestCursorResolverRejectsIdentityChangesAcrossAwaitBoundaries(t *testing.T) {
	for _, test := range []struct {
		name      string
		afterCall int
		pid       int
	}{
		{name: "owner", afterCall: 2, pid: 101},
		{name: "client", afterCall: 3, pid: 202},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorResolverFixture(t)
			f.afterIdentity = func(call int) {
				if call == test.afterCall {
					identity := f.identities[test.pid]
					identity.StartTime += "-reused"
					f.identities[test.pid] = identity
				}
			}
			engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
			engine.cursorIdentity = f.identityReader()
			if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok {
				t.Fatal("identity changed across await was accepted")
			}
		})
	}
}

func TestCursorResolverRejectsClosingFileAndRootChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*cursorResolverFixture)
	}{
		{name: "binding", mutate: func(f *cursorResolverFixture) {
			binding := journal.CursorBinding{SchemaVersion: 1, OwnerDir: f.manifest.OwnerDir, OwnerID: f.manifest.OwnerID, CaseID: "other-case", Cwd: f.manifest.Cwd, Identity: f.manifest.OwnerIdentity, TerminalID: f.manifest.TerminalID}
			writeDaemonJSON(f.t, f.manifest.BindingPath, binding)
		}},
		{name: "manifest", mutate: func(f *cursorResolverFixture) {
			manifest := f.manifest
			manifest.CaseID = "other-case"
			writeDaemonJSON(f.t, f.manifestPath, manifest)
		}},
		{name: "registry root", mutate: func(f *cursorResolverFixture) {
			if err := os.Chmod(f.config.RegistryRoot, 0o755); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "owner root", mutate: func(f *cursorResolverFixture) {
			if err := os.Chmod(f.config.OwnerRoots[0], 0o755); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "binding root", mutate: func(f *cursorResolverFixture) {
			if err := os.Chmod(f.config.BindingRoots[0], 0o755); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorResolverFixture(t)
			f.runtime.afterSnapshot = func() { test.mutate(&f) }
			engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
			engine.cursorIdentity = f.identityReader()
			if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok {
				t.Fatal("closing evidence change was accepted")
			}
		})
	}
}

func TestCursorResolverAllowsSessionRevisionAndContentGrowth(t *testing.T) {
	f := newCursorResolverFixture(t)
	f.runtime.afterSnapshot = func() {
		record := f.sessionRecord(f.manifest.NativeSessionID, 2)
		session := record["session"].(map[string]any)
		session["operations"] = []any{map[string]any{"publicText": "new output"}}
		session["output"] = map[string]any{"chars": 10, "truncated": false}
		writeDaemonJSON(t, f.sessionPath, record)
	}
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); !ok {
		t.Fatal("legitimate session growth was rejected")
	}
}

func TestCursorResolverRejectsStaticIdentityAndSessionMismatches(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*cursorResolverFixture)
		session runtime.SessionRef
	}{
		{name: "owner identity", mutate: func(f *cursorResolverFixture) {
			owner := f.identities[101]
			owner.StartTime = "reused"
			f.identities[101] = owner
		}},
		{name: "client claim", mutate: func(f *cursorResolverFixture) {
			claim := f.claim
			claim.Identity.StartTime = "stale"
			writeDaemonJSON(f.t, f.claimPath, claim)
		}},
		{name: "shell returned", mutate: func(f *cursorResolverFixture) { pid := 202; f.runtime.process.ShellPID = &pid }},
		{name: "wrong argv", mutate: func(f *cursorResolverFixture) { f.runtime.process.ForegroundProcesses[0].Argv[1] = "/wrong-entry" }},
		{name: "wrong runtime session", session: runtime.NamedSession("other")},
		{name: "native session", mutate: func(f *cursorResolverFixture) {
			writeDaemonJSON(f.t, f.sessionPath, f.sessionRecord("other-native", 1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorResolverFixture(t)
			if test.mutate != nil {
				test.mutate(&f)
			}
			engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
			engine.cursorIdentity = f.identityReader()
			if _, ok := engine.selectJournalRef(context.Background(), test.session, f.pane); ok {
				t.Fatal("mismatched static evidence was accepted")
			}
		})
	}
}

func TestCursorResolverAllowsReconnectBetweenCompleteResolutions(t *testing.T) {
	f := newCursorResolverFixture(t)
	engine := NewEngineWithCursorConfig(nil, nil, f.runtime, f.config)
	engine.cursorIdentity = f.identityReader()
	first, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane)
	if !ok {
		t.Fatal("initial resolution failed")
	}
	client := f.claim.Identity
	client.PID = 303
	client.StartTime = "reconnected"
	f.claim.ClaimID = "claim-two"
	f.claim.Identity = client
	writeDaemonJSON(t, f.claimPath, f.claim)
	f.identities[client.PID] = client
	f.runtime.process.ForegroundProcesses[0].PID = client.PID
	second, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane)
	if !ok || first != second {
		t.Fatalf("completed reconnect did not retain stable ref: %#v %#v", first, second)
	}
}

func TestPersistentEngineRejectsCursorConfigBeforeStateWrites(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "invalid.json")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(journal.CursorConfigEnv, configPath)
	store, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistentEngine(nil, nil, runtime.NewFake(), store, nil); err == nil {
		t.Fatal("invalid explicit Cursor config was accepted")
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("state was written before config rejection: %v, %v", entries, err)
	}
}

func TestEphemeralEngineNeverReadsCursorEnvironment(t *testing.T) {
	t.Setenv(journal.CursorConfigEnv, "/definitely/missing/private-config.json")
	engine := NewEngine(nil, nil, runtime.NewFake())
	if engine.CursorConfig != nil || engine.Journal.Cursor != nil {
		t.Fatal("ephemeral engine loaded Cursor environment")
	}
}

func TestDisabledCursorResolverMakesNoRuntimeOrIdentityCalls(t *testing.T) {
	f := newCursorResolverFixture(t)
	engine := NewEngine(nil, nil, f.runtime)
	engine.cursorIdentity = f.identityReader()
	if _, ok := engine.selectJournalRef(context.Background(), runtime.DefaultSession(), f.pane); ok || f.runtime.totalCalls() != 0 || f.identityCalls != 0 {
		t.Fatal("disabled resolver touched runtime or process identity")
	}
}

func TestCursorSnapshotAndHistoryRPCSharePrivateResolverResult(t *testing.T) {
	f := newCursorResolverFixture(t)
	operationID, runID, wireText := "operation-one", "run-one", "<private-wrapper>wire</private-wrapper>"
	operation := map[string]any{"operationId": operationID, "runId": runID, "caseId": f.manifest.CaseID, "ownerId": f.manifest.OwnerID, "cwd": f.manifest.Cwd, "nativeSessionId": f.manifest.NativeSessionID, "text": wireText, "publicText": "verified public answer", "status": "completed", "toolCalls": []any{"private tool"}}
	writeDaemonJSON(t, f.sessionPath, map[string]any{"schemaVersion": 1, "revision": 2, "savedAt": "2026-09-06T12:00:01Z", "session": map[string]any{"schemaVersion": 1, "ownerId": f.manifest.OwnerID, "cwd": f.manifest.Cwd, "nativeSessionId": f.manifest.NativeSessionID, "status": "ready", "operations": []any{operation}, "publicHistory": []any{"unverified secret"}}})
	publicInputs := daemonMkdir(t, filepath.Join(f.manifest.OwnerDir, "public-inputs"), 0o700)
	operationHash, wireHash := sha256.Sum256([]byte(operationID)), sha256.Sum256([]byte(wireText))
	writeDaemonJSON(t, filepath.Join(publicInputs, hex.EncodeToString(operationHash[:])+".json"), map[string]any{"schemaVersion": 1, "origin": "service", "ownerId": f.manifest.OwnerID, "caseId": f.manifest.CaseID, "cwd": f.manifest.Cwd, "nativeSessionId": f.manifest.NativeSessionID, "operationId": operationID, "runId": runID, "wireTextSha256": hex.EncodeToString(wireHash[:]), "userText": "verified public question"})
	engine, client := runtimeRPCClient(t, f.runtime)
	engine.CursorConfig = f.config
	engine.Journal.Cursor = f.config
	engine.cursorIdentity = f.identityReader()
	raw, err := client.RPC("Snapshot", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	if !strings.Contains(encoded, `"history_available":true`) || !strings.Contains(encoded, `"agent":"Cursor ACP"`) || strings.Contains(encoded, f.manifestPath) || strings.Contains(encoded, f.manifest.TerminalID) {
		t.Fatalf("Cursor snapshot leaked private resolver state: %s", raw)
	}
	if raw, err := client.RPC("History", map[string]any{"pane_id": f.pane.PaneID, "limit": 20}); err != nil || !strings.Contains(string(raw), "verified public question") || !strings.Contains(string(raw), "verified public answer") || strings.Contains(string(raw), "private-wrapper") {
		t.Fatalf("Cursor history was not readable: %s, %v", raw, err)
	}
	if raw, err := client.RPC("AgentTrace", map[string]any{"pane_id": f.pane.PaneID, "limit": 20}); err != nil || !strings.Contains(string(raw), "verified public question") || !strings.Contains(string(raw), "verified public answer") || strings.Contains(string(raw), "private tool") {
		t.Fatalf("Cursor trace was not readable: %s, %v", raw, err)
	}
	if raw, err := client.RPC("AgentTraceSummary", map[string]any{"pane_id": f.pane.PaneID, "limit": 20}); err != nil || !strings.Contains(string(raw), "verified public question") || !strings.Contains(string(raw), "verified public answer") {
		t.Fatalf("Cursor trace summary was not readable: %s, %v", raw, err)
	}
	if _, err := client.RPC("AgentTraceDetail", map[string]any{"pane_id": f.pane.PaneID, "detail_ref": "unsupported"}); err == nil || err.Error() != "transcript_unavailable" {
		t.Fatalf("Cursor trace detail was not refused: %v", err)
	}
}

type cursorResolverRuntime struct {
	snapshot          runtime.Snapshot
	process           runtime.PaneProcessInfoView
	processSequence   []runtime.PaneProcessInfoView
	snapshotCalls     int
	processCalls      int
	executeCalls      int
	processErr        error
	afterFirstProcess func()
	afterSnapshot     func()
}

func (r *cursorResolverRuntime) Describe(context.Context, runtime.SessionRef) (runtime.Descriptor, error) {
	return runtime.Descriptor{}, nil
}
func (r *cursorResolverRuntime) Execute(context.Context, runtime.SessionRef, string, runtime.Command) (runtime.Receipt, error) {
	r.executeCalls++
	return runtime.Receipt{}, errors.New("unexpected execute")
}
func (r *cursorResolverRuntime) Observe(_ context.Context, _ runtime.SessionRef, query runtime.Query) (runtime.View, error) {
	switch query.(type) {
	case runtime.SnapshotQuery:
		r.snapshotCalls++
		snapshot := r.snapshot
		snapshot.Panes = append([]runtime.Pane(nil), r.snapshot.Panes...)
		if r.afterSnapshot != nil {
			r.afterSnapshot()
		}
		return runtime.SnapshotView{Snapshot: snapshot}, nil
	case runtime.PaneProcessInfoQuery:
		r.processCalls++
		if r.processErr != nil {
			return nil, r.processErr
		}
		view := r.process
		if len(r.processSequence) > 0 {
			index := min(r.processCalls-1, len(r.processSequence)-1)
			view = r.processSequence[index]
		}
		view.ForegroundProcesses = append([]runtime.PaneForegroundProcess(nil), view.ForegroundProcesses...)
		if r.processCalls == 1 && r.afterFirstProcess != nil {
			r.afterFirstProcess()
		}
		return view, nil
	default:
		return nil, errors.New("unexpected observe")
	}
}
func (r *cursorResolverRuntime) totalCalls() int { return r.snapshotCalls + r.processCalls }

type cursorResolverFixture struct {
	t             *testing.T
	config        *journal.CursorConfig
	runtime       *cursorResolverRuntime
	pane          runtime.Pane
	manifestPath  string
	claimPath     string
	sessionPath   string
	claim         journal.CursorClaim
	manifest      journal.CursorManifest
	identities    map[int]journal.CursorProcessIdentity
	identityCalls int
	afterIdentity func(int)
}

func (f *cursorResolverFixture) sessionRecord(native string, revision int) map[string]any {
	return map[string]any{"schemaVersion": 1, "revision": revision, "savedAt": "2026-09-06T12:00:00Z", "session": map[string]any{"schemaVersion": 1, "ownerId": f.manifest.OwnerID, "cwd": f.manifest.Cwd, "nativeSessionId": native, "status": "ready"}}
}

func (f *cursorResolverFixture) identityReader() cursorIdentityReader {
	return func(_ context.Context, pid int) (*journal.CursorProcessIdentity, error) {
		f.identityCalls++
		identity, ok := f.identities[pid]
		if !ok {
			return nil, nil
		}
		copy := identity
		if f.afterIdentity != nil {
			f.afterIdentity(f.identityCalls)
		}
		return &copy, nil
	}
}

func newCursorResolverFixture(t *testing.T) cursorResolverFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := daemonMkdir(t, filepath.Join(root, "registry"), 0o700)
	ownerRoot := daemonMkdir(t, filepath.Join(root, "owners"), 0o700)
	ownerDir := daemonMkdir(t, filepath.Join(ownerRoot, "owner-one"), 0o700)
	bindingRoot := daemonMkdir(t, filepath.Join(root, "bindings"), 0o700)
	cwd := daemonMkdir(t, filepath.Join(root, "cwd"), 0o755)
	terminals := daemonMkdir(t, filepath.Join(ownerDir, "terminals"), 0o700)
	terminalID := "terminal-one"
	terminalDir := daemonMkdir(t, filepath.Join(terminals, terminalID), 0o700)
	nodePath, entryPath := filepath.Join(root, "node"), filepath.Join(root, "entry.mjs")
	_ = os.WriteFile(nodePath, []byte("node"), 0o700)
	_ = os.WriteFile(entryPath, []byte("entry"), 0o600)
	bindingPath := filepath.Join(bindingRoot, "binding.json")
	argv := []string{nodePath, entryPath, bindingPath}
	ownerIdentity := journal.CursorProcessIdentity{PID: 101, StartTime: "owner-start", BootID: "boot", Executable: nodePath, CommandLine: "owner command"}
	clientIdentity := journal.CursorProcessIdentity{PID: 202, StartTime: "client-start", BootID: "boot", Executable: nodePath, CommandLine: argv[0] + " " + argv[1] + " " + argv[2]}
	manifest := journal.CursorManifest{SchemaVersion: 1, Source: "allinone:cursor-acp", Agent: "Cursor ACP", WorkspaceID: "workspace-one", PaneID: "workspace-one:p1", TerminalID: terminalID, OwnerDir: ownerDir, OwnerID: "owner-one", CaseID: "case-one", Cwd: cwd, NativeSessionID: "native-one", OwnerIdentity: ownerIdentity, BindingPath: bindingPath, NodePath: nodePath, EntryPath: entryPath}
	claim := journal.CursorClaim{SchemaVersion: 1, TerminalID: terminalID, ClaimID: "claim-one", Identity: clientIdentity}
	claimPath := filepath.Join(terminalDir, "claim.json")
	writeDaemonJSON(t, bindingPath, journal.CursorBinding{SchemaVersion: 1, OwnerDir: ownerDir, OwnerID: "owner-one", CaseID: "case-one", Cwd: cwd, Identity: ownerIdentity, TerminalID: terminalID})
	writeDaemonJSON(t, claimPath, claim)
	sessionPath := filepath.Join(ownerDir, "session.json")
	writeDaemonJSON(t, sessionPath, map[string]any{"schemaVersion": 1, "revision": 1, "savedAt": "2026-09-06T12:00:00Z", "session": map[string]any{"schemaVersion": 1, "ownerId": "owner-one", "cwd": cwd, "nativeSessionId": "native-one", "status": "ready", "operations": []any{}}})
	terminalHash := sha256.Sum256([]byte(terminalID))
	manifestPath := filepath.Join(registry, hex.EncodeToString(terminalHash[:])+".json")
	writeDaemonJSON(t, manifestPath, manifest)
	configPath := filepath.Join(root, "config.json")
	writeDaemonJSON(t, configPath, journal.CursorConfig{SchemaVersion: 1, RegistryRoot: registry, OwnerRoots: []string{ownerRoot}, BindingRoots: []string{bindingRoot}, NodePath: nodePath, EntryPath: entryPath})
	config, err := journal.LoadCursorConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	pane := runtime.Pane{PaneID: manifest.PaneID, WorkspaceID: manifest.WorkspaceID, TabID: "workspace-one:t1", Cwd: cwd, TerminalID: terminalID}
	shellPID := 99
	rt := &cursorResolverRuntime{snapshot: runtime.Snapshot{Panes: []runtime.Pane{pane}}, process: runtime.PaneProcessInfoView{PaneID: pane.PaneID, ShellPID: &shellPID, ForegroundProcesses: []runtime.PaneForegroundProcess{{PID: clientIdentity.PID, Argv: argv, Cwd: &cwd}}}}
	return cursorResolverFixture{t: t, config: config, runtime: rt, pane: pane, manifestPath: manifestPath, claimPath: claimPath, sessionPath: sessionPath, claim: claim, manifest: manifest, identities: map[int]journal.CursorProcessIdentity{ownerIdentity.PID: ownerIdentity, clientIdentity.PID: clientIdentity}}
}

func daemonMkdir(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, mode)
	return path
}

func writeDaemonJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o600)
}

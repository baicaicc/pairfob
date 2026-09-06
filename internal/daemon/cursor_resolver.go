package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"pairfob/internal/journal"
	"pairfob/internal/runtime"
)

type cursorIdentityReader func(context.Context, int) (*journal.CursorProcessIdentity, error)

const (
	cursorProcessSamples = 5
	cursorProcessDelay   = 40 * time.Millisecond
)

type cursorClientSample struct {
	process  runtime.PaneForegroundProcess
	shellPID int
	groupID  *int
}

func (e *Engine) selectJournalRef(ctx context.Context, session runtime.SessionRef, pane runtime.Pane) (journal.Ref, bool) {
	if pane.AgentSession != nil {
		return journalRef(pane.AgentSession), true
	}
	if pane.TranscriptRefChecked {
		return journal.Ref{}, false
	}
	ref, err := e.resolveCursorRef(ctx, session, pane)
	return ref, err == nil && ref.Source != ""
}

func (e *Engine) resolveCursorRef(ctx context.Context, session runtime.SessionRef, pane runtime.Pane) (journal.Ref, error) {
	config := e.CursorConfig
	if config == nil || pane.TerminalID == "" {
		return journal.Ref{}, errors.New("Cursor resolver disabled")
	}
	if session.Name != config.RuntimeSession {
		return journal.Ref{}, errors.New("Cursor runtime session does not match configuration")
	}
	registration, err := config.LoadRegistration(pane.TerminalID)
	if err != nil || registration == nil {
		if err == nil {
			err = errors.New("Cursor terminal is not registered")
		}
		return journal.Ref{}, err
	}
	manifest := registration.Manifest
	if manifest.WorkspaceID != pane.WorkspaceID || manifest.PaneID != pane.PaneID || manifest.TerminalID != pane.TerminalID || manifest.Cwd != pane.Cwd {
		return journal.Ref{}, errors.New("Cursor registration does not match pane")
	}
	readIdentity := e.cursorIdentity
	if readIdentity == nil {
		readIdentity = nativeCursorProcessIdentity
	}
	ownerIdentity, err := readIdentity(ctx, manifest.OwnerIdentity.PID)
	if err != nil || ownerIdentity == nil || !journal.SameCursorIdentity(*ownerIdentity, manifest.OwnerIdentity) {
		return journal.Ref{}, errors.New("Cursor owner process identity does not match")
	}
	bindingBefore, err := config.LoadBinding(manifest)
	if err != nil {
		return journal.Ref{}, err
	}
	sessionBefore, err := config.LoadSession(manifest)
	if err != nil {
		return journal.Ref{}, err
	}
	claimBefore, err := config.LoadClaim(manifest)
	if err != nil {
		return journal.Ref{}, err
	}
	processBefore, err := e.cursorClient(ctx, session, manifest, claimBefore.Identity.PID, nil)
	if err != nil {
		return journal.Ref{}, err
	}
	clientIdentity, err := readIdentity(ctx, processBefore.process.PID)
	expectedCommandLine := manifest.NodePath + " " + manifest.EntryPath + " " + manifest.BindingPath
	if err != nil || clientIdentity == nil || !journal.SameCursorIdentity(*clientIdentity, claimBefore.Identity) || clientIdentity.Executable != manifest.NodePath || clientIdentity.CommandLine != expectedCommandLine {
		return journal.Ref{}, errors.New("Cursor client process identity does not match claim")
	}

	claimAfter, err := config.LoadClaim(manifest)
	if err != nil || !reflect.DeepEqual(claimBefore, claimAfter) {
		return journal.Ref{}, errors.New("Cursor claim changed during resolution")
	}
	snapshotAfter, err := e.rawCursorSnapshot(ctx, session)
	if err != nil {
		return journal.Ref{}, err
	}
	paneAfter, err := exactCursorPane(snapshotAfter, pane.PaneID)
	if err != nil || !sameCursorPane(pane, paneAfter) {
		return journal.Ref{}, errors.New("Cursor pane changed during resolution")
	}
	processAfter, err := e.cursorClient(ctx, session, manifest, claimAfter.Identity.PID, &processBefore)
	if err != nil || !sameCursorProcess(processBefore, processAfter) {
		return journal.Ref{}, errors.New("Cursor foreground client changed during resolution")
	}
	ownerAfter, err := readIdentity(ctx, manifest.OwnerIdentity.PID)
	if err != nil || ownerAfter == nil || !journal.SameCursorIdentity(*ownerAfter, manifest.OwnerIdentity) || !journal.SameCursorIdentity(*ownerAfter, *ownerIdentity) {
		return journal.Ref{}, errors.New("Cursor owner identity changed during resolution")
	}
	clientAfter, err := readIdentity(ctx, processAfter.process.PID)
	if err != nil || clientAfter == nil || !journal.SameCursorIdentity(*clientAfter, claimAfter.Identity) || !journal.SameCursorIdentity(*clientAfter, *clientIdentity) || clientAfter.Executable != manifest.NodePath || clientAfter.CommandLine != expectedCommandLine {
		return journal.Ref{}, errors.New("Cursor client identity changed during resolution")
	}
	bindingAfter, err := config.LoadBinding(manifest)
	if err != nil || !reflect.DeepEqual(bindingBefore, bindingAfter) {
		return journal.Ref{}, errors.New("Cursor binding changed during resolution")
	}
	sessionAfter, err := config.LoadSession(manifest)
	if err != nil || sessionAfter.Revision < sessionBefore.Revision || sessionAfter.OwnerID != sessionBefore.OwnerID || sessionAfter.Cwd != sessionBefore.Cwd || sessionAfter.NativeSessionID != sessionBefore.NativeSessionID {
		return journal.Ref{}, errors.New("Cursor session identity changed during resolution")
	}
	registrationAfter, err := config.LoadRegistration(pane.TerminalID)
	if err != nil || registrationAfter == nil || registrationAfter.Path != registration.Path || registrationAfter.Digest != registration.Digest {
		return journal.Ref{}, errors.New("Cursor registration changed during resolution")
	}
	return journal.Ref{Source: "allinone:cursor-acp", Agent: "cursor", Kind: "path", Value: registration.Path, ManifestDigest: registration.Digest}, nil
}

func (e *Engine) cursorClient(ctx context.Context, session runtime.SessionRef, manifest journal.CursorManifest, expectedPID int, baseline *cursorClientSample) (cursorClientSample, error) {
	if e.RT == nil {
		return cursorClientSample{}, errors.New("Cursor runtime is unavailable")
	}
	expectedArgv := []string{manifest.NodePath, manifest.EntryPath, manifest.BindingPath}
	for attempt := 0; attempt < cursorProcessSamples; attempt++ {
		if err := ctx.Err(); err != nil {
			return cursorClientSample{}, err
		}
		view, err := e.RT.Observe(ctx, session, runtime.PaneProcessInfoQuery{PaneID: manifest.PaneID})
		if err != nil {
			return cursorClientSample{}, err
		}
		if err := ctx.Err(); err != nil {
			return cursorClientSample{}, err
		}
		info, ok := view.(runtime.PaneProcessInfoView)
		if !ok || info.PaneID != manifest.PaneID || info.ShellPID == nil {
			return cursorClientSample{}, errors.New("Cursor foreground process identity is incomplete")
		}
		matching := 0
		var expected *runtime.PaneForegroundProcess
		for i := range info.ForegroundProcesses {
			process := &info.ForegroundProcesses[i]
			exact := process.PID != *info.ShellPID && process.Cwd != nil && *process.Cwd == manifest.Cwd && reflect.DeepEqual(process.Argv, expectedArgv)
			if exact {
				matching++
			}
			if process.PID == expectedPID {
				if expected != nil || !exact {
					return cursorClientSample{}, errors.New("Cursor claimed foreground process does not match registration")
				}
				expected = process
			}
		}
		if expected == nil || matching != 1 {
			return cursorClientSample{}, errors.New("Cursor claimed foreground process is missing or duplicated")
		}
		sample := cursorClientSample{process: *expected, shellPID: *info.ShellPID, groupID: copyCursorPID(info.ForegroundProcessGroupID)}
		if baseline == nil {
			baseline = &sample
		} else if sample.shellPID != baseline.shellPID || !reflect.DeepEqual(sample.groupID, baseline.groupID) || !sameCursorForegroundProcess(sample.process, baseline.process) {
			return cursorClientSample{}, errors.New("Cursor foreground process identity changed between samples")
		}
		if len(info.ForegroundProcesses) == 1 {
			return sample, nil
		}
		if attempt == cursorProcessSamples-1 {
			break
		}
		if err := waitCursorProcessSample(ctx); err != nil {
			return cursorClientSample{}, err
		}
	}
	return cursorClientSample{}, errors.New("Cursor foreground process remained non-exclusive")
}

func waitCursorProcessSample(ctx context.Context) error {
	timer := time.NewTimer(cursorProcessDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func copyCursorPID(pid *int) *int {
	if pid == nil {
		return nil
	}
	copy := *pid
	return &copy
}

func (e *Engine) rawCursorSnapshot(ctx context.Context, session runtime.SessionRef) (runtime.Snapshot, error) {
	view, err := e.RT.Observe(ctx, session, runtime.SnapshotQuery{})
	if err != nil {
		return runtime.Snapshot{}, err
	}
	snapshot, ok := view.(runtime.SnapshotView)
	if !ok {
		return runtime.Snapshot{}, fmt.Errorf("Cursor runtime returned an invalid snapshot")
	}
	return snapshot.Snapshot, nil
}

func exactCursorPane(snapshot runtime.Snapshot, paneID string) (runtime.Pane, error) {
	var found *runtime.Pane
	for i := range snapshot.Panes {
		if snapshot.Panes[i].PaneID != paneID {
			continue
		}
		if found != nil {
			return runtime.Pane{}, errors.New("duplicate Cursor pane")
		}
		copy := snapshot.Panes[i]
		found = &copy
	}
	if found == nil {
		return runtime.Pane{}, errors.New("Cursor pane disappeared")
	}
	return *found, nil
}

func sameCursorPane(before, after runtime.Pane) bool {
	return before.PaneID == after.PaneID && before.WorkspaceID == after.WorkspaceID && before.TabID == after.TabID && before.TerminalID == after.TerminalID && before.Cwd == after.Cwd && reflect.DeepEqual(before.AgentSession, after.AgentSession)
}

func sameCursorProcess(before, after cursorClientSample) bool {
	return before.shellPID == after.shellPID && reflect.DeepEqual(before.groupID, after.groupID) && sameCursorForegroundProcess(before.process, after.process)
}

func sameCursorForegroundProcess(before, after runtime.PaneForegroundProcess) bool {
	return before.PID == after.PID && reflect.DeepEqual(before.Argv, after.Argv) && reflect.DeepEqual(before.Cwd, after.Cwd)
}

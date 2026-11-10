package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// eachSession runs fn for every session concurrently and waits for all of them.
func eachSession(sessions []SessionRef, fn func(int, SessionRef)) {
	var wg sync.WaitGroup
	for i, session := range sessions {
		wg.Add(1)
		go func(i int, session SessionRef) {
			defer wg.Done()
			fn(i, session)
		}(i, session)
	}
	wg.Wait()
}

// snapshot reads every running session and concatenates them, default first.
// A named session that fails to answer is left out of this snapshot; when no
// session answers, the default session's error stands, so availability still
// follows the default socket exactly as it does without merging.
func (m *Merged) snapshot(ctx context.Context) (Snapshot, error) {
	sessions := m.running(ctx)
	parts := make([]Snapshot, len(sessions))
	errs := make([]error, len(sessions))
	eachSession(sessions, func(i int, target SessionRef) {
		view, err := m.herdr.Observe(ctx, target, SnapshotQuery{})
		if err != nil {
			errs[i] = err
			return
		}
		snapshot, ok := view.(SnapshotView)
		if !ok {
			errs[i] = errors.New("runtime returned an invalid snapshot view")
			return
		}
		if target.Name == "" && defaultIDsAmbiguous(snapshot.Snapshot) {
			// A default-session id containing the separator would route to a
			// named session; leave the session out rather than misroute.
			errs[i] = &Fault{Code: CodeInternal, Operation: "session.snapshot", Outcome: OutcomeNotApplied, Retry: RetryReadSafe, SafeMessage: "default-session id contains the session separator"}
			return
		}
		parts[i] = qualifySnapshot(target, snapshot.Snapshot)
	})

	var merged Snapshot
	found := false
	for i := range sessions {
		if errs[i] != nil {
			continue
		}
		part := parts[i]
		if !found {
			merged = Snapshot{HerdrVersion: part.HerdrVersion, HerdrProtocol: part.HerdrProtocol, CapturedAt: part.CapturedAt, Focused: part.Focused}
			found = true
		}
		merged.Workspaces = append(merged.Workspaces, part.Workspaces...)
		merged.Tabs = append(merged.Tabs, part.Tabs...)
		merged.Panes = append(merged.Panes, part.Panes...)
		merged.Layouts = append(merged.Layouts, part.Layouts...)
	}
	if !found {
		return Snapshot{}, errs[0]
	}
	return merged, nil
}

func defaultIDsAmbiguous(snapshot Snapshot) bool {
	ambiguous := func(id string) bool { return strings.Contains(id, mergedIDSeparator) }
	for _, workspace := range snapshot.Workspaces {
		if ambiguous(workspace.WorkspaceID) {
			return true
		}
	}
	for _, tab := range snapshot.Tabs {
		if ambiguous(tab.TabID) {
			return true
		}
	}
	for _, pane := range snapshot.Panes {
		if ambiguous(pane.PaneID) {
			return true
		}
	}
	return false
}

// qualifySnapshot rewrites every routable id of one session's snapshot and
// marks its workspaces with the session name. The session's own Snapshot is
// not modified. Terminal and agent-instance ids are identities, not routes,
// and stay as Herdr reported them.
func qualifySnapshot(session SessionRef, snapshot Snapshot) Snapshot {
	if session.Name == "" {
		return snapshot
	}
	q := func(id string) string { return qualifyMergedID(session, id) }
	out := snapshot
	out.Session = nil
	out.Focused = make(map[string]string, len(snapshot.Focused))
	for key, value := range snapshot.Focused {
		out.Focused[key] = q(value)
	}
	out.Workspaces = make([]Workspace, len(snapshot.Workspaces))
	for i, workspace := range snapshot.Workspaces {
		workspace.WorkspaceID = q(workspace.WorkspaceID)
		workspace.Session = session.Name
		out.Workspaces[i] = workspace
	}
	out.Tabs = make([]Tab, len(snapshot.Tabs))
	for i, tab := range snapshot.Tabs {
		tab.TabID, tab.WorkspaceID = q(tab.TabID), q(tab.WorkspaceID)
		out.Tabs[i] = tab
	}
	out.Panes = make([]Pane, len(snapshot.Panes))
	for i, pane := range snapshot.Panes {
		pane.PaneID, pane.WorkspaceID, pane.TabID = q(pane.PaneID), q(pane.WorkspaceID), q(pane.TabID)
		out.Panes[i] = pane
	}
	if snapshot.Layouts != nil {
		out.Layouts = make([]TabLayout, len(snapshot.Layouts))
		for i, layout := range snapshot.Layouts {
			layout.WorkspaceID, layout.TabID, layout.FocusedPaneID = q(layout.WorkspaceID), q(layout.TabID), q(layout.FocusedPaneID)
			panes := make([]LayoutPane, len(layout.Panes))
			for j, pane := range layout.Panes {
				pane.PaneID = q(pane.PaneID)
				panes[j] = pane
			}
			layout.Panes = panes
			out.Layouts[i] = layout
		}
	}
	return out
}

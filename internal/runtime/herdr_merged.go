package runtime

import (
	"context"
	"strings"
)

// Merged fronts every running Herdr session on this computer as one view, so a
// single pairing lists all of them at once. The default session keeps its own
// ids; each running named session's workspace, tab and pane ids are qualified
// as "<session>::<id>". A call naming a qualified id goes to that session's
// socket with the bare id, and any other call goes to the default session.
//
// Callers address the view as the default session. A named SessionRef is
// refused, so a connection has exactly one id space and the per-session
// switcher stays off (Merged is deliberately not a SessionLister).
type Merged struct {
	herdr *Herdr
}

const mergedIDSeparator = "::"

// NewMerged wraps h. Discovering named sessions needs h's multi-session
// switch, so merging turns it on.
func NewMerged(h *Herdr) *Merged {
	h.Multi = true
	return &Merged{herdr: h}
}

// Herdr is the adapter underneath, for local maintenance such as autostart.
func (m *Merged) Herdr() *Herdr { return m.herdr }

func refuseNamedSession(operation string, session SessionRef) error {
	if session.Name == "" {
		return nil
	}
	return &Fault{Code: CodeUnsupported, Operation: operation, Outcome: OutcomeNotApplied, Retry: RetryNever, SafeMessage: "Herdr sessions are merged into the default view"}
}

// splitMergedID returns the session owning a merged-view id and the id Herdr
// knows. Session names cannot contain ':', so the first separator decides.
func splitMergedID(operation, id string) (SessionRef, string, error) {
	i := strings.Index(id, mergedIDSeparator)
	if i < 0 {
		return DefaultSession(), id, nil
	}
	name, bare := id[:i], id[i+len(mergedIDSeparator):]
	if !validSessionName.MatchString(name) || name == "." || name == ".." ||
		strings.Contains(bare, mergedIDSeparator) || !validResourceID.MatchString(bare) {
		return SessionRef{}, "", invalidFault(operation, "invalid session-qualified id")
	}
	return NamedSession(name), bare, nil
}

func qualifyMergedID(session SessionRef, id string) string {
	if session.Name == "" || id == "" {
		return id
	}
	return session.Name + mergedIDSeparator + id
}

// routeMerged rewrites every non-empty id of one request to its bare form and
// returns the single session they share. Ids of different sessions cannot meet
// in one Herdr call; a request with no id goes to the default session.
func routeMerged(operation string, ids ...*string) (SessionRef, error) {
	var session SessionRef
	seen := false
	for _, id := range ids {
		if *id == "" {
			continue
		}
		target, bare, err := splitMergedID(operation, *id)
		if err != nil {
			return SessionRef{}, err
		}
		if seen && target != session {
			return SessionRef{}, invalidFault(operation, "ids belong to different Herdr sessions")
		}
		session, seen, *id = target, true, bare
	}
	return session, nil
}

// running lists the sessions to merge: always the default session, whose own
// call reports when it is offline, then every named session whose socket
// accepted a connection, in name order.
func (m *Merged) running(ctx context.Context) []SessionRef {
	out := []SessionRef{DefaultSession()}
	infos, err := m.herdr.Sessions(ctx)
	if err != nil {
		return out
	}
	for _, info := range infos {
		if info.Name != "" && info.Running {
			out = append(out, NamedSession(info.Name))
		}
	}
	return out
}

// Describe offers a feature only when every running session supports it and
// lists the agent kinds any of them can start.
func (m *Merged) Describe(ctx context.Context, session SessionRef) (Descriptor, error) {
	if err := refuseNamedSession("describe", session); err != nil {
		return Descriptor{}, err
	}
	sessions := m.running(ctx)
	descriptors := make([]Descriptor, len(sessions))
	errs := make([]error, len(sessions))
	eachSession(sessions, func(i int, target SessionRef) {
		descriptors[i], errs[i] = m.herdr.Describe(ctx, target)
	})
	var merged Descriptor
	found := false
	kinds := map[string]bool{}
	for i, target := range sessions {
		if errs[i] != nil {
			continue
		}
		part := descriptors[i]
		if !found {
			merged = Descriptor{Runtime: part.Runtime, Version: part.Version, Protocol: part.Protocol, Capabilities: map[Feature]Capability{}}
			for feature, capability := range part.Capabilities {
				merged.Capabilities[feature] = capability
			}
			found = true
		} else {
			for feature, capability := range merged.Capabilities {
				if capability.Available && !part.Supports(feature) {
					merged.Capabilities[feature] = Capability{Reason: "not supported by Herdr session " + target.Name}
				}
			}
		}
		for _, kind := range part.AgentKinds {
			if !kinds[kind.Kind] {
				kinds[kind.Kind] = true
				merged.AgentKinds = append(merged.AgentKinds, kind)
			}
		}
		for _, warning := range part.Warnings {
			if target.Name != "" {
				warning = target.Name + ": " + warning
			}
			merged.Warnings = append(merged.Warnings, warning)
		}
	}
	if !found {
		return Descriptor{}, errs[0]
	}
	return merged, nil
}

func (m *Merged) Observe(ctx context.Context, session SessionRef, query Query) (View, error) {
	if err := refuseNamedSession("observe", session); err != nil {
		return nil, err
	}
	switch value := query.(type) {
	case SnapshotQuery:
		snapshot, err := m.snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return SnapshotView{Snapshot: snapshot}, nil
	case PaneReadQuery:
		target, err := routeMerged("pane.read", &value.PaneID)
		if err != nil {
			return nil, err
		}
		return m.herdr.Observe(ctx, target, value)
	case HistoryQuery:
		target, err := routeMerged("history", &value.PaneID)
		if err != nil {
			return nil, err
		}
		return m.herdr.Observe(ctx, target, value)
	case AgentInspectQuery:
		target, err := routeMerged("agent.inspect", &value.PaneID)
		if err != nil {
			return nil, err
		}
		return m.herdr.Observe(ctx, target, value)
	case WorktreeListQuery:
		target, err := routeMerged("worktree.list", &value.WorkspaceID)
		if err != nil {
			return nil, err
		}
		view, err := m.herdr.Observe(ctx, target, value)
		if list, ok := view.(WorktreeListView); ok {
			view = qualifyWorktreeList(target, list)
		}
		return view, err
	case PaneProcessInfoQuery:
		target, err := routeMerged("pane.process_info", &value.PaneID)
		if err != nil {
			return nil, err
		}
		view, err := m.herdr.Observe(ctx, target, value)
		if info, ok := view.(PaneProcessInfoView); ok {
			info.PaneID = qualifyMergedID(target, info.PaneID)
			view = info
		}
		return view, err
	case MachineListQuery:
		return m.herdr.Observe(ctx, DefaultSession(), value)
	default:
		return nil, unsupported("observe", "unknown runtime query")
	}
}

func qualifyWorktreeList(session SessionRef, list WorktreeListView) WorktreeListView {
	list.Source.SourceWorkspace = qualifyMergedID(session, list.Source.SourceWorkspace)
	worktrees := make([]Worktree, len(list.Worktrees))
	for i, worktree := range list.Worktrees {
		if worktree.OpenWorkspaceID != nil {
			id := qualifyMergedID(session, *worktree.OpenWorkspaceID)
			worktree.OpenWorkspaceID = &id
		}
		worktrees[i] = worktree
	}
	list.Worktrees = worktrees
	return list
}

func (m *Merged) Execute(ctx context.Context, session SessionRef, operationID string, command Command) (Receipt, error) {
	if err := refuseNamedSession("execute", session); err != nil {
		return notApplied(operationID, err)
	}
	routed, target, err := routeMergedCommand(command)
	if err != nil {
		return notApplied(operationID, err)
	}
	receipt, err := m.herdr.Execute(ctx, target, operationID, routed)
	agentTarget := ""
	if prompt, ok := routed.(PromptAgentCommand); ok {
		agentTarget = prompt.Target
	}
	receipt.Created = qualifyEntityRefs(target, receipt.Created, agentTarget)
	receipt.Updated = qualifyEntityRefs(target, receipt.Updated, agentTarget)
	receipt.Removed = qualifyEntityRefs(target, receipt.Removed, agentTarget)
	return receipt, qualifyFault(target, err)
}

// routeMergedCommand rewrites the ids of every known command. An unknown
// command fails closed rather than reaching the default session with an id
// that may belong to another one.
func routeMergedCommand(command Command) (Command, SessionRef, error) {
	var target SessionRef
	var err error
	switch value := command.(type) {
	case SendTextCommand:
		target, err = routeMerged("pane.send_text", &value.PaneID)
		command = value
	case SendKeysCommand:
		target, err = routeMerged("pane.send_keys", &value.PaneID)
		command = value
	case RenamePaneCommand:
		target, err = routeMerged("pane.rename", &value.PaneID)
		command = value
	case ClosePaneCommand:
		target, err = routeMerged("pane.close", &value.PaneID)
		command = value
	case ResizePaneCommand:
		target, err = routeMerged("pane.resize", &value.PaneID)
		command = value
	case SwapPaneCommand:
		target, err = routeMerged("pane.swap", &value.PaneID)
		command = value
	case ZoomPaneCommand:
		target, err = routeMerged("pane.zoom", &value.PaneID)
		command = value
	case RenameTabCommand:
		target, err = routeMerged("tab.rename", &value.TabID)
		command = value
	case CloseTabCommand:
		target, err = routeMerged("tab.close", &value.TabID)
		command = value
	case RenameWorkspaceCommand:
		target, err = routeMerged("workspace.rename", &value.WorkspaceID)
		command = value
	case CloseWorkspaceCommand:
		target, err = routeMerged("workspace.close", &value.WorkspaceID)
		command = value
	case CreateConversationCommand:
		// A new conversation has no id to route by; it opens in the default session.
	case CreateTabCommand:
		target, err = routeMerged("tab.create", &value.WorkspaceID)
		command = value
	case SplitPaneCommand:
		target, err = routeMerged("pane.split", &value.WorkspaceID, &value.TargetPaneID, &value.TargetTabID)
		command = value
	case PromptAgentCommand:
		target, err = routeMerged("agent.prompt", &value.Target)
		command = value
	case WorktreeCreateCommand:
		target, err = routeMerged("worktree.create", &value.WorkspaceID)
		command = value
	case WorktreeOpenCommand:
		target, err = routeMerged("worktree.open", &value.WorkspaceID)
		command = value
	default:
		return nil, SessionRef{}, unsupported("execute", "unknown runtime command")
	}
	return command, target, err
}

// qualifyEntityRefs qualifies workspace, tab and pane refs. An agent ref is a
// Herdr agent name unless Herdr echoed the prompted pane id back.
func qualifyEntityRefs(session SessionRef, refs []EntityRef, agentTarget string) []EntityRef {
	if session.Name == "" || len(refs) == 0 {
		return refs
	}
	out := make([]EntityRef, len(refs))
	for i, ref := range refs {
		switch ref.Kind {
		case EntityWorkspace, EntityTab, EntityPane:
			ref.ID = qualifyMergedID(session, ref.ID)
		case EntityAgent:
			if agentTarget != "" && ref.ID == agentTarget {
				ref.ID = qualifyMergedID(session, ref.ID)
			}
		}
		out[i] = ref
	}
	return out
}

func qualifyFault(session SessionRef, err error) error {
	fault, ok := AsFault(err)
	if !ok || session.Name == "" || fault.Target == nil {
		return err
	}
	copied := *fault
	refs := qualifyEntityRefs(session, []EntityRef{*fault.Target}, "")
	copied.Target = &refs[0]
	return &copied
}

func (m *Merged) OpenTerminal(ctx context.Context, open TerminalOpen) (TerminalController, error) {
	if err := refuseNamedSession("terminal.open", open.Session); err != nil {
		return nil, err
	}
	target, bare, err := splitMergedID("terminal.open", open.PaneID)
	if err != nil {
		return nil, err
	}
	open.Session, open.PaneID = target, bare
	return m.herdr.OpenTerminal(ctx, open)
}

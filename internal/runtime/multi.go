package runtime

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sessionIDSeparator = "::"
	rootSessionName    = "default"
	multiProbeTimeout  = 250 * time.Millisecond
	multiDiscoveryTTL  = 2 * time.Second
)

// Multi fronts every live Herdr session on this computer as one Runtime so a
// single pairing carries all of them. Every workspace, tab and pane id that
// crosses the seam is qualified as "<session>::<id>": Herdr never sees the
// prefix and the phone never sees a bare id. The session named by
// HERDR_SOCKET_PATH (when it points inside the config root) is listed first so
// the phone's default focus follows the terminal Pairfob was started from.
type Multi struct {
	herdr   *Herdr
	current string

	mu         sync.Mutex
	discovered []string
	scannedAt  time.Time
	probe      time.Duration
	ttl        time.Duration
}

// OpenMulti builds the aggregate over ~/.config/herdr.
func OpenMulti() (*Multi, string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, "herdr:offline", &Fault{Code: CodeOffline, Outcome: OutcomeNotApplied, Retry: RetryReadSafe, SafeMessage: "cannot resolve Herdr config root"}
	}
	root := filepath.Join(home, ".config", "herdr")
	m := NewMulti(root)
	m.current = sessionForSocket(root, os.Getenv("HERDR_SOCKET_PATH"))
	return m, "herdr-multi:" + root, nil
}

// NewMulti aggregates the sessions under configRoot: the root socket is the
// "default" session, every sessions/<name>/herdr.sock is a named one.
func NewMulti(configRoot string) *Multi {
	h := NewHerdr(filepath.Join(configRoot, "herdr.sock"))
	h.ConfigRoot = configRoot
	h.Multi = true
	return &Multi{herdr: h, probe: multiProbeTimeout, ttl: multiDiscoveryTTL}
}

// Current returns the session listed first, or "" when none was inferred.
func (m *Multi) Current() string { return m.current }

func sessionForSocket(root, socket string) string {
	if socket == "" {
		return ""
	}
	socket = filepath.Clean(socket)
	if socket == filepath.Join(root, "herdr.sock") {
		return rootSessionName
	}
	dir, base := filepath.Split(socket)
	dir = filepath.Clean(dir)
	if base != "herdr.sock" || filepath.Dir(dir) != filepath.Join(root, "sessions") {
		return ""
	}
	name := filepath.Base(dir)
	if name == rootSessionName || !validSessionName.MatchString(name) {
		return ""
	}
	return name
}

func socketAlive(path string, timeout time.Duration) bool {
	connection, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

// Sessions lists the live sessions in display order. Results are cached for a
// short window so the daemon's poll loops do not dial every socket each tick.
func (m *Multi) Sessions(ctx context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.discovered != nil && time.Since(m.scannedAt) < m.ttl {
		return append([]string(nil), m.discovered...)
	}
	return m.rescanLocked()
}

func (m *Multi) rescanLocked() []string {
	names := make([]string, 0, 4)
	if socketAlive(m.herdr.Socket, m.probe) {
		names = append(names, rootSessionName)
	}
	entries, _ := os.ReadDir(filepath.Join(m.herdr.ConfigRoot, "sessions"))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == rootSessionName || !validSessionName.MatchString(name) {
			continue
		}
		if socketAlive(filepath.Join(m.herdr.ConfigRoot, "sessions", name, "herdr.sock"), m.probe) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for i, name := range names {
		if name == m.current && i > 0 {
			copy(names[1:i+1], names[:i])
			names[0] = name
			break
		}
	}
	m.discovered = names
	m.scannedAt = time.Now()
	return append([]string(nil), names...)
}

func (m *Multi) hasSession(ctx context.Context, name string) bool {
	for _, live := range m.Sessions(ctx) {
		if live == name {
			return true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, live := range m.rescanLocked() {
		if live == name {
			return true
		}
	}
	return false
}

func sessionRef(name string) SessionRef {
	if name == rootSessionName {
		return SessionRef{}
	}
	return NamedSession(name)
}

func qualify(session, id string) string {
	if id == "" {
		return ""
	}
	return session + sessionIDSeparator + id
}

func qualifyRefs(session string, refs []EntityRef) []EntityRef {
	for i := range refs {
		switch refs[i].Kind {
		case EntityWorkspace, EntityTab, EntityPane:
			refs[i].ID = qualify(session, refs[i].ID)
		}
	}
	return refs
}

func noLiveSessions(operation string) error {
	return &Fault{Code: CodeOffline, Operation: operation, Outcome: OutcomeNotApplied, Retry: RetryReadSafe, SafeMessage: "no live Herdr session under the config root"}
}

// split turns "<session>::<id>" into its parts and refuses bare or unknown ids
// so a mistyped prefix can never reach a different session's socket.
func (m *Multi) split(ctx context.Context, qualified, operation string) (string, string, error) {
	i := strings.Index(qualified, sessionIDSeparator)
	if i <= 0 || i+len(sessionIDSeparator) >= len(qualified) {
		return "", "", invalidFault(operation, "resource id must be qualified as <session>::<id>")
	}
	session, bare := qualified[:i], qualified[i+len(sessionIDSeparator):]
	if !validSessionName.MatchString(session) || !validResourceID.MatchString(bare) {
		return "", "", invalidFault(operation, "invalid qualified resource id")
	}
	if !m.hasSession(ctx, session) {
		return "", "", &Fault{Code: CodeNotFound, Operation: operation, Outcome: OutcomeNotApplied, Retry: RetryNever, SafeMessage: "Herdr session is not live: " + session}
	}
	return session, bare, nil
}

// splitOptional accepts an empty id (route to the first live session).
func (m *Multi) splitOptional(ctx context.Context, qualified, operation string) (string, string, error) {
	if qualified == "" {
		names := m.Sessions(ctx)
		if len(names) == 0 {
			return "", "", noLiveSessions(operation)
		}
		return names[0], "", nil
	}
	return m.split(ctx, qualified, operation)
}

func (m *Multi) Describe(ctx context.Context, _ SessionRef) (Descriptor, error) {
	names := m.Sessions(ctx)
	if len(names) == 0 {
		return Descriptor{}, noLiveSessions("describe")
	}
	var out Descriptor
	var lastErr error
	kinds := map[string]struct{}{}
	described := false
	for _, name := range names {
		descriptor, err := m.herdr.Describe(ctx, sessionRef(name))
		if err != nil {
			lastErr = err
			out.Warnings = append(out.Warnings, name+": "+err.Error())
			continue
		}
		for _, kind := range descriptor.AgentKinds {
			kinds[kind.Kind] = struct{}{}
		}
		out.Warnings = append(out.Warnings, descriptor.Warnings...)
		if !described {
			out.Runtime, out.Version, out.Protocol = descriptor.Runtime, descriptor.Version, descriptor.Protocol
			out.Capabilities = make(map[Feature]Capability, len(descriptor.Capabilities))
			for feature, capability := range descriptor.Capabilities {
				out.Capabilities[feature] = capability
			}
			described = true
			continue
		}
		// A feature is offered only when every live session supports it.
		for feature, capability := range out.Capabilities {
			other, ok := descriptor.Capabilities[feature]
			if capability.Available && (!ok || !other.Available) {
				out.Capabilities[feature] = Capability{Reason: "unsupported by session " + name}
			}
		}
	}
	if !described {
		return Descriptor{}, lastErr
	}
	names = make([]string, 0, len(kinds))
	for kind := range kinds {
		names = append(names, kind)
	}
	sort.Strings(names)
	out.AgentKinds = make([]AgentKind, 0, len(names))
	for _, kind := range names {
		out.AgentKinds = append(out.AgentKinds, AgentKind{Kind: kind})
	}
	return out, nil
}

func (m *Multi) Observe(ctx context.Context, _ SessionRef, query Query) (View, error) {
	switch value := query.(type) {
	case SnapshotQuery:
		return m.mergedSnapshot(ctx)
	case PaneReadQuery:
		session, bare, err := m.split(ctx, value.PaneID, "pane.read")
		if err != nil {
			return nil, err
		}
		value.PaneID = bare
		return m.herdr.Observe(ctx, sessionRef(session), value)
	case HistoryQuery:
		session, bare, err := m.split(ctx, value.PaneID, "history")
		if err != nil {
			return nil, err
		}
		value.PaneID = bare
		return m.herdr.Observe(ctx, sessionRef(session), value)
	case WorktreeListQuery:
		session, bare, err := m.splitOptional(ctx, value.WorkspaceID, "worktree.list")
		if err != nil {
			return nil, err
		}
		value.WorkspaceID = bare
		view, err := m.herdr.Observe(ctx, sessionRef(session), value)
		if err != nil {
			return nil, err
		}
		list, ok := view.(WorktreeListView)
		if !ok {
			return view, nil
		}
		list.Source.SourceWorkspace = qualify(session, list.Source.SourceWorkspace)
		for i := range list.Worktrees {
			if open := list.Worktrees[i].OpenWorkspaceID; open != nil && *open != "" {
				qualified := qualify(session, *open)
				list.Worktrees[i].OpenWorkspaceID = &qualified
			}
		}
		return list, nil
	default:
		return nil, unsupported("observe", "unknown runtime query")
	}
}

func (m *Multi) mergedSnapshot(ctx context.Context) (View, error) {
	names := m.Sessions(ctx)
	if len(names) == 0 {
		return nil, noLiveSessions("session.snapshot")
	}
	merged := Snapshot{CapturedAt: time.Now().Unix(), Focused: map[string]string{}}
	var lastErr error
	for _, name := range names {
		snapshot, err := m.herdr.snapshot(ctx, sessionRef(name))
		if err != nil {
			lastErr = err
			continue
		}
		if merged.HerdrVersion == "" {
			merged.HerdrVersion, merged.HerdrProtocol = snapshot.HerdrVersion, snapshot.HerdrProtocol
			for key, id := range snapshot.Focused {
				merged.Focused[key] = qualify(name, id)
			}
		}
		for _, workspace := range snapshot.Workspaces {
			workspace.WorkspaceID = qualify(name, workspace.WorkspaceID)
			workspace.Session = name
			merged.Workspaces = append(merged.Workspaces, workspace)
		}
		for _, tab := range snapshot.Tabs {
			tab.TabID, tab.WorkspaceID = qualify(name, tab.TabID), qualify(name, tab.WorkspaceID)
			tab.Session = name
			merged.Tabs = append(merged.Tabs, tab)
		}
		for _, pane := range snapshot.Panes {
			pane.PaneID, pane.WorkspaceID, pane.TabID = qualify(name, pane.PaneID), qualify(name, pane.WorkspaceID), qualify(name, pane.TabID)
			pane.Session = name
			merged.Panes = append(merged.Panes, pane)
		}
		merged.Sessions = append(merged.Sessions, SessionInfo{Name: name, Workspaces: len(snapshot.Workspaces), Panes: len(snapshot.Panes)})
	}
	if merged.HerdrVersion == "" {
		return nil, lastErr
	}
	return SnapshotView{Snapshot: merged}, nil
}

func (m *Multi) Execute(ctx context.Context, _ SessionRef, operationID string, command Command) (Receipt, error) {
	session, routed, err := m.route(ctx, command)
	if err != nil {
		return Receipt{OperationID: operationID, Outcome: OutcomeNotApplied}, err
	}
	receipt, err := m.herdr.Execute(ctx, sessionRef(session), operationID, routed)
	receipt.Created = qualifyRefs(session, receipt.Created)
	receipt.Updated = qualifyRefs(session, receipt.Updated)
	receipt.Removed = qualifyRefs(session, receipt.Removed)
	return receipt, err
}

// route strips the session prefix from every id a command carries. Commands
// without an id (or with an empty optional one) go to the first live session.
func (m *Multi) route(ctx context.Context, command Command) (string, Command, error) {
	switch value := command.(type) {
	case SendTextCommand:
		session, bare, err := m.split(ctx, value.PaneID, "pane.send_text")
		value.PaneID = bare
		return session, value, err
	case SendKeysCommand:
		session, bare, err := m.split(ctx, value.PaneID, "pane.send_keys")
		value.PaneID = bare
		return session, value, err
	case RenamePaneCommand:
		session, bare, err := m.split(ctx, value.PaneID, "pane.rename")
		value.PaneID = bare
		return session, value, err
	case RenameTabCommand:
		session, bare, err := m.split(ctx, value.TabID, "tab.rename")
		value.TabID = bare
		return session, value, err
	case RenameWorkspaceCommand:
		session, bare, err := m.split(ctx, value.WorkspaceID, "workspace.rename")
		value.WorkspaceID = bare
		return session, value, err
	case ClosePaneCommand:
		session, bare, err := m.split(ctx, value.PaneID, "pane.close")
		value.PaneID = bare
		return session, value, err
	case CloseTabCommand:
		session, bare, err := m.split(ctx, value.TabID, "tab.close")
		value.TabID = bare
		return session, value, err
	case CloseWorkspaceCommand:
		session, bare, err := m.split(ctx, value.WorkspaceID, "workspace.close")
		value.WorkspaceID = bare
		return session, value, err
	case CreateConversationCommand:
		session, _, err := m.splitOptional(ctx, "", "workspace.create")
		return session, value, err
	case CreateTabCommand:
		session, bare, err := m.split(ctx, value.WorkspaceID, "tab.create")
		value.WorkspaceID = bare
		return session, value, err
	case SplitPaneCommand:
		session, bare, err := m.split(ctx, value.TargetPaneID, "pane.split")
		if err != nil {
			return "", value, err
		}
		value.TargetPaneID = bare
		if value.WorkspaceID != "" {
			workspaceSession, workspace, err := m.split(ctx, value.WorkspaceID, "pane.split")
			if err != nil {
				return "", value, err
			}
			if workspaceSession != session {
				return "", value, invalidFault("pane.split", "workspace and target pane belong to different sessions")
			}
			value.WorkspaceID = workspace
		}
		return session, value, nil
	case PromptAgentCommand:
		// Target may be a pane id or an agent name; only a qualified id is stripped.
		if strings.Contains(value.Target, sessionIDSeparator) {
			session, bare, err := m.split(ctx, value.Target, "agent.prompt")
			value.Target = bare
			return session, value, err
		}
		session, _, err := m.splitOptional(ctx, "", "agent.prompt")
		return session, value, err
	case WorktreeCreateCommand:
		session, bare, err := m.splitOptional(ctx, value.WorkspaceID, "worktree.create")
		value.WorkspaceID = bare
		return session, value, err
	case WorktreeOpenCommand:
		session, bare, err := m.splitOptional(ctx, value.WorkspaceID, "worktree.open")
		value.WorkspaceID = bare
		return session, value, err
	case ResizePaneCommand:
		session, bare, err := m.split(ctx, value.PaneID, "pane.resize")
		value.PaneID = bare
		return session, value, err
	case SwapPaneCommand:
		session, bare, err := m.split(ctx, value.PaneID, "pane.swap")
		value.PaneID = bare
		return session, value, err
	case ZoomPaneCommand:
		session, bare, err := m.split(ctx, value.PaneID, "pane.zoom")
		value.PaneID = bare
		return session, value, err
	default:
		return "", command, unsupported("execute", "unknown runtime command")
	}
}

// OpenTerminal attaches to the pane's own session; the bare id is what the
// Herdr CLI receives and the session name becomes its --session flag.
func (m *Multi) OpenTerminal(ctx context.Context, open TerminalOpen) (TerminalController, error) {
	session, bare, err := m.split(ctx, open.PaneID, "terminal.open")
	if err != nil {
		return nil, err
	}
	open.Session = sessionRef(session)
	open.PaneID = bare
	return m.herdr.OpenTerminal(ctx, open)
}

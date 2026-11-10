package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	maxPaneForegroundProcesses = 256
	maxPaneProcessArgv         = 256
	maxPaneProcessArgBytes     = 64 * 1024
	maxPaneProcessArgBytesAll  = 256 * 1024
	maxPaneProcessCwdBytes     = 16 * 1024
	maxPaneProcessPID          = 1<<31 - 1
)

// PaneProcessInfoQuery is internal runtime evidence used to resolve the owner
// of a live pane. Its trusted identity must not cross a JSON boundary. The
// local hook binding and new-shell readiness checks keep their own narrower
// pane.process_info reads; this is the complete, strictly validated view.
type PaneProcessInfoQuery struct {
	PaneID string `json:"-"`
}

func (PaneProcessInfoQuery) runtimeQuery() {}

// PaneProcessInfoView preserves exactly the process identity Herdr reports.
// Missing nullable fields and empty process data remain unknown.
type PaneProcessInfoView struct {
	PaneID                   string                  `json:"-"`
	ShellPID                 *int                    `json:"-"`
	ForegroundProcessGroupID *int                    `json:"-"`
	ForegroundProcesses      []PaneForegroundProcess `json:"-"`
}

func (PaneProcessInfoView) runtimeView() {}

type PaneForegroundProcess struct {
	PID  int      `json:"-"`
	Argv []string `json:"-"`
	Cwd  *string  `json:"-"`
}

type herdrPaneProcessInfoWire struct {
	PaneID                   string                       `json:"pane_id"`
	ShellPID                 *int64                       `json:"shell_pid"`
	ForegroundProcessGroupID *int64                       `json:"foreground_process_group_id"`
	ForegroundProcesses      []herdrForegroundProcessWire `json:"foreground_processes"`
}

type herdrForegroundProcessWire struct {
	PID  int64    `json:"pid"`
	Argv []string `json:"argv"`
	Cwd  *string  `json:"cwd"`
}

func (h *Herdr) paneProcessInfo(ctx context.Context, session SessionRef, query PaneProcessInfoQuery) (View, error) {
	if !validResourceID.MatchString(query.PaneID) {
		return nil, invalidFault("pane.process_info", "invalid pane id")
	}
	raw, err := h.call(ctx, session, "pane.process_info", map[string]any{"pane_id": query.PaneID}, false)
	if err != nil {
		return nil, err
	}
	var response struct {
		Type        string                    `json:"type"`
		ProcessInfo *herdrPaneProcessInfoWire `json:"process_info"`
	}
	if !utf8.Valid(raw) {
		return nil, responseFault("pane.process_info", "invalid Herdr pane process info response", nil, false)
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, responseFault("pane.process_info", "invalid Herdr pane process info response", err, false)
	}
	if response.Type != "pane_process_info" || response.ProcessInfo == nil {
		return nil, responseFault("pane.process_info", "invalid Herdr pane process info response", nil, false)
	}
	info := response.ProcessInfo
	if !validResourceID.MatchString(info.PaneID) || info.PaneID != query.PaneID {
		return nil, responseFault("pane.process_info", "invalid Herdr pane process info target", nil, false)
	}
	if !validOptionalPID(info.ShellPID) || !validOptionalPID(info.ForegroundProcessGroupID) {
		return nil, responseFault("pane.process_info", "invalid Herdr pane process identity", nil, false)
	}
	if len(info.ForegroundProcesses) > maxPaneForegroundProcesses {
		return nil, responseFault("pane.process_info", "too many Herdr foreground processes", nil, false)
	}

	view := PaneProcessInfoView{PaneID: info.PaneID}
	view.ShellPID = copyValidatedPID(info.ShellPID)
	view.ForegroundProcessGroupID = copyValidatedPID(info.ForegroundProcessGroupID)
	if info.ForegroundProcesses != nil {
		view.ForegroundProcesses = make([]PaneForegroundProcess, 0, len(info.ForegroundProcesses))
	}
	seen := make(map[int64]struct{}, len(info.ForegroundProcesses))
	totalArgBytes := 0
	for _, process := range info.ForegroundProcesses {
		if process.PID <= 0 || process.PID > maxPaneProcessPID {
			return nil, responseFault("pane.process_info", "invalid Herdr foreground process pid", nil, false)
		}
		if _, duplicate := seen[process.PID]; duplicate {
			return nil, responseFault("pane.process_info", "duplicate Herdr foreground process pid", nil, false)
		}
		seen[process.PID] = struct{}{}
		if len(process.Argv) > maxPaneProcessArgv {
			return nil, responseFault("pane.process_info", "too many Herdr foreground process arguments", nil, false)
		}
		for _, arg := range process.Argv {
			if !validProcessString(arg, maxPaneProcessArgBytes) {
				return nil, responseFault("pane.process_info", "invalid Herdr foreground process argument", nil, false)
			}
			totalArgBytes += len(arg)
			if totalArgBytes > maxPaneProcessArgBytesAll {
				return nil, responseFault("pane.process_info", "Herdr foreground process arguments are too large", nil, false)
			}
		}
		if process.Cwd != nil && !validProcessCwd(*process.Cwd) {
			return nil, responseFault("pane.process_info", "invalid Herdr foreground process cwd", nil, false)
		}
		item := PaneForegroundProcess{PID: int(process.PID)}
		if process.Argv != nil {
			item.Argv = append(make([]string, 0, len(process.Argv)), process.Argv...)
		}
		if process.Cwd != nil {
			cwd := *process.Cwd
			item.Cwd = &cwd
		}
		view.ForegroundProcesses = append(view.ForegroundProcesses, item)
	}
	return view, nil
}

func validOptionalPID(pid *int64) bool {
	return pid == nil || (*pid > 0 && *pid <= maxPaneProcessPID)
}

func copyValidatedPID(pid *int64) *int {
	if pid == nil {
		return nil
	}
	value := int(*pid)
	return &value
}

func validProcessString(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validProcessCwd(cwd string) bool {
	return cwd != "" && validProcessString(cwd, maxPaneProcessCwdBytes) && filepath.IsAbs(cwd) && filepath.Clean(cwd) == cwd
}

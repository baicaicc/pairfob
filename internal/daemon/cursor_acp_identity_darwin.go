//go:build darwin

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pairfob/internal/journal"
)

const cursorIdentityOutputMax = 256 << 10

var darwinIdentityRow = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(.+)$`)

type cursorCommandResult struct {
	stdout []byte
	stderr []byte
}

func nativeCursorProcessIdentity(ctx context.Context, pid int) (*journal.CursorProcessIdentity, error) {
	return darwinCursorProcessIdentity(ctx, pid, runCursorIdentityCommand, filepath.EvalSymlinks)
}

type cursorCommandRunner func(context.Context, string, ...string) (cursorCommandResult, error)

func darwinCursorProcessIdentity(ctx context.Context, pid int, run cursorCommandRunner, resolve func(string) (string, error)) (*journal.CursorProcessIdentity, error) {
	if pid <= 0 {
		return nil, errors.New("invalid process id")
	}
	args := []string{"-ww", "-p", strconv.Itoa(pid), "-o", "pid=,ppid=,lstart="}
	first, err := run(ctx, "/bin/ps", args...)
	if err != nil {
		if cursorPIDProvenAbsent(first, err) {
			return nil, nil
		}
		return nil, err
	}
	firstText := strings.TrimSpace(string(first.stdout))
	row := darwinIdentityRow.FindStringSubmatch(firstText)
	if row == nil {
		return nil, errors.New("process identity response was not recognized")
	}
	executableResult, err := run(ctx, "/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "comm=")
	if err != nil {
		return nil, err
	}
	commandResult, err := run(ctx, "/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=")
	if err != nil {
		return nil, err
	}
	bootResult, err := run(ctx, "/usr/sbin/sysctl", "-n", "kern.boottime")
	if err != nil {
		return nil, err
	}
	last, err := run(ctx, "/bin/ps", args...)
	if err != nil || firstText != strings.TrimSpace(string(last.stdout)) {
		return nil, errors.New("process changed during inspection")
	}
	executable, err := resolve(strings.TrimSpace(string(executableResult.stdout)))
	if err != nil {
		return nil, err
	}
	parsedPID, _ := strconv.Atoi(row[1])
	identity := &journal.CursorProcessIdentity{PID: parsedPID, StartTime: row[3], BootID: strings.TrimSpace(string(bootResult.stdout)), Executable: executable, CommandLine: strings.TrimSpace(string(commandResult.stdout))}
	if identity.PID != pid || identity.StartTime == "" || identity.BootID == "" || identity.Executable == "" || identity.CommandLine == "" {
		return nil, errors.New("process identity is incomplete")
	}
	return identity, nil
}

func cursorPIDProvenAbsent(result cursorCommandResult, err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == 1 && len(bytes.TrimSpace(result.stdout)) == 0 && len(bytes.TrimSpace(result.stderr)) == 0
}

func runCursorIdentityCommand(ctx context.Context, executable string, args ...string) (cursorCommandResult, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, executable, args...)
	cmd.Env = cursorCommandEnv()
	var stdout, stderr boundedCursorBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := cursorCommandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if stdout.overflow || stderr.overflow || len(stdout.Bytes())+len(stderr.Bytes()) > cursorIdentityOutputMax {
		return result, fmt.Errorf("process identity command output exceeds %d bytes", cursorIdentityOutputMax)
	}
	if commandCtx.Err() != nil {
		return result, commandCtx.Err()
	}
	return result, err
}

func cursorCommandEnv() []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "LC_ALL=") {
			environment = append(environment, item)
		}
	}
	return append(environment, "LC_ALL=C")
}

type boundedCursorBuffer struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *boundedCursorBuffer) Write(data []byte) (int, error) {
	remaining := cursorIdentityOutputMax - b.buffer.Len()
	if remaining > 0 {
		_, _ = b.buffer.Write(data[:min(remaining, len(data))])
	}
	if len(data) > remaining {
		b.overflow = true
	}
	return len(data), nil
}

func (b *boundedCursorBuffer) Bytes() []byte { return append([]byte(nil), b.buffer.Bytes()...) }

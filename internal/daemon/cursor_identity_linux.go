//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"pairfob/internal/journal"
)

const cursorIdentityOutputMax = 256 << 10

func nativeCursorProcessIdentity(ctx context.Context, pid int) (*journal.CursorProcessIdentity, error) {
	if pid <= 0 {
		return nil, errors.New("invalid process id")
	}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	first, err := readCursorIdentityFile(ctx, filepath.Join(root, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	start, err := linuxStartTime(first)
	if err != nil {
		return nil, err
	}
	executable, err := filepath.EvalSymlinks(filepath.Join(root, "exe"))
	if err != nil {
		return nil, err
	}
	cmdline, err := readCursorIdentityFile(ctx, filepath.Join(root, "cmdline"))
	if err != nil {
		return nil, err
	}
	bootID, err := readCursorIdentityFile(ctx, "/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	last, err := readCursorIdentityFile(ctx, filepath.Join(root, "stat"))
	if err != nil {
		return nil, err
	}
	after, err := linuxStartTime(last)
	if err != nil || start != after {
		return nil, errors.New("process changed during inspection")
	}
	commandLine := strings.ReplaceAll(strings.TrimSuffix(string(cmdline), "\x00"), "\x00", " ")
	identity := &journal.CursorProcessIdentity{PID: pid, StartTime: start, BootID: strings.TrimSpace(string(bootID)), Executable: executable, CommandLine: commandLine}
	if identity.BootID == "" || identity.Executable == "" || identity.CommandLine == "" {
		return nil, errors.New("process identity is incomplete")
	}
	return identity, nil
}

func linuxStartTime(raw []byte) (string, error) {
	closeParen := strings.LastIndexByte(string(raw), ')')
	if closeParen < 0 || closeParen+2 > len(raw) {
		return "", errors.New("unrecognized /proc stat")
	}
	fields := strings.Fields(string(raw[closeParen+2:]))
	if len(fields) <= 19 || fields[19] == "" {
		return "", errors.New("unrecognized /proc stat")
	}
	return fields[19], nil
}

func readCursorIdentityFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, cursorIdentityOutputMax+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > cursorIdentityOutputMax {
		return nil, fmt.Errorf("process identity output exceeds %d bytes", cursorIdentityOutputMax)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

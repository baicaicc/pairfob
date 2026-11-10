//go:build darwin

package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDarwinCursorIdentityUsesExactCommandsAndStableSample(t *testing.T) {
	type call struct {
		path string
		args []string
	}
	var calls []call
	results := []string{
		" 42 1 Sun Sep  6 12:00:00 2026",
		"/private/tmp/node",
		"/private/tmp/node /private/tmp/entry.mjs /private/tmp/binding.json",
		"{ sec = 123, usec = 456 } Sun Sep  6 00:00:00 2026",
		" 42 1 Sun Sep  6 12:00:00 2026",
	}
	run := func(_ context.Context, path string, args ...string) (cursorCommandResult, error) {
		calls = append(calls, call{path: path, args: append([]string(nil), args...)})
		return cursorCommandResult{stdout: []byte(results[len(calls)-1])}, nil
	}
	identity, err := darwinCursorProcessIdentity(context.Background(), 42, run, func(path string) (string, error) { return path, nil })
	if err != nil {
		t.Fatal(err)
	}
	expected := []call{
		{path: "/bin/ps", args: []string{"-ww", "-p", "42", "-o", "pid=,ppid=,lstart="}},
		{path: "/bin/ps", args: []string{"-ww", "-p", "42", "-o", "comm="}},
		{path: "/bin/ps", args: []string{"-ww", "-p", "42", "-o", "command="}},
		{path: "/usr/sbin/sysctl", args: []string{"-n", "kern.boottime"}},
		{path: "/bin/ps", args: []string{"-ww", "-p", "42", "-o", "pid=,ppid=,lstart="}},
	}
	if !reflect.DeepEqual(calls, expected) || identity.PID != 42 || identity.StartTime != "Sun Sep  6 12:00:00 2026" || identity.Executable != "/private/tmp/node" {
		t.Fatalf("unexpected identity/calls: %#v %#v", identity, calls)
	}
}

func TestDarwinCursorIdentityRejectsErrorsAndReuse(t *testing.T) {
	failure := errors.New("ps failed")
	if _, err := darwinCursorProcessIdentity(context.Background(), 42, func(context.Context, string, ...string) (cursorCommandResult, error) {
		return cursorCommandResult{}, failure
	}, func(path string) (string, error) { return path, nil }); !errors.Is(err, failure) {
		t.Fatalf("command error was not preserved: %v", err)
	}

	results := []string{"42 1 first", "/node", "/node /entry /binding", "boot", "42 1 second"}
	index := 0
	_, err := darwinCursorProcessIdentity(context.Background(), 42, func(context.Context, string, ...string) (cursorCommandResult, error) {
		result := cursorCommandResult{stdout: []byte(results[index])}
		index++
		return result, nil
	}, func(path string) (string, error) { return path, nil })
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("PID reuse was accepted: %v", err)
	}
}

type cursorFakeExitError int

func (e cursorFakeExitError) Error() string { return "exited" }
func (e cursorFakeExitError) ExitCode() int { return int(e) }

func TestDarwinCursorIdentityOnlyTreatsSilentExitOneAsAbsent(t *testing.T) {
	if identity, err := darwinCursorProcessIdentity(context.Background(), 42, func(context.Context, string, ...string) (cursorCommandResult, error) {
		return cursorCommandResult{}, cursorFakeExitError(1)
	}, func(path string) (string, error) { return path, nil }); err != nil || identity != nil {
		t.Fatalf("silent ps exit 1 was not absence: %#v %v", identity, err)
	}
	for _, result := range []cursorCommandResult{{stderr: []byte("denied")}, {stdout: []byte("unexpected")}} {
		if _, err := darwinCursorProcessIdentity(context.Background(), 42, func(context.Context, string, ...string) (cursorCommandResult, error) {
			return result, cursorFakeExitError(1)
		}, func(path string) (string, error) { return path, nil }); err == nil {
			t.Fatal("non-silent ps failure was treated as absence")
		}
	}
}

func TestDarwinCursorIdentityOutputIsBounded(t *testing.T) {
	var buffer boundedCursorBuffer
	payload := make([]byte, cursorIdentityOutputMax+1)
	if n, err := buffer.Write(payload); err != nil || n != len(payload) || !buffer.overflow || len(buffer.Bytes()) != cursorIdentityOutputMax {
		t.Fatalf("bounded writer failed: n=%d err=%v overflow=%v size=%d", n, err, buffer.overflow, len(buffer.Bytes()))
	}
}

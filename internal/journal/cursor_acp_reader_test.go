//go:build darwin || linux

package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestCursorReaderProjectsOnlyVerifiedPublicConversation(t *testing.T) {
	f := newCursorReaderFixture(t)
	operations := []map[string]any{
		cursorTestOperation(f, "op-zero", "run-zero", "", "", nil),
		cursorTestOperation(f, "op-one", "run-one", "<wrapper>secret</wrapper>\nraw wire", "", map[string]any{"toolCalls": []any{"secret"}}),
		cursorTestOperation(f, "op-two", "run-two", "你好🙂", "answer two", map[string]any{"interactions": []any{"secret"}}),
		cursorTestOperation(f, "op-three", "run-three", "private three", "assistant only", nil),
		cursorTestOperation(f, "op-four", "run-four", "empty user wire", "after empty", nil),
		cursorTestOperation(f, "op-five", "run-five", "你好🙂", "answer five", map[string]any{"error": "private error"}),
	}
	delete(operations[1], "status")
	operations[2]["status"] = map[string]any{"ignored": true}
	f.writeSession(2, operations, map[string]any{"publicHistory": []any{map[string]any{"text": "unverified secret"}}, "output": "private output"})
	f.writeInput(operations[1], "service", "original one", "")
	f.writeInput(operations[2], "terminal", "你好🙂", "old-terminal")
	f.writeInput(operations[4], "service", "", "")
	f.writeInput(operations[5], "terminal", "你好🙂", "another-terminal")

	if !f.reader.Supports(f.ref) || !f.reader.Available(f.ref) {
		t.Fatal("readable Cursor ref was not available")
	}
	page, err := f.reader.Read(f.ref, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{{Role: "user", Text: "original one"}, {Role: "user", Text: "你好🙂"}, {Role: "assistant", Text: "answer two"}, {Role: "assistant", Text: "assistant only"}, {Role: "assistant", Text: "after empty"}, {Role: "user", Text: "你好🙂"}, {Role: "assistant", Text: "answer five"}}
	if !reflectMessages(page.Messages, want) || page.NextCursor != nil {
		t.Fatalf("history=%+v", page)
	}
	encoded := messagesText(page.Messages)
	for _, secret := range []string{"wrapper", "raw wire", "private three", "unverified secret", "private output", "private error"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("private field %q escaped projection: %s", secret, encoded)
		}
	}
	trace, err := f.reader.ReadTrace(f.ref, nil, 4)
	if err != nil || len(trace.Items) != 4 || trace.Items[0].Text != "assistant only" || trace.Items[3].Text != "answer five" || trace.NextCursor == nil {
		t.Fatalf("trace=%+v err=%v", trace, err)
	}
	summary, err := f.reader.ReadTraceSummary(f.ref, nil, 4)
	if err != nil || len(summary.Items) != 4 || summary.Items[0].Text != "assistant only" {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if _, err := f.reader.ReadTraceDetail(f.ref, "anything"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Cursor detail error=%v", err)
	}
}

func TestCursorReaderPaginationIsDisjointStableAndModeBound(t *testing.T) {
	f := newCursorReaderFixture(t)
	ops := []map[string]any{
		cursorTestOperation(f, "op-a", "run-a", "same", "", nil),
		cursorTestOperation(f, "op-b", "run-b", "same", "answer b", nil),
		cursorTestOperation(f, "op-c", "run-c", "wire c", "answer c", nil),
	}
	f.writeSession(2, ops, nil)
	for index, op := range ops {
		f.writeInput(op, "service", "question "+string(rune('A'+index)), "")
	}

	first, err := f.reader.Read(f.ref, nil, 1)
	if err != nil || first.NextCursor == nil || len(first.Messages) != 1 || first.Messages[0].Text != "question A" {
		t.Fatalf("first history=%+v err=%v", first, err)
	}
	var history []Message
	cursor := first.NextCursor
	history = append(history, first.Messages...)
	for cursor != nil {
		page, readErr := f.reader.Read(f.ref, cursor, 1)
		if readErr != nil {
			t.Fatal(readErr)
		}
		history = append(history, page.Messages...)
		cursor = page.NextCursor
	}
	if got := messagesText(history); got != "question A|question B|answer b|question C|answer c" {
		t.Fatalf("history pages overlap or skip: %s", got)
	}

	latest, err := f.reader.ReadTrace(f.ref, nil, 1)
	if err != nil || latest.NextCursor == nil || latest.Items[0].Text != "answer c" {
		t.Fatalf("latest trace=%+v err=%v", latest, err)
	}
	if _, err := f.reader.Read(f.ref, latest.NextCursor, 1); !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("trace cursor crossed into History: %v", err)
	}
	if _, err := f.reader.ReadTrace(f.ref, first.NextCursor, 1); !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("History cursor crossed into trace: %v", err)
	}

	ops[2]["publicText"] = "answer c streaming growth"
	ops = append(ops, cursorTestOperation(f, "op-d", "run-d", "wire d", "answer d", nil))
	f.writeSession(3, ops, nil)
	older, err := f.reader.ReadTrace(f.ref, latest.NextCursor, 20)
	if err != nil || strings.Contains(traceText(older.Items), "answer d") || !strings.Contains(traceText(older.Items), "question C") {
		t.Fatalf("old trace cursor did not survive growth/append: %+v, %v", older, err)
	}
	continued, err := f.reader.Read(f.ref, first.NextCursor, 20)
	if err != nil || strings.Contains(messagesText(continued.Messages), "question A") || !strings.Contains(messagesText(continued.Messages), "answer d") {
		t.Fatalf("old History cursor did not survive append: %+v, %v", continued, err)
	}

	ops[0], ops[1] = ops[1], ops[0]
	f.writeSession(4, ops, nil)
	if _, err := f.reader.Read(f.ref, first.NextCursor, 1); !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("reordered anchor prefix was accepted: %v", err)
	}
	ops[0], ops[1] = ops[1], ops[0]
	deleted := []map[string]any{ops[0], ops[1], ops[3]}
	f.writeSession(5, deleted, nil)
	if _, err := f.reader.Read(f.ref, first.NextCursor, 1); !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("deleted prior snapshot anchor was accepted: %v", err)
	}
	ops[1] = cursorTestOperation(f, "op-replaced", "run-replaced", "same", "replacement", nil)
	f.writeSession(6, ops, nil)
	if _, err := f.reader.Read(f.ref, first.NextCursor, 1); !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("replaced prior snapshot anchor was accepted: %v", err)
	}
	if strings.Contains(*first.NextCursor, f.manifest.OwnerDir) || strings.Contains(*first.NextCursor, "op-a") || len(*first.NextCursor) > 1024 {
		t.Fatalf("cursor exposed private identity: %q", *first.NextCursor)
	}
}

func TestCursorReaderFailsClosedForOperationAndInputTampering(t *testing.T) {
	for _, field := range []string{"text", "publicText"} {
		t.Run("null operation "+field, func(t *testing.T) {
			f := newCursorReaderFixture(t)
			op := cursorTestOperation(f, "op-one", "run-one", "", "", nil)
			op[field] = nil
			f.writeSession(2, []map[string]any{op}, nil)
			if f.reader.Available(f.ref) {
				t.Fatalf("null %s was available", field)
			}
			if _, err := f.reader.Read(f.ref, nil, 20); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("null %s error=%v", field, err)
			}
		})
	}
	t.Run("empty operation strings", func(t *testing.T) {
		f := newCursorReaderFixture(t)
		op := cursorTestOperation(f, "op-one", "run-one", "", "", nil)
		delete(op, "status")
		f.writeSession(2, []map[string]any{op}, nil)
		page, err := f.reader.Read(f.ref, nil, 20)
		if err != nil || !f.reader.Available(f.ref) || len(page.Messages) != 0 {
			t.Fatalf("valid empty operation strings: page=%+v err=%v", page, err)
		}
	})
	t.Run("duplicate operation", func(t *testing.T) {
		f := newCursorReaderFixture(t)
		op := cursorTestOperation(f, "op-one", "run-one", "wire", "answer", nil)
		f.writeSession(2, []map[string]any{op, op}, nil)
		if f.reader.Available(f.ref) {
			t.Fatal("duplicate operation was available")
		}
	})
	t.Run("operation identity", func(t *testing.T) {
		f := newCursorReaderFixture(t)
		op := cursorTestOperation(f, "op-one", "run-one", "wire", "answer", nil)
		op["nativeSessionId"] = "another-native"
		f.writeSession(2, []map[string]any{op}, nil)
		if _, err := f.reader.Read(f.ref, nil, 20); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("identity mismatch=%v", err)
		}
	})
	t.Run("unsafe public input directory", func(t *testing.T) {
		f := newCursorReaderFixture(t)
		f.writeSession(2, []map[string]any{cursorTestOperation(f, "op-one", "run-one", "wire", "answer", nil)}, nil)
		directory := cursorMkdir(t, filepath.Join(f.manifest.OwnerDir, "public-inputs"), 0o700)
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if f.reader.Available(f.ref) {
			t.Fatal("unsafe public input directory was available")
		}
	})
	t.Run("invalid public input UTF-8", func(t *testing.T) {
		f := newCursorReaderFixture(t)
		op := cursorTestOperation(f, "op-one", "run-one", "wire", "answer", nil)
		f.writeSession(2, []map[string]any{op}, nil)
		path := f.writeInput(op, "service", "public", "")
		record := f.inputRecord("op-one", "run-one", "wire", "service", "public", "")
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		marker := []byte(`"userText":"public"`)
		index := bytes.Index(raw, marker)
		if index < 0 {
			t.Fatalf("userText marker missing from %s", raw)
		}
		raw[index+len(`"userText":"`)+2] = 0xff
		writeCursorRaw(t, path, raw)
		if f.reader.Available(f.ref) {
			t.Fatal("invalid UTF-8 input was advertised as available")
		}
		if _, err := f.reader.Read(f.ref, nil, 20); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("invalid UTF-8 History error=%v", err)
		}
		if _, err := f.reader.ReadTrace(f.ref, nil, 20); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("invalid UTF-8 trace error=%v", err)
		}
	})
	for _, test := range []struct {
		name   string
		mutate func(*cursorReaderFixture, string)
	}{
		{name: "wrong hash", mutate: func(f *cursorReaderFixture, path string) {
			raw := f.inputRecord("op-one", "run-one", "wire", "service", "public", "")
			raw["wireTextSha256"] = strings.Repeat("0", 64)
			writeCursorJSON(f.t, path, raw)
		}},
		{name: "extra field", mutate: func(f *cursorReaderFixture, path string) {
			raw := f.inputRecord("op-one", "run-one", "wire", "service", "public", "")
			raw["extra"] = "private"
			writeCursorJSON(f.t, path, raw)
		}},
		{name: "null user text", mutate: func(f *cursorReaderFixture, path string) {
			raw := f.inputRecord("op-one", "run-one", "wire", "service", "", "")
			raw["userText"] = nil
			writeCursorJSON(f.t, path, raw)
		}},
		{name: "unsafe mode", mutate: func(f *cursorReaderFixture, path string) { _ = os.Chmod(path, 0o644) }},
		{name: "symlink", mutate: func(f *cursorReaderFixture, path string) {
			outside := filepath.Join(f.root, "outside.json")
			_ = os.Rename(path, outside)
			_ = os.Symlink(outside, path)
		}},
		{name: "fifo", mutate: func(f *cursorReaderFixture, path string) {
			_ = os.Remove(path)
			_ = syscall.Mkfifo(path, 0o600)
		}},
		{name: "oversize", mutate: func(f *cursorReaderFixture, path string) {
			writeCursorRaw(f.t, path, []byte(strings.Repeat("x", cursorPublicInputMax+1)))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorReaderFixture(t)
			op := cursorTestOperation(f, "op-one", "run-one", "wire", "answer", nil)
			f.writeSession(2, []map[string]any{op}, nil)
			path := f.writeInput(op, "service", "public", "")
			test.mutate(&f, path)
			if f.reader.Available(f.ref) {
				t.Fatal("unsafe input was advertised as available")
			}
			if _, err := f.reader.Read(f.ref, nil, 20); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("unsafe input error=%v", err)
			}
		})
	}
}

// The ACP ref uses agent "cursor", the same agent name as Herdr's built-in
// cursor-agent binding. Dispatch keys on the source, so a registration path
// never reaches the cursor-agent transcript, title or activity readers, and a
// reader without explicit configuration refuses the ref.
func TestCursorACPRefStaysOnItsOwnAdapter(t *testing.T) {
	f := newCursorReaderFixture(t)
	op := cursorTestOperation(f, "op-one", "run-one", "wire", "answer", nil)
	f.writeSession(2, []map[string]any{op}, nil)
	f.writeInput(op, "service", "question", "")
	f.reader.CursorRoot = cursorMkdir(t, filepath.Join(f.root, "cursor-agent"), 0o700)

	if title := f.reader.ReadTitle(f.ref); title != (Title{}) {
		t.Fatalf("ACP ref reached a title reader: %+v", title)
	}
	if activity := f.reader.ReadActivity(f.ref); activity != (Activity{}) {
		t.Fatalf("ACP ref reached an activity reader: %+v", activity)
	}
	summary, err := f.reader.ReadTraceSummaryWith(f.ref, nil, 20, TraceOptions{Models: true, Markers: true, Labels: true, Times: true, Writes: true})
	if err != nil || len(summary.Items) != 2 || summary.Items[0].Type != "user" || summary.Items[0].Text != "question" ||
		summary.Items[1].Type != "assistant" || summary.Items[1].Text != "answer" || summary.Items[1].At != 0 || summary.Items[1].DetailRef != "" {
		t.Fatalf("summary with options=%+v err=%v", summary, err)
	}
	for _, other := range []Ref{
		{Source: "herdr:cursor", Agent: CursorACPAgent, Kind: "path", Value: f.ref.Value, ManifestDigest: f.ref.ManifestDigest},
		{Source: CursorACPSource, Agent: CursorACPAgent, Kind: "id", Value: "session_12345678"},
		{Source: CursorACPSource, Agent: "codex", Kind: "path", Value: f.ref.Value, ManifestDigest: f.ref.ManifestDigest},
	} {
		if f.reader.Supports(other) {
			t.Fatalf("non-ACP shape was routed to the ACP adapter: %+v", other)
		}
	}

	disabled := &Reader{CursorRoot: f.reader.CursorRoot}
	if disabled.Supports(f.ref) || disabled.Available(f.ref) {
		t.Fatal("ACP ref was supported without explicit configuration")
	}
	if _, err := disabled.Read(f.ref, nil, 20); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled History error=%v", err)
	}
	if _, err := disabled.ReadTrace(f.ref, nil, 20); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled trace error=%v", err)
	}
}

func TestCursorReaderBoundsEmptyRecordScanningAndClipsEncodedText(t *testing.T) {
	f := newCursorReaderFixture(t)
	ops := make([]map[string]any, 0, 2100)
	for index := range 2100 {
		ops = append(ops, cursorTestOperation(f, "op-"+strconv.Itoa(index), "run-"+strconv.Itoa(index), "wire", "", nil))
	}
	f.writeSession(2, ops, nil)
	page, err := f.reader.Read(f.ref, nil, 200)
	if err != nil || len(page.Messages) != 0 || page.NextCursor == nil || !page.Truncated {
		t.Fatalf("bounded empty History page=%+v err=%v", page, err)
	}
	finished, err := f.reader.Read(f.ref, page.NextCursor, 200)
	if err != nil || finished.NextCursor != nil {
		t.Fatalf("History scan cursor did not progress: %+v %v", finished, err)
	}
	trace, err := f.reader.ReadTrace(f.ref, nil, 200)
	if err != nil || len(trace.Items) != 0 || trace.NextCursor == nil || !trace.Truncated || !trace.SummaryTruncated {
		t.Fatalf("bounded empty trace=%+v err=%v", trace, err)
	}

	large := strings.Repeat("\\\n", 80_000)
	op := cursorTestOperation(f, "op-large", "run-large", "wire", large, nil)
	f.writeSession(3, []map[string]any{op}, nil)
	largeTrace, err := f.reader.ReadTrace(f.ref, nil, 20)
	if err != nil || len(largeTrace.Items) != 1 || !largeTrace.Truncated || !largeTrace.SummaryTruncated || eventSize(largeTrace.Items[0]) > maxTraceEventBytes {
		t.Fatalf("large trace was not encoded-byte clipped: %+v %v", largeTrace, err)
	}
	summary, err := f.reader.ReadTraceSummary(f.ref, nil, 20)
	if err != nil || !summary.Truncated {
		t.Fatalf("summary truncation=%+v %v", summary, err)
	}
}

type cursorReaderFixture struct {
	*cursorLocalFixture
	reader *Reader
	ref    Ref
}

func newCursorReaderFixture(t *testing.T) cursorReaderFixture {
	local := newCursorLocalFixture(t)
	registration, err := local.loaded.LoadRegistration(local.manifest.TerminalID)
	if err != nil || registration == nil {
		t.Fatal(err)
	}
	ref := Ref{Source: CursorACPSource, Agent: CursorACPAgent, Kind: "path", Value: registration.Path, ManifestDigest: registration.Digest}
	return cursorReaderFixture{cursorLocalFixture: &local, reader: &Reader{CursorACP: local.loaded}, ref: ref}
}

func cursorTestOperation(f cursorReaderFixture, operationID, runID, text, publicText string, extras map[string]any) map[string]any {
	op := map[string]any{"operationId": operationID, "runId": runID, "caseId": f.manifest.CaseID, "ownerId": f.manifest.OwnerID, "cwd": f.manifest.Cwd, "nativeSessionId": f.manifest.NativeSessionID, "text": text, "publicText": publicText, "status": "completed"}
	for key, value := range extras {
		op[key] = value
	}
	return op
}

func (f cursorReaderFixture) writeSession(revision int, operations []map[string]any, extras map[string]any) {
	session := map[string]any{"schemaVersion": 1, "ownerId": f.manifest.OwnerID, "cwd": f.manifest.Cwd, "nativeSessionId": f.manifest.NativeSessionID, "status": "ready", "operations": operations, "interactions": []any{}}
	for key, value := range extras {
		session[key] = value
	}
	writeCursorJSON(f.t, f.sessionPath, map[string]any{"schemaVersion": 1, "revision": revision, "savedAt": "2026-09-06T12:00:00Z", "session": session})
}

func (f cursorReaderFixture) inputRecord(operationID, runID, wire, origin, userText, terminalID string) map[string]any {
	digest := sha256.Sum256([]byte(wire))
	record := map[string]any{"schemaVersion": 1, "origin": origin, "ownerId": f.manifest.OwnerID, "caseId": f.manifest.CaseID, "cwd": f.manifest.Cwd, "nativeSessionId": f.manifest.NativeSessionID, "operationId": operationID, "runId": runID, "wireTextSha256": hex.EncodeToString(digest[:]), "userText": userText}
	if origin == "terminal" {
		record["terminalId"] = terminalID
	}
	return record
}

func (f cursorReaderFixture) writeInput(operation map[string]any, origin, userText, terminalID string) string {
	directory := filepath.Join(f.manifest.OwnerDir, "public-inputs")
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		cursorMkdir(f.t, directory, 0o700)
	}
	operationID, runID, wire := operation["operationId"].(string), operation["runId"].(string), operation["text"].(string)
	digest := sha256.Sum256([]byte(operationID))
	path := filepath.Join(directory, hex.EncodeToString(digest[:])+".json")
	writeCursorJSON(f.t, path, f.inputRecord(operationID, runID, wire, origin, userText, terminalID))
	return path
}

func reflectMessages(got, want []Message) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func messagesText(messages []Message) string {
	parts := make([]string, len(messages))
	for index, message := range messages {
		parts[index] = message.Text
	}
	return strings.Join(parts, "|")
}

func traceText(events []Event) string {
	parts := make([]string, len(events))
	for index, event := range events {
		parts[index] = event.Text
	}
	return strings.Join(parts, "|")
}

package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeCursorControlRecords(t *testing.T, f cursorLocalFixture, endpoint string, ppid any) {
	t.Helper()
	writeCursorJSON(t, filepath.Join(f.manifest.OwnerDir, "intent.json"), map[string]any{
		"kind": cursorOwnerKind, "ownerId": f.manifest.OwnerID, "caseId": f.manifest.CaseID, "cwd": f.manifest.Cwd,
		"nonce": "nonce-one", "credential": "private-credential", "createdAt": "2026-09-06T12:00:00Z", "extra": []any{"allowed"},
	})
	identity := f.manifest.OwnerIdentity
	writeCursorJSON(t, filepath.Join(f.manifest.OwnerDir, "owner.json"), map[string]any{
		"ownerId": f.manifest.OwnerID, "nonce": "nonce-one", "endpoint": endpoint, "identity": map[string]any{
			"pid": identity.PID, "ppid": ppid, "startTime": identity.StartTime, "bootId": identity.BootID,
			"executable": identity.Executable, "commandLine": identity.CommandLine, "futureIdentityField": true,
		}, "channel": map[string]any{"future": true},
	})
}

func TestCursorControlLoadsActualRecordsWithoutPublishingSecrets(t *testing.T) {
	f := newCursorLocalFixture(t)
	writeCursorControlRecords(t, f, "http://127.0.0.1:3210/control", "ignored-parent-extra")
	control, err := f.loaded.LoadControl(f.manifest)
	if err != nil || control.Endpoint() != "http://127.0.0.1:3210/control" || control.Credential() != "private-credential" {
		t.Fatalf("control=%#v err=%v", control, err)
	}
	encoded, err := json.Marshal(control)
	if err != nil || strings.Contains(string(encoded), "credential") || strings.Contains(string(encoded), "3210") || string(encoded) != "{}" {
		t.Fatalf("private control serialized: %s err=%v", encoded, err)
	}
	if err := f.loaded.VerifyControl(f.manifest, control); err != nil {
		t.Fatal(err)
	}
	writeCursorControlRecords(t, f, "http://127.0.0.1:3211/control", 999)
	if err := f.loaded.VerifyControl(f.manifest, control); err == nil {
		t.Fatal("changed private control records were accepted")
	}
}

func TestCursorControlRejectsEndpointAndPrivateFileAttacks(t *testing.T) {
	endpoints := []string{
		"http://127.0.0.1/control", "http://127.0.0.1:80/control", "http://127.0.0.2:3210/control",
		"http://localhost:3210/control", "https://127.0.0.1:3210/control", "http://user@127.0.0.1:3210/control",
		"http://127.0.0.1:3210/other", "http://127.0.0.1:3210/control?q=1", "http://127.0.0.1:3210/control#x",
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint, func(t *testing.T) {
			f := newCursorLocalFixture(t)
			writeCursorControlRecords(t, f, endpoint, 7)
			if _, err := f.loaded.LoadControl(f.manifest); err == nil {
				t.Fatal("unsafe endpoint accepted")
			}
		})
	}
	t.Run("mode", func(t *testing.T) {
		f := newCursorLocalFixture(t)
		writeCursorControlRecords(t, f, "http://127.0.0.1:3210/control", 7)
		if err := os.Chmod(filepath.Join(f.manifest.OwnerDir, "intent.json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := f.loaded.LoadControl(f.manifest); err == nil {
			t.Fatal("public intent accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		f := newCursorLocalFixture(t)
		writeCursorControlRecords(t, f, "http://127.0.0.1:3210/control", 7)
		owner := filepath.Join(f.manifest.OwnerDir, "owner.json")
		copy := filepath.Join(f.manifest.OwnerDir, "owner-copy.json")
		raw, _ := os.ReadFile(owner)
		writeCursorRaw(t, copy, raw)
		if err := os.Remove(owner); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(copy, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := f.loaded.LoadControl(f.manifest); err == nil {
			t.Fatal("symlink owner record accepted")
		}
	})
	t.Run("fifo", func(t *testing.T) {
		f := newCursorLocalFixture(t)
		writeCursorControlRecords(t, f, "http://127.0.0.1:3210/control", 7)
		owner := filepath.Join(f.manifest.OwnerDir, "owner.json")
		if err := os.Remove(owner); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(owner, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.loaded.LoadControl(f.manifest); err == nil {
			t.Fatal("FIFO owner record accepted")
		}
	})
}

func TestCursorPublicInputExclusivePublicationAndConflict(t *testing.T) {
	f := newCursorLocalFixture(t)
	input := CursorPublicInput{OperationID: "pairfob_owner_op_one", RunID: "pairfob_owner_run_one", Text: "/quit\n\x00literal"}
	if err := f.loaded.PublishPublicInput(f.manifest, input); err != nil {
		t.Fatal(err)
	}
	if err := f.loaded.PublishPublicInput(f.manifest, input); err != nil {
		t.Fatalf("identical replay failed: %v", err)
	}
	if err := f.loaded.PublishPublicInput(f.manifest, CursorPublicInput{OperationID: input.OperationID, RunID: "other", Text: input.Text}); err == nil {
		t.Fatal("conflicting immutable record was accepted")
	}
	directory := filepath.Join(f.manifest.OwnerDir, "public-inputs")
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("public input directory mode=%v err=%v", info.Mode(), err)
	}
	sum := sha256.Sum256([]byte(input.OperationID))
	path := filepath.Join(directory, hex.EncodeToString(sum[:])+".json")
	raw, err := readPrivateFile(path, cursorPublicInputMax, 0o600)
	if err != nil || !strings.Contains(string(raw), `"origin":"service"`) || strings.Contains(string(raw), "wireText") && !strings.Contains(string(raw), "wireTextSha256") {
		t.Fatalf("bad public input record: %s err=%v", raw, err)
	}
	other := CursorPublicInput{OperationID: "pairfob_owner_op_symlink", RunID: "run", Text: "text"}
	otherSum := sha256.Sum256([]byte(other.OperationID))
	unsafe := filepath.Join(directory, hex.EncodeToString(otherSum[:])+".json")
	if err := os.Symlink(path, unsafe); err != nil {
		t.Fatal(err)
	}
	if err := f.loaded.PublishPublicInput(f.manifest, other); err == nil {
		t.Fatal("unsafe existing publication was accepted")
	}
}

func TestCursorPublicInputSyncsVerifiedExistingTarget(t *testing.T) {
	f := newCursorLocalFixture(t)
	input := CursorPublicInput{OperationID: "pairfob_owner_op_existing", RunID: "pairfob_owner_run_existing", Text: "literal"}
	if err := f.loaded.PublishPublicInput(f.manifest, input); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(f.manifest.OwnerDir, "public-inputs")
	sum := sha256.Sum256([]byte(input.OperationID))
	target := filepath.Join(directory, hex.EncodeToString(sum[:])+".json")
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	writeCursorRaw(t, target, raw)
	originalSync := syncCursorRecord
	t.Cleanup(func() { syncCursorRecord = originalSync })
	var syncedTarget bool
	syncCursorRecord = func(file *os.File) error {
		if file.Name() == target {
			syncedTarget = true
		}
		return file.Sync()
	}
	if err := f.loaded.PublishPublicInput(f.manifest, input); err != nil {
		t.Fatal(err)
	}
	if !syncedTarget {
		t.Fatal("identical existing target was accepted without syncing its verified inode")
	}
}

func TestCursorPublicInputTargetSyncFailureFailsClosed(t *testing.T) {
	f := newCursorLocalFixture(t)
	input := CursorPublicInput{OperationID: "pairfob_owner_op_sync_failure", RunID: "pairfob_owner_run_sync_failure", Text: "literal"}
	if err := f.loaded.PublishPublicInput(f.manifest, input); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(f.manifest.OwnerDir, "public-inputs")
	sum := sha256.Sum256([]byte(input.OperationID))
	target := filepath.Join(directory, hex.EncodeToString(sum[:])+".json")
	originalSync := syncCursorRecord
	t.Cleanup(func() { syncCursorRecord = originalSync })
	syncCursorRecord = func(file *os.File) error {
		if file.Name() == target {
			return errors.New("injected target sync failure")
		}
		return file.Sync()
	}
	if err := f.loaded.PublishPublicInput(f.manifest, input); err == nil || !strings.Contains(err.Error(), "sync Cursor public input target") {
		t.Fatalf("target sync failure was accepted: %v", err)
	}
}

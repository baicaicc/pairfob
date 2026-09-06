package journal

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CursorPublicInput is the immutable source text paired with one owner
// operation. It intentionally contains no control credential or endpoint.
type CursorPublicInput struct {
	OperationID string
	RunID       string
	Text        string
}

type cursorPublicInputRecord struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Origin          string `json:"origin"`
	OwnerID         string `json:"ownerId"`
	CaseID          string `json:"caseId"`
	Cwd             string `json:"cwd"`
	NativeSessionID string `json:"nativeSessionId"`
	OperationID     string `json:"operationId"`
	RunID           string `json:"runId"`
	WireTextSHA256  string `json:"wireTextSha256"`
	UserText        string `json:"userText"`
}

var syncCursorRecord = func(file *os.File) error { return file.Sync() }

// PublishPublicInput durably publishes the service-origin input before an
// owner control request can be sent. Existing identical records are accepted.
func (c *CursorConfig) PublishPublicInput(manifest CursorManifest, input CursorPublicInput) error {
	if c == nil || c.validateManifest(manifest, manifest.TerminalID) != nil || !validBounded(input.OperationID, cursorIDMax) ||
		!validBounded(input.RunID, cursorIDMax) || !validJSString(input.Text, cursorJSStringMax, true) {
		return errors.New("invalid Cursor public input")
	}
	if err := validateDirectoryPath(manifest.Cwd, false); err != nil || validatePrivateDirectory(manifest.OwnerDir) != nil {
		return errors.New("Cursor public input binding changed")
	}
	directory := filepath.Join(manifest.OwnerDir, "public-inputs")
	if err := ensureCursorPublicDirectory(directory); err != nil {
		return err
	}
	wireSum := sha256.Sum256([]byte(input.Text))
	record := cursorPublicInputRecord{SchemaVersion: 1, Origin: "service", OwnerID: manifest.OwnerID, CaseID: manifest.CaseID,
		Cwd: manifest.Cwd, NativeSessionID: manifest.NativeSessionID, OperationID: input.OperationID, RunID: input.RunID,
		WireTextSHA256: hex.EncodeToString(wireSum[:]), UserText: input.Text}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > cursorPublicInputMax {
		return errors.New("Cursor public input exceeds durable bound")
	}
	nameSum := sha256.Sum256([]byte(input.OperationID))
	target := filepath.Join(directory, hex.EncodeToString(nameSum[:])+".json")
	if err := publishCursorRecord(directory, target, raw); err != nil {
		return err
	}
	if err := verifyAndSyncCursorPublicRecord(target, record); err != nil {
		return err
	}
	return syncCursorDirectory(directory)
}

func ensureCursorPublicDirectory(path string) error {
	err := os.Mkdir(path, 0o700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := validatePrivateDirectory(path); err != nil {
		return errors.New("Cursor public input directory is unsafe")
	}
	return syncCursorDirectory(filepath.Dir(path))
}

func publishCursorRecord(directory, target string, raw []byte) error {
	var nonce [16]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return err
	}
	temporary := filepath.Join(directory, ".pairfob-input-"+hex.EncodeToString(nonce[:])+".tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if written, err := file.Write(raw); err != nil || written != len(raw) {
		return errors.New("Cursor public input could not be written")
	}
	if err := syncCursorRecord(file); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, target); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := syncCursorDirectory(directory); err != nil {
		return err
	}
	return nil
}

func verifyAndSyncCursorPublicRecord(path string, expected cursorPublicInputRecord) error {
	if err := canonicalAbsolute(path); err != nil {
		return errors.New("Cursor public input target is unsafe")
	}
	file, err := openCursorNoFollow(path)
	if err != nil {
		return errors.New("Cursor public input target is unavailable")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > cursorPublicInputMax || before.Mode().Perm() != 0o600 {
		return errors.New("Cursor public input target has an invalid type, size, or mode")
	}
	raw, err := io.ReadAll(io.LimitReader(file, cursorPublicInputMax+1))
	if err != nil || int64(len(raw)) != before.Size() || !sameCursorPublicRecord(raw, expected) {
		return errors.New("Cursor public input publication conflict")
	}
	if err := verifyCursorFileIdentity(path, before, file); err != nil {
		return err
	}
	if err := syncCursorRecord(file); err != nil {
		return fmt.Errorf("sync Cursor public input target: %w", err)
	}
	return verifyCursorFileIdentity(path, before, file)
}

func verifyCursorFileIdentity(path string, before os.FileInfo, file *os.File) error {
	after, err := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || canonicalAbsolute(path) != nil || !cursorFileStable(before, after) || !os.SameFile(after, pathInfo) {
		return errors.New("Cursor public input target changed while being verified")
	}
	return nil
}

func sameCursorPublicRecord(raw []byte, expected cursorPublicInputRecord) bool {
	var actual cursorPublicInputRecord
	if decodeExactObject(raw, &actual, "schemaVersion", "origin", "ownerId", "caseId", "cwd", "nativeSessionId", "operationId", "runId", "wireTextSha256", "userText") != nil {
		return false
	}
	a, errA := json.Marshal(actual)
	b, errB := json.Marshal(expected)
	return errA == nil && errB == nil && len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

func syncCursorDirectory(path string) error {
	if err := validatePrivateDirectory(path); err != nil {
		return errors.New("Cursor directory is no longer private and canonical")
	}
	directory, err := openCursorNoFollow(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	before, err := directory.Stat()
	if err != nil || !before.IsDir() || before.Mode().Perm() != 0o700 {
		return errors.New("Cursor directory has an invalid type or mode")
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync Cursor directory: %w", err)
	}
	after, err := directory.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || canonicalAbsolute(path) != nil || !cursorFileStable(before, after) || !os.SameFile(after, pathInfo) {
		return errors.New("Cursor directory changed while being synced")
	}
	return nil
}

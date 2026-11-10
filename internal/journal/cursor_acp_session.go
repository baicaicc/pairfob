package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const cursorJSStringMax = 64 << 10

type cursorOperation struct {
	OperationID     string
	RunID           string
	CaseID          string
	OwnerID         string
	Cwd             string
	NativeSessionID string
	Text            string
	PublicText      string
}

type cursorSessionSnapshot struct {
	CursorSession
	Operations []cursorOperation
}

func parseCursorSession(raw []byte, manifest CursorManifest, operations bool) (cursorSessionSnapshot, error) {
	var result cursorSessionSnapshot
	if !utf8.Valid(raw) {
		return result, errors.New("invalid Cursor session UTF-8")
	}
	var envelope map[string]json.RawMessage
	if err := decodeJSONObject(raw, &envelope); err != nil {
		return result, errors.New("invalid Cursor session envelope")
	}
	var schema int
	var revision int64
	var savedAt string
	var sessionRaw json.RawMessage
	if requiredJSON(envelope, "schemaVersion", &schema) != nil || schema != 1 ||
		requiredJSON(envelope, "revision", &revision) != nil || revision <= 0 || revision > 1<<53-1 ||
		requiredJSONString(envelope, "savedAt", &savedAt) != nil || requiredJSON(envelope, "session", &sessionRaw) != nil {
		return result, errors.New("invalid Cursor session envelope")
	}
	saved, err := time.Parse(time.RFC3339, savedAt)
	if err != nil {
		return result, errors.New("invalid Cursor session savedAt")
	}
	var session map[string]json.RawMessage
	if err := decodeJSONObject(sessionRaw, &session); err != nil {
		return result, errors.New("invalid Cursor session body")
	}
	result.Revision, result.SavedAt = revision, saved
	if requiredJSON(session, "schemaVersion", &result.SchemaVersion) != nil || result.SchemaVersion != 1 {
		return result, errors.New("invalid Cursor session schemaVersion")
	}
	for key, target := range map[string]*string{"ownerId": &result.OwnerID, "cwd": &result.Cwd, "nativeSessionId": &result.NativeSessionID} {
		if requiredJSONString(session, key, target) != nil || !validJSString(*target, cursorJSStringMax, false) {
			return result, fmt.Errorf("invalid Cursor session %s", key)
		}
	}
	if result.OwnerID != manifest.OwnerID || result.Cwd != manifest.Cwd || result.NativeSessionID != manifest.NativeSessionID {
		return result, errors.New("Cursor session does not match manifest")
	}
	if rawStatus, ok := session["status"]; ok {
		var status string
		if json.Unmarshal(rawStatus, &status) == nil && validJSString(status, cursorJSStringMax, false) {
			result.Status = status
		}
	}
	if !operations {
		return result, nil
	}
	var rawOperations []json.RawMessage
	if requiredJSON(session, "operations", &rawOperations) != nil || rawOperations == nil {
		return result, errors.New("invalid Cursor session operations")
	}
	result.Operations = make([]cursorOperation, 0, len(rawOperations))
	seen := make(map[string]struct{}, len(rawOperations))
	for _, rawOperation := range rawOperations {
		op, err := parseCursorOperation(rawOperation, manifest)
		if err != nil {
			return cursorSessionSnapshot{}, err
		}
		if _, duplicate := seen[op.OperationID]; duplicate {
			return cursorSessionSnapshot{}, errors.New("duplicate Cursor operation ID")
		}
		seen[op.OperationID] = struct{}{}
		result.Operations = append(result.Operations, op)
	}
	return result, nil
}

func parseCursorOperation(raw []byte, manifest CursorManifest) (cursorOperation, error) {
	var object map[string]json.RawMessage
	if err := decodeJSONObject(raw, &object); err != nil {
		return cursorOperation{}, errors.New("invalid Cursor operation")
	}
	var op cursorOperation
	fields := map[string]*string{
		"operationId": &op.OperationID, "runId": &op.RunID, "caseId": &op.CaseID,
		"ownerId": &op.OwnerID, "cwd": &op.Cwd, "nativeSessionId": &op.NativeSessionID,
		"text": &op.Text, "publicText": &op.PublicText,
	}
	for key, target := range fields {
		if requiredJSONString(object, key, target) != nil {
			return cursorOperation{}, fmt.Errorf("invalid Cursor operation %s", key)
		}
		valid := validJSString(*target, cursorJSStringMax, key == "text")
		if key == "publicText" {
			valid = utf8.ValidString(*target)
		}
		if !valid {
			return cursorOperation{}, fmt.Errorf("invalid Cursor operation %s", key)
		}
	}
	if op.OwnerID != manifest.OwnerID || op.CaseID != manifest.CaseID || op.Cwd != manifest.Cwd || op.NativeSessionID != manifest.NativeSessionID {
		return cursorOperation{}, errors.New("Cursor operation identity does not match manifest")
	}
	return op, nil
}

func requiredJSONString(object map[string]json.RawMessage, key string, target *string) error {
	raw, ok := object[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("missing or null string field")
	}
	return json.Unmarshal(raw, target)
}

func decodeJSONObject(raw []byte, target *map[string]json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil || *target == nil {
		return errors.New("invalid JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validJSString(value string, max int, empty bool) bool {
	if !utf8.ValidString(value) || (!empty && value == "") {
		return false
	}
	return len(utf16.Encode([]rune(value))) <= max
}

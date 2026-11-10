package journal

// Projects a verified allInOne Cursor ACP session as History / Trace pages:
// public input records become user messages, operation publicText becomes
// assistant text. Page tokens bind the manifest digest and stable operation
// anchors, so a rewritten session.json cannot splice pages together.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
)

const (
	cursorPublicInputMax = 512 << 10
	cursorSlotScanMax    = 4096
	cursorInputReadMax   = 512
)

type cursorPublicInput struct {
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
	TerminalID      string `json:"terminalId,omitempty"`
}

type cursorPageToken struct {
	Version  int    `json:"v"`
	Mode     string `json:"m"`
	Binding  string `json:"b"`
	Anchor   string `json:"a"`
	Prefix   string `json:"p"`
	Count    int    `json:"n"`
	Snapshot string `json:"s"`
}

type cursorProjection struct {
	snapshot       cursorSessionSnapshot
	manifest       CursorManifest
	registration   CursorRegistration
	binding        CursorBinding
	config         *CursorConfig
	publicInputDir string
	inputsPresent  bool
}

func (r *Reader) supportsCursorACP(ref Ref) bool {
	if r.CursorACP == nil || ref.Source != CursorACPSource || ref.Agent != CursorACPAgent || ref.Kind != "path" {
		return false
	}
	if len(ref.ManifestDigest) != sha256.Size*2 || filepath.Dir(ref.Value) != r.CursorACP.RegistryRoot || filepath.Clean(ref.Value) != ref.Value || !filepath.IsAbs(ref.Value) {
		return false
	}
	decoded, err := hex.DecodeString(ref.ManifestDigest)
	return err == nil && hex.EncodeToString(decoded) == ref.ManifestDigest
}

func (r *Reader) cursorACPAvailable(ref Ref) bool {
	projection, err := r.loadCursorProjection(ref)
	if err != nil {
		return false
	}
	if !projection.inputsPresent {
		return projection.verify(ref) == nil
	}
	reads := 0
	for _, operation := range projection.snapshot.Operations {
		if reads >= cursorInputReadMax {
			break
		}
		reads++
		if _, _, err := projection.publicInput(operation); err != nil {
			return false
		}
	}
	return projection.verify(ref) == nil
}

func (r *Reader) loadCursorProjection(ref Ref) (cursorProjection, error) {
	registration, err := r.CursorACP.LoadRegistrationRef(ref)
	if err != nil || registration == nil {
		return cursorProjection{}, ErrUnavailable
	}
	manifest := registration.Manifest
	bindingBefore, err := r.CursorACP.LoadBinding(manifest)
	if err != nil {
		return cursorProjection{}, ErrUnavailable
	}
	raw, err := readPrivateFile(filepath.Join(manifest.OwnerDir, "session.json"), cursorSessionRecordMax, 0o600)
	if err != nil {
		return cursorProjection{}, ErrUnavailable
	}
	snapshot, err := parseCursorSession(raw, manifest, true)
	if err != nil {
		return cursorProjection{}, ErrUnavailable
	}
	directory := filepath.Join(manifest.OwnerDir, "public-inputs")
	present, err := cursorPublicDirectory(directory)
	if err != nil {
		return cursorProjection{}, ErrUnavailable
	}
	return cursorProjection{snapshot: snapshot, manifest: manifest, registration: *registration, binding: bindingBefore, config: r.CursorACP, publicInputDir: directory, inputsPresent: present}, nil
}

func (p cursorProjection) verify(ref Ref) error {
	registration, err := p.config.LoadRegistrationRef(ref)
	if err != nil || registration == nil || registration.Path != p.registration.Path || registration.Digest != p.registration.Digest {
		return ErrUnavailable
	}
	binding, err := p.config.LoadBinding(p.manifest)
	if err != nil || !reflect.DeepEqual(binding, p.binding) {
		return ErrUnavailable
	}
	session, err := p.config.LoadSession(p.manifest)
	if err != nil || session.Revision < p.snapshot.Revision || session.OwnerID != p.snapshot.OwnerID || session.Cwd != p.snapshot.Cwd || session.NativeSessionID != p.snapshot.NativeSessionID {
		return ErrUnavailable
	}
	return nil
}

func cursorPublicDirectory(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := validatePrivateDirectory(path); err != nil {
		return false, err
	}
	return true, nil
}

func (p cursorProjection) publicInput(op cursorOperation) (string, bool, error) {
	if !p.inputsPresent {
		return "", false, nil
	}
	operationHash := sha256.Sum256([]byte(op.OperationID))
	path := filepath.Join(p.publicInputDir, hex.EncodeToString(operationHash[:])+".json")
	raw, err := readPrivateFile(path, cursorPublicInputMax, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var probe map[string]json.RawMessage
	if err := decodeJSONObject(raw, &probe); err != nil {
		return "", false, err
	}
	var origin string
	if requiredJSONString(probe, "origin", &origin) != nil || (origin != "service" && origin != "terminal") {
		return "", false, errors.New("invalid Cursor public input origin")
	}
	keys := []string{"schemaVersion", "origin", "ownerId", "caseId", "cwd", "nativeSessionId", "operationId", "runId", "wireTextSha256", "userText"}
	if origin == "terminal" {
		keys = append(keys, "terminalId")
	}
	var record cursorPublicInput
	if err := decodeExactObject(raw, &record, keys...); err != nil || record.SchemaVersion != 1 || record.Origin != origin {
		return "", false, errors.New("invalid Cursor public input record")
	}
	stringFields := map[string]*string{
		"origin": &record.Origin, "ownerId": &record.OwnerID, "caseId": &record.CaseID,
		"cwd": &record.Cwd, "nativeSessionId": &record.NativeSessionID,
		"operationId": &record.OperationID, "runId": &record.RunID,
		"wireTextSha256": &record.WireTextSHA256, "userText": &record.UserText,
	}
	if origin == "terminal" {
		stringFields["terminalId"] = &record.TerminalID
	}
	for key, target := range stringFields {
		if requiredJSONString(probe, key, target) != nil {
			return "", false, errors.New("invalid Cursor public input string field")
		}
	}
	if record.OwnerID != op.OwnerID || record.CaseID != op.CaseID || record.Cwd != op.Cwd || record.NativeSessionID != op.NativeSessionID || record.OperationID != op.OperationID || record.RunID != op.RunID {
		return "", false, errors.New("Cursor public input identity mismatch")
	}
	if !validJSString(record.UserText, cursorJSStringMax, true) || (origin == "terminal" && !validSegment(record.TerminalID)) {
		return "", false, errors.New("invalid Cursor public input fields")
	}
	wireDigest := sha256.Sum256([]byte(op.Text))
	wanted := hex.EncodeToString(wireDigest[:])
	decoded, decodeErr := hex.DecodeString(record.WireTextSHA256)
	if decodeErr != nil || hex.EncodeToString(decoded) != record.WireTextSHA256 || subtle.ConstantTimeCompare([]byte(record.WireTextSHA256), []byte(wanted)) != 1 {
		return "", false, errors.New("Cursor public input wire hash mismatch")
	}
	return record.UserText, true, nil
}

func cursorSlotAnchor(op cursorOperation, position byte) string {
	request := sha256.Sum256([]byte(op.Text))
	hash := sha256.New()
	for _, value := range []string{op.OperationID, op.RunID, op.CaseID, op.OwnerID, op.Cwd, op.NativeSessionID} {
		field := sha256.Sum256([]byte(value))
		hash.Write(field[:])
	}
	hash.Write(request[:])
	hash.Write([]byte{0, position})
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func cursorBindingFingerprint(ref Ref) string {
	hash := sha256.Sum256([]byte(ref.Source + "\x00" + ref.Agent + "\x00" + ref.Kind + "\x00" + ref.Value + "\x00" + ref.ManifestDigest))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func cursorModeFingerprint(mode string) string {
	hash := sha256.Sum256([]byte("cursor-page:v1\x00" + mode))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func cursorPrefix(operations []cursorOperation, slot int) string {
	hash := sha256.New()
	for index := 0; index <= slot; index++ {
		op := operations[index/2]
		hash.Write([]byte(cursorSlotAnchor(op, byte(index%2))))
		hash.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func encodeCursorPageToken(ref Ref, operations []cursorOperation, mode string, slot int) string {
	count := len(operations) * 2
	value := cursorPageToken{Version: 1, Mode: cursorModeFingerprint(mode), Binding: cursorBindingFingerprint(ref), Anchor: cursorSlotAnchor(operations[slot/2], byte(slot%2)), Prefix: cursorPrefix(operations, slot), Count: count, Snapshot: cursorPrefix(operations, count-1)}
	raw, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursorPageToken(ref Ref, operations []cursorOperation, mode string, cursor *string) (int, error) {
	if cursor == nil || *cursor == "" {
		if mode == "h" {
			return 0, nil
		}
		return len(operations)*2 - 1, nil
	}
	if len(*cursor) > 1024 {
		return 0, ErrCursorInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil {
		return 0, ErrCursorInvalid
	}
	var token cursorPageToken
	if decodeExactObject(raw, &token, "v", "m", "b", "a", "p", "n", "s") != nil || token.Version != 1 || token.Anchor == "" || token.Prefix == "" || token.Count < 2 || token.Count%2 != 0 || token.Snapshot == "" {
		return 0, ErrCursorInvalid
	}
	if token.Mode != cursorModeFingerprint(mode) || token.Binding != cursorBindingFingerprint(ref) {
		return 0, ErrCursorConflict
	}
	if len(operations)*2 < token.Count || cursorPrefix(operations, token.Count-1) != token.Snapshot {
		return 0, ErrCursorConflict
	}
	for slot := range len(operations) * 2 {
		if cursorSlotAnchor(operations[slot/2], byte(slot%2)) != token.Anchor {
			continue
		}
		if cursorPrefix(operations, slot) != token.Prefix {
			return 0, ErrCursorConflict
		}
		if mode == "h" {
			return slot + 1, nil
		}
		return slot - 1, nil
	}
	return 0, ErrCursorConflict
}

func (r *Reader) readCursorACPHistory(ref Ref, cursor *string, limit int) (Page, error) {
	projection, err := r.loadCursorProjection(ref)
	if err != nil {
		return Page{}, ErrUnavailable
	}
	start, err := decodeCursorPageToken(ref, projection.snapshot.Operations, "h", cursor)
	if err != nil {
		return Page{}, err
	}
	page := Page{Messages: make([]Message, 0, limit)}
	next, limited, err := projection.readForward(start, limit, func(message Message) bool {
		message, page.Truncated = clipMessage(message, page.Truncated)
		page.Messages = append(page.Messages, message)
		return true
	})
	if err != nil {
		return Page{}, ErrUnavailable
	}
	if err := projection.verify(ref); err != nil {
		return Page{}, ErrUnavailable
	}
	page.Truncated = page.Truncated || limited
	if next >= 0 {
		value := encodeCursorPageToken(ref, projection.snapshot.Operations, "h", next)
		page.NextCursor = &value
	}
	return page, nil
}

func (p cursorProjection) readForward(start, limit int, add func(Message) bool) (int, bool, error) {
	slots, scans, inputReads, last := len(p.snapshot.Operations)*2, 0, 0, -1
	items, bytes := 0, 0
	for slot := start; slot < slots; slot++ {
		if scans >= cursorSlotScanMax || (slot%2 == 0 && p.inputsPresent && inputReads >= cursorInputReadMax) {
			return last, true, nil
		}
		if items >= limit {
			return last, false, nil
		}
		op := p.snapshot.Operations[slot/2]
		message, present, err := p.slotMessage(op, slot%2, &inputReads)
		if err != nil {
			return -1, false, err
		}
		scans++
		if present {
			clipped, _ := clipMessage(message, false)
			size := messageSize(clipped)
			if items > 0 && bytes+size > maxPageItemsBytes {
				return last, false, nil
			}
			add(message)
			items++
			bytes += size
		}
		last = slot
	}
	return -1, false, nil
}

func (p cursorProjection) slotMessage(op cursorOperation, position int, inputReads *int) (Message, bool, error) {
	if position == 1 {
		return Message{Role: "assistant", Text: op.PublicText}, op.PublicText != "", nil
	}
	if p.inputsPresent {
		(*inputReads)++
	}
	text, present, err := p.publicInput(op)
	return Message{Role: "user", Text: text}, present && text != "", err
}

func cursorTraceEvent(message Message) Event {
	return Event{Type: message.Role, Text: message.Text}
}

func (r *Reader) readCursorACPTrace(ref Ref, cursor *string, limit int) (TracePage, error) {
	projection, err := r.loadCursorProjection(ref)
	if err != nil {
		return TracePage{}, ErrUnavailable
	}
	start, err := decodeCursorPageToken(ref, projection.snapshot.Operations, "t", cursor)
	if err != nil {
		return TracePage{}, err
	}
	page := TracePage{Items: make([]Event, 0, limit)}
	scans, inputReads, pageBytes, last := 0, 0, 0, -1
	stopped, scanLimited := false, false
	for slot := start; slot >= 0; slot-- {
		if scans >= cursorSlotScanMax || (slot%2 == 0 && projection.inputsPresent && inputReads >= cursorInputReadMax) {
			stopped, scanLimited = true, true
			break
		}
		if len(page.Items) >= limit {
			stopped = true
			break
		}
		op := projection.snapshot.Operations[slot/2]
		message, present, readErr := projection.slotMessage(op, slot%2, &inputReads)
		if readErr != nil {
			return TracePage{}, ErrUnavailable
		}
		scans++
		if present {
			event, clipped := clipEvent(cursorTraceEvent(message), false)
			size := eventSize(event)
			if len(page.Items) > 0 && pageBytes+size > maxTraceItemsBytes {
				stopped = true
				break
			}
			event.SummaryTruncated = clipped
			page.Truncated = page.Truncated || clipped
			page.SummaryTruncated = page.SummaryTruncated || clipped
			page.Items = append(page.Items, event)
			pageBytes += size
		}
		last = slot
	}
	for left, right := 0, len(page.Items)-1; left < right; left, right = left+1, right-1 {
		page.Items[left], page.Items[right] = page.Items[right], page.Items[left]
	}
	if err := projection.verify(ref); err != nil {
		return TracePage{}, ErrUnavailable
	}
	page.Truncated = page.Truncated || scanLimited
	page.SummaryTruncated = page.SummaryTruncated || scanLimited
	if stopped && last >= 0 {
		value := encodeCursorPageToken(ref, projection.snapshot.Operations, "t", last)
		page.NextCursor = &value
	}
	return page, nil
}

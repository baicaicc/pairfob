package daemon

// Sends a phone prompt to a verified allInOne Cursor ACP owner over its
// private loopback control endpoint instead of typing into the pane. The
// public input record is durable before the request, every binding is fenced
// before and after the network call, and an unprovable outcome is unknown.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"pairfob/internal/journal"
	"pairfob/internal/runtime"
)

const (
	cursorControlRequestMax  = 256 << 10
	cursorControlResponseMax = 16 << 20
	cursorJSSafeInteger      = int64(1<<53 - 1)
)

type cursorPromptBinding struct {
	pane         runtime.Pane
	registration journal.CursorRegistration
	binding      journal.CursorBinding
	claim        journal.CursorClaim
	session      journal.CursorSession
	control      *journal.CursorControl
}

// rpcPromptAgentWithCursor replaces the lookup-then-execute PromptAgent path
// while Cursor ACP is configured. Whether a pane routes to an owner depends on
// live verification, so the operation ledger answers a replay or a conflicting
// reuse before any snapshot, process or owner read happens.
func (e *Engine) rpcPromptAgentWithCursor(s *sess, id string, sessionName *string, explicitOperationID string, command runtime.PromptAgentCommand) {
	operationID, ok := mutationOperationID(explicitOperationID, id, true)
	if !ok {
		e.replyErr(s, id, "invalid_argument", "invalid operation_id")
		return
	}
	// Keep this deadline in lockstep with executeRPC and pwa MUTATION_RPC_TIMEOUT_MS.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	session := runtimeSession(sessionName)
	receipt, err := e.executeTrackedMutation(ctx, s.deviceID, session, operationID, command, func() (runtime.Receipt, error) {
		return e.executePromptAgent(ctx, s.deviceID, operationID, session, command)
	})
	if err != nil {
		e.replyRuntimeErr(s, id, err, "agent_not_found")
		return
	}
	e.audit("prompt_agent", map[string]any{"device_id": s.deviceID, "pane_id": command.Target, "bytes": len([]byte(command.Text)), "outcome": receipt.Outcome})
	e.reply(s, id, map[string]any{"pane_id": command.Target, "agent_status": "unknown", "operation_id": operationID, "outcome": receipt.Outcome})
}

func (e *Engine) executePromptAgent(ctx context.Context, deviceID, phoneOperationID string, session runtime.SessionRef, command runtime.PromptAgentCommand) (runtime.Receipt, error) {
	snapshot, err := e.rawCursorSnapshot(ctx, session)
	if err != nil {
		return promptNotApplied(runtime.CodeOffline, "agent snapshot is unavailable", err)
	}
	pane, err := exactCursorPane(snapshot, command.Target)
	if err != nil {
		return promptNotApplied(runtime.CodeNotFound, "agent pane is no longer available", err)
	}
	// A session ref supplied by Herdr is built-in authority. Local Cursor
	// registration is considered only for otherwise-unbound raw terminal panes.
	if pane.AgentSession != nil {
		return e.executeBuiltinPrompt(ctx, session, phoneOperationID, pane, command)
	}
	config := e.CursorConfig
	if config == nil || pane.TerminalID == "" || session.Name != config.RuntimeSession {
		return e.executeBuiltinPrompt(ctx, session, phoneOperationID, pane, command)
	}
	registration, err := config.LoadRegistration(pane.TerminalID)
	if err != nil {
		return promptNotApplied(runtime.CodeConflict, "registered Cursor owner is invalid", err)
	}
	if registration == nil {
		return e.executeBuiltinPrompt(ctx, session, phoneOperationID, pane, command)
	}
	bound, err := e.loadCursorPromptBinding(ctx, session, pane, *registration)
	if err != nil {
		return promptNotApplied(runtime.CodeConflict, "registered Cursor owner could not be verified", err)
	}
	nativeOperationID, nativeRunID := cursorPromptIDs(deviceID, phoneOperationID, session, bound.registration, bound.session)
	if err := e.verifyCursorPromptBinding(ctx, session, bound); err != nil {
		return promptNotApplied(runtime.CodeConflict, "Cursor owner binding changed", err)
	}
	if err := config.PublishPublicInput(bound.registration.Manifest, journal.CursorPublicInput{OperationID: nativeOperationID, RunID: nativeRunID, Text: command.Text}); err != nil {
		return promptNotApplied(runtime.CodeConflict, "Cursor public input could not be published", err)
	}
	if err := e.verifyCursorPromptBinding(ctx, session, bound); err != nil {
		return promptNotApplied(runtime.CodeConflict, "Cursor owner binding changed before admission", err)
	}
	response, err := e.sendCursorPrompt(ctx, bound, nativeOperationID, nativeRunID, command.Text)
	if err != nil {
		if fault, ok := runtime.AsFault(err); ok {
			return runtime.Receipt{Outcome: fault.Outcome}, err
		}
		return promptUnknown("Cursor owner admission outcome is unknown", err)
	}
	accepted, refusalCode, err := validateCursorPromptResponse(response, bound, nativeOperationID, nativeRunID, command.Text)
	if err != nil {
		return promptUnknown("Cursor owner returned an invalid admission response", err)
	}
	if err := e.verifyCursorPromptBinding(ctx, session, bound); err != nil {
		return promptUnknown("Cursor owner binding changed after admission", err)
	}
	if !accepted {
		return promptNotApplied(refusalCode, "Cursor owner refused the prompt", nil)
	}
	return runtime.Receipt{Outcome: runtime.OutcomeApplied, Updated: []runtime.EntityRef{{Kind: runtime.EntityAgent, ID: command.Target}}}, nil
}

func (e *Engine) executeBuiltinPrompt(ctx context.Context, session runtime.SessionRef, operationID string, pane runtime.Pane, command runtime.PromptAgentCommand) (runtime.Receipt, error) {
	if pane.Agent == "" {
		return promptNotApplied(runtime.CodeNotFound, "pane does not currently host an agent", nil)
	}
	return e.RT.Execute(ctx, session, operationID, command)
}

func (e *Engine) loadCursorPromptBinding(ctx context.Context, session runtime.SessionRef, pane runtime.Pane, registration journal.CursorRegistration) (cursorPromptBinding, error) {
	ref, err := e.resolveCursorRef(ctx, session, pane)
	if err != nil || ref.Value != registration.Path || ref.ManifestDigest != registration.Digest {
		return cursorPromptBinding{}, errors.New("Cursor resolver did not preserve registration")
	}
	manifest := registration.Manifest
	binding, err := e.CursorConfig.LoadBinding(manifest)
	if err != nil {
		return cursorPromptBinding{}, err
	}
	claim, err := e.CursorConfig.LoadClaim(manifest)
	if err != nil {
		return cursorPromptBinding{}, err
	}
	native, err := e.CursorConfig.LoadSession(manifest)
	if err != nil {
		return cursorPromptBinding{}, err
	}
	control, err := e.CursorConfig.LoadControl(manifest)
	if err != nil {
		return cursorPromptBinding{}, err
	}
	return cursorPromptBinding{pane: pane, registration: registration, binding: binding, claim: claim, session: native, control: control}, nil
}

func (e *Engine) verifyCursorPromptBinding(ctx context.Context, session runtime.SessionRef, expected cursorPromptBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manifest := expected.registration.Manifest
	readIdentity := e.cursorIdentity
	if readIdentity == nil {
		readIdentity = nativeCursorProcessIdentity
	}
	identity, err := readIdentity(ctx, manifest.OwnerIdentity.PID)
	if err != nil || identity == nil || !journal.SameCursorIdentity(*identity, manifest.OwnerIdentity) {
		return errors.New("Cursor owner identity changed")
	}
	ref, err := e.resolveCursorRef(ctx, session, expected.pane)
	if err != nil || ref.Value != expected.registration.Path || ref.ManifestDigest != expected.registration.Digest {
		return errors.New("Cursor registration or pane changed")
	}
	registration, err := e.CursorConfig.LoadRegistration(manifest.TerminalID)
	if err != nil || registration == nil || registration.Path != expected.registration.Path || registration.Digest != expected.registration.Digest {
		return errors.New("Cursor registration changed")
	}
	binding, err := e.CursorConfig.LoadBinding(manifest)
	if err != nil || !reflect.DeepEqual(binding, expected.binding) {
		return errors.New("Cursor binding changed")
	}
	claim, err := e.CursorConfig.LoadClaim(manifest)
	if err != nil || !reflect.DeepEqual(claim, expected.claim) {
		return errors.New("Cursor claim changed")
	}
	native, err := e.CursorConfig.LoadSession(manifest)
	if err != nil || native.Revision < expected.session.Revision || native.OwnerID != expected.session.OwnerID || native.Cwd != expected.session.Cwd || native.NativeSessionID != expected.session.NativeSessionID {
		return errors.New("Cursor native session changed")
	}
	if err := e.CursorConfig.VerifyControl(manifest, expected.control); err != nil {
		return err
	}
	identity, err = readIdentity(ctx, manifest.OwnerIdentity.PID)
	if err != nil || identity == nil || !journal.SameCursorIdentity(*identity, manifest.OwnerIdentity) {
		return errors.New("Cursor owner identity changed at binding fence")
	}
	return ctx.Err()
}

func cursorPromptIDs(deviceID, operationID string, session runtime.SessionRef, registration journal.CursorRegistration, native journal.CursorSession) (string, string) {
	fields := []string{"pairfob-cursor-prompt-v1", deviceID, operationID, session.Name, registration.Manifest.OwnerID,
		registration.Manifest.CaseID, registration.Manifest.Cwd, registration.Manifest.NativeSessionID, registration.Digest,
		native.OwnerID, native.Cwd, native.NativeSessionID}
	digest := func(kind string) string {
		hash := sha256.New()
		for _, value := range append([]string{kind}, fields...) {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			hash.Write(size[:])
			hash.Write([]byte(value))
		}
		return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
	}
	return "pairfob_owner_op_" + digest("operation"), "pairfob_owner_run_" + digest("run")
}

type cursorControlEnvelope struct {
	OwnerID  string
	CaseID   string
	Cwd      string
	Identity journal.CursorProcessIdentity
	Object   map[string]json.RawMessage
}

func (e *Engine) sendCursorPrompt(ctx context.Context, bound cursorPromptBinding, operationID, runID, text string) (cursorControlEnvelope, error) {
	body, err := json.Marshal(struct {
		OwnerID string `json:"ownerId"`
		CaseID  string `json:"caseId"`
		Cwd     string `json:"cwd"`
		Method  string `json:"method"`
		Params  struct {
			OperationID string `json:"operationId"`
			RunID       string `json:"runId"`
			Text        string `json:"text"`
		} `json:"params"`
	}{OwnerID: bound.registration.Manifest.OwnerID, CaseID: bound.registration.Manifest.CaseID, Cwd: bound.registration.Manifest.Cwd, Method: "prompt",
		Params: struct {
			OperationID string `json:"operationId"`
			RunID       string `json:"runId"`
			Text        string `json:"text"`
		}{operationID, runID, text}})
	if err != nil || len(body) > cursorControlRequestMax {
		return cursorControlEnvelope{}, promptNotAppliedError(runtime.CodeInvalid, "Cursor owner request is invalid", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bound.control.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return cursorControlEnvelope{}, promptNotAppliedError(runtime.CodeInvalid, "Cursor owner request is invalid", err)
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+bound.control.Credential())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", fmt.Sprint(len(body)))
	req.ContentLength = int64(len(body))
	var wroteHeaders atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteHeaders: func() { wroteHeaders.Store(true) }}))
	timeout := e.cursorHTTPTimeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxConnsPerHost: 1, DialContext: (&net.Dialer{Timeout: timeout}).DialContext}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		if wroteHeaders.Load() {
			return cursorControlEnvelope{}, promptUnknownError("Cursor owner admission outcome is unknown", err)
		}
		return cursorControlEnvelope{}, promptNotAppliedError(runtime.CodeOffline, "Cursor owner endpoint is unavailable", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return cursorControlEnvelope{}, promptUnknownError("Cursor owner attempted to redirect admission", nil)
	}
	limited := io.LimitReader(response.Body, cursorControlResponseMax+1)
	raw, err := io.ReadAll(limited)
	if err != nil || len(raw) > cursorControlResponseMax || !utf8.Valid(raw) {
		return cursorControlEnvelope{}, promptUnknownError("Cursor owner response could not be read", err)
	}
	var object map[string]json.RawMessage
	if decodeStrictJSON(raw, &object) != nil || object == nil {
		return cursorControlEnvelope{}, promptUnknownError("Cursor owner response is invalid", nil)
	}
	envelope, err := parseCursorControlEnvelope(object, bound.registration.Manifest)
	if err != nil {
		return cursorControlEnvelope{}, promptUnknownError("Cursor owner response identity is invalid", err)
	}
	return envelope, nil
}

func parseCursorControlEnvelope(object map[string]json.RawMessage, manifest journal.CursorManifest) (cursorControlEnvelope, error) {
	var envelope cursorControlEnvelope
	for key, target := range map[string]*string{"ownerId": &envelope.OwnerID, "caseId": &envelope.CaseID, "cwd": &envelope.Cwd} {
		if raw, ok := object[key]; !ok || json.Unmarshal(raw, target) != nil || *target == "" {
			return envelope, errors.New("missing response binding")
		}
	}
	if envelope.OwnerID != manifest.OwnerID || envelope.CaseID != manifest.CaseID || envelope.Cwd != manifest.Cwd {
		return envelope, errors.New("response binding mismatch")
	}
	var identity map[string]json.RawMessage
	if raw, ok := object["identity"]; !ok || json.Unmarshal(raw, &identity) != nil || len(identity) != 5 {
		return envelope, errors.New("invalid response identity")
	}
	if err := json.Unmarshal(identity["pid"], &envelope.Identity.PID); err != nil {
		return envelope, err
	}
	for key, target := range map[string]*string{"startTime": &envelope.Identity.StartTime, "bootId": &envelope.Identity.BootID, "executable": &envelope.Identity.Executable, "commandLine": &envelope.Identity.CommandLine} {
		if json.Unmarshal(identity[key], target) != nil || *target == "" {
			return envelope, errors.New("invalid response identity field")
		}
	}
	if !journal.SameCursorIdentity(envelope.Identity, manifest.OwnerIdentity) {
		return envelope, errors.New("response owner identity mismatch")
	}
	envelope.Object = object
	return envelope, nil
}

func validateCursorPromptResponse(response cursorControlEnvelope, bound cursorPromptBinding, operationID, runID, text string) (bool, runtime.ErrorCode, error) {
	ok, okPresent := cursorResponseBool(response.Object, "ok")
	recorded, recordedPresent := cursorResponseBool(response.Object, "recorded")
	if !okPresent || !recordedPresent {
		return false, runtime.CodeInternal, errors.New("missing response outcome")
	}
	if !ok {
		if recorded {
			return false, runtime.CodeInternal, errors.New("refusal claims recording")
		}
		var failure map[string]json.RawMessage
		if json.Unmarshal(response.Object["error"], &failure) != nil || failure == nil {
			return false, runtime.CodeInternal, errors.New("invalid refusal")
		}
		notSent, notSentPresent := cursorResponseBool(failure, "notSent")
		unknown, unknownPresent := cursorResponseBool(failure, "outcomeUnknown")
		if !notSentPresent || !unknownPresent || !notSent || unknown {
			return false, runtime.CodeInternal, errors.New("refusal outcome is uncertain")
		}
		var code string
		_ = json.Unmarshal(failure["code"], &code)
		return false, cursorRefusalCode(code), nil
	}
	if !recorded {
		return false, runtime.CodeInternal, errors.New("successful response was not recorded")
	}
	var topOperationID, savedAt string
	var revision int64
	if json.Unmarshal(response.Object["operationId"], &topOperationID) != nil || topOperationID != operationID ||
		json.Unmarshal(response.Object["revision"], &revision) != nil || revision <= 0 || revision > cursorJSSafeInteger ||
		json.Unmarshal(response.Object["savedAt"], &savedAt) != nil {
		return false, runtime.CodeInternal, errors.New("invalid admission metadata")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, savedAt); err != nil || parsed.Format(time.RFC3339Nano) == "" {
		return false, runtime.CodeInternal, errors.New("invalid admission time")
	}
	var operation map[string]json.RawMessage
	if json.Unmarshal(response.Object["operation"], &operation) != nil || operation == nil {
		return false, runtime.CodeInternal, errors.New("missing admitted operation")
	}
	wanted := map[string]string{"ownerId": bound.registration.Manifest.OwnerID, "caseId": bound.registration.Manifest.CaseID,
		"cwd": bound.registration.Manifest.Cwd, "nativeSessionId": bound.registration.Manifest.NativeSessionID,
		"operationId": operationID, "runId": runID, "text": text}
	for key, expected := range wanted {
		var actual string
		if json.Unmarshal(operation[key], &actual) != nil || actual != expected {
			return false, runtime.CodeInternal, errors.New("admitted operation identity mismatch")
		}
	}
	var status string
	if json.Unmarshal(operation["status"], &status) != nil || !cursorOwnerOperationStatus(status) {
		return false, runtime.CodeInternal, errors.New("admitted operation status is invalid")
	}
	return true, "", nil
}

func cursorOwnerOperationStatus(value string) bool {
	switch value {
	case "admitted", "sending", "running", "waiting", "uncertain", "error", "completed":
		return true
	default:
		return false
	}
}

func cursorResponseBool(object map[string]json.RawMessage, key string) (bool, bool) {
	raw, present := object[key]
	if !present {
		return false, false
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func cursorRefusalCode(code string) runtime.ErrorCode {
	switch code {
	case "session_not_ready", "agent_busy", "busy", "not_ready":
		return runtime.CodeNotReady
	case "invalid_argument":
		return runtime.CodeInvalid
	case "rate_limited":
		return runtime.CodeRateLimited
	default:
		return runtime.CodeConflict
	}
}

func promptNotApplied(code runtime.ErrorCode, message string, cause error) (runtime.Receipt, error) {
	return runtime.Receipt{Outcome: runtime.OutcomeNotApplied}, promptNotAppliedError(code, message, cause)
}

func promptNotAppliedError(code runtime.ErrorCode, message string, cause error) error {
	return &runtime.Fault{Code: code, Operation: "agent.prompt", Outcome: runtime.OutcomeNotApplied, Retry: runtime.RetryNever, SafeMessage: message, Cause: cause}
}

func promptUnknown(message string, cause error) (runtime.Receipt, error) {
	return runtime.Receipt{Outcome: runtime.OutcomeUnknown}, promptUnknownError(message, cause)
}

func promptUnknownError(message string, cause error) error {
	return &runtime.Fault{Code: runtime.CodeTimeout, Operation: "agent.prompt", Outcome: runtime.OutcomeUnknown, Retry: runtime.RetryUserOnly, SafeMessage: message, Cause: cause}
}

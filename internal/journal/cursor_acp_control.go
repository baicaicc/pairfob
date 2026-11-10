package journal

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
)

const cursorOwnerKind = "allinone-cursor-owner-v1"

// CursorControl is a private, authenticated owner endpoint. Its fields stay
// unexported so credentials cannot be serialized into RPC or audit payloads.
type CursorControl struct {
	endpoint     string
	credential   string
	intentDigest string
	ownerDigest  string
}

func (c CursorControl) Endpoint() string   { return c.endpoint }
func (c CursorControl) Credential() string { return c.credential }

// LoadControl reads owner credentials only after the caller has established a
// trusted manifest binding. Actual owner records may add fields over time.
func (c *CursorConfig) LoadControl(manifest CursorManifest) (*CursorControl, error) {
	if c == nil || c.validateManifest(manifest, manifest.TerminalID) != nil {
		return nil, errors.New("invalid Cursor control manifest")
	}
	if err := validatePrivateDirectory(manifest.OwnerDir); err != nil || !withinPrivateRoot(manifest.OwnerDir, c.OwnerRoots) {
		return nil, errors.New("Cursor owner directory is no longer private and configured")
	}
	intentRaw, err := readPrivateFile(filepath.Join(manifest.OwnerDir, "intent.json"), cursorSessionRecordMax, 0o600)
	if err != nil {
		return nil, errors.New("Cursor owner intent is unavailable")
	}
	ownerRaw, err := readPrivateFile(filepath.Join(manifest.OwnerDir, "owner.json"), cursorSessionRecordMax, 0o600)
	if err != nil {
		return nil, errors.New("Cursor owner endpoint is unavailable")
	}
	intent, err := parseCursorIntent(intentRaw, manifest)
	if err != nil {
		return nil, err
	}
	endpoint, err := parseCursorOwner(ownerRaw, manifest, intent.nonce)
	if err != nil {
		return nil, err
	}
	intentSum, ownerSum := sha256.Sum256(intentRaw), sha256.Sum256(ownerRaw)
	return &CursorControl{endpoint: endpoint, credential: intent.credential, intentDigest: hex.EncodeToString(intentSum[:]), ownerDigest: hex.EncodeToString(ownerSum[:])}, nil
}

// VerifyControl proves the exact private control records are still published.
func (c *CursorConfig) VerifyControl(manifest CursorManifest, expected *CursorControl) error {
	if expected == nil {
		return errors.New("missing Cursor control baseline")
	}
	actual, err := c.LoadControl(manifest)
	if err != nil || subtle.ConstantTimeCompare([]byte(actual.intentDigest), []byte(expected.intentDigest)) != 1 ||
		subtle.ConstantTimeCompare([]byte(actual.ownerDigest), []byte(expected.ownerDigest)) != 1 {
		return errors.New("Cursor control records changed")
	}
	return nil
}

type cursorIntentPrivate struct {
	nonce      string
	credential string
}

func parseCursorIntent(raw []byte, manifest CursorManifest) (cursorIntentPrivate, error) {
	var object map[string]json.RawMessage
	if decodeJSONObject(raw, &object) != nil {
		return cursorIntentPrivate{}, errors.New("invalid Cursor owner intent")
	}
	values := map[string]string{}
	for _, key := range []string{"kind", "ownerId", "caseId", "cwd", "nonce", "credential"} {
		var value string
		if requiredJSONString(object, key, &value) != nil || !validBounded(value, cursorIDMax) {
			return cursorIntentPrivate{}, errors.New("invalid Cursor owner intent fields")
		}
		values[key] = value
	}
	if values["kind"] != cursorOwnerKind || values["ownerId"] != manifest.OwnerID || values["caseId"] != manifest.CaseID || values["cwd"] != manifest.Cwd {
		return cursorIntentPrivate{}, errors.New("Cursor owner intent does not match manifest")
	}
	return cursorIntentPrivate{nonce: values["nonce"], credential: values["credential"]}, nil
}

func parseCursorOwner(raw []byte, manifest CursorManifest, nonce string) (string, error) {
	var object map[string]json.RawMessage
	if decodeJSONObject(raw, &object) != nil {
		return "", errors.New("invalid Cursor owner record")
	}
	var ownerID, ownerNonce, endpoint string
	if requiredJSONString(object, "ownerId", &ownerID) != nil || requiredJSONString(object, "nonce", &ownerNonce) != nil ||
		requiredJSONString(object, "endpoint", &endpoint) != nil || !validSegment(ownerID) || !validBounded(ownerNonce, cursorIDMax) ||
		!validBounded(endpoint, cursorPathMax) || ownerID != manifest.OwnerID || ownerNonce != nonce {
		return "", errors.New("Cursor owner record does not match intent")
	}
	var identityObject map[string]json.RawMessage
	if requiredJSON(object, "identity", &identityObject) != nil || identityObject == nil {
		return "", errors.New("Cursor owner identity is invalid")
	}
	identity, err := parseCursorFingerprint(identityObject)
	if err != nil || !SameCursorIdentity(identity, manifest.OwnerIdentity) {
		return "", errors.New("Cursor owner identity does not match manifest")
	}
	if err := validateCursorEndpoint(endpoint); err != nil {
		return "", err
	}
	return endpoint, nil
}

func parseCursorFingerprint(object map[string]json.RawMessage) (CursorProcessIdentity, error) {
	var identity CursorProcessIdentity
	if requiredJSON(object, "pid", &identity.PID) != nil || requiredJSONString(object, "startTime", &identity.StartTime) != nil ||
		requiredJSONString(object, "bootId", &identity.BootID) != nil || requiredJSONString(object, "executable", &identity.Executable) != nil ||
		requiredJSONString(object, "commandLine", &identity.CommandLine) != nil || !validIdentity(identity) {
		return CursorProcessIdentity{}, errors.New("invalid Cursor process fingerprint")
	}
	return identity, nil
}

func validateCursorEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.Path != "/control" ||
		parsed.EscapedPath() != "/control" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return errors.New("invalid Cursor owner endpoint")
	}
	if net.ParseIP(parsed.Hostname()) == nil || parsed.Host != net.JoinHostPort("127.0.0.1", parsed.Port()) {
		return errors.New("Cursor owner endpoint is not exact loopback")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1024 || port > 65535 {
		return fmt.Errorf("invalid Cursor owner endpoint port")
	}
	return nil
}

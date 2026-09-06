package journal

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CursorConfigEnv        = "PAIRFOB_ALLINONE_CURSOR_CONFIG"
	cursorSmallRecordMax   = 64 << 10
	cursorSessionRecordMax = 16 << 20
	cursorPathMax          = 16 << 10
	cursorIDMax            = 64 << 10
	cursorRootMax          = 32
)

var cursorSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var cursorSessionName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type CursorConfig struct {
	SchemaVersion  int      `json:"schemaVersion"`
	RegistryRoot   string   `json:"registryRoot"`
	OwnerRoots     []string `json:"ownerRoots"`
	BindingRoots   []string `json:"bindingRoots"`
	NodePath       string   `json:"nodePath"`
	EntryPath      string   `json:"entryPath"`
	RuntimeSession string   `json:"runtimeSession"`
}

type CursorProcessIdentity struct {
	PID         int    `json:"pid"`
	StartTime   string `json:"startTime"`
	BootID      string `json:"bootId"`
	Executable  string `json:"executable"`
	CommandLine string `json:"commandLine"`
}

type CursorManifest struct {
	SchemaVersion   int                   `json:"schemaVersion"`
	Source          string                `json:"source"`
	Agent           string                `json:"agent"`
	WorkspaceID     string                `json:"workspaceId"`
	PaneID          string                `json:"paneId"`
	TerminalID      string                `json:"terminalId"`
	OwnerDir        string                `json:"ownerDir"`
	OwnerID         string                `json:"ownerId"`
	CaseID          string                `json:"caseId"`
	Cwd             string                `json:"cwd"`
	NativeSessionID string                `json:"nativeSessionId"`
	OwnerIdentity   CursorProcessIdentity `json:"ownerIdentity"`
	BindingPath     string                `json:"bindingPath"`
	NodePath        string                `json:"nodePath"`
	EntryPath       string                `json:"entryPath"`
}

type CursorRegistration struct {
	Manifest CursorManifest
	Path     string
	Digest   string
}

type CursorBinding struct {
	SchemaVersion int                   `json:"schemaVersion"`
	OwnerDir      string                `json:"ownerDir"`
	OwnerID       string                `json:"ownerId"`
	CaseID        string                `json:"caseId"`
	Cwd           string                `json:"cwd"`
	Identity      CursorProcessIdentity `json:"identity"`
	TerminalID    string                `json:"terminalId"`
}

type CursorClaim struct {
	SchemaVersion int                   `json:"schemaVersion"`
	TerminalID    string                `json:"terminalId"`
	ClaimID       string                `json:"claimId"`
	Identity      CursorProcessIdentity `json:"identity"`
}

type CursorSession struct {
	SchemaVersion   int
	Revision        int64
	SavedAt         time.Time
	OwnerID         string
	Cwd             string
	NativeSessionID string
	Status          string
}

func LoadCursorConfigFromEnv() (*CursorConfig, error) {
	path, present := os.LookupEnv(CursorConfigEnv)
	if !present {
		return nil, nil
	}
	if path == "" {
		return nil, errors.New("Cursor local resolver config path is empty")
	}
	config, err := LoadCursorConfig(path)
	if err != nil {
		return nil, fmt.Errorf("Cursor local resolver config: %w", err)
	}
	return config, nil
}

func LoadCursorConfig(path string) (*CursorConfig, error) {
	if err := canonicalAbsolute(path); err != nil {
		return nil, fmt.Errorf("invalid config path: %w", err)
	}
	raw, err := readPrivateFile(path, cursorSmallRecordMax, 0o600)
	if err != nil {
		return nil, err
	}
	var config CursorConfig
	if err := decodeExactObject(raw, &config, "schemaVersion", "registryRoot", "ownerRoots", "bindingRoots", "nodePath", "entryPath", "runtimeSession"); err != nil {
		return nil, fmt.Errorf("invalid config JSON: %w", err)
	}
	if config.SchemaVersion != 1 || len(config.OwnerRoots) == 0 || len(config.OwnerRoots) > cursorRootMax || len(config.BindingRoots) == 0 || len(config.BindingRoots) > cursorRootMax {
		return nil, errors.New("invalid schema version or root count")
	}
	if config.RuntimeSession != "" && (!cursorSessionName.MatchString(config.RuntimeSession) || config.RuntimeSession == "." || config.RuntimeSession == "..") {
		return nil, errors.New("invalid runtimeSession")
	}
	if err := validateRoot(config.RegistryRoot); err != nil {
		return nil, fmt.Errorf("invalid registryRoot: %w", err)
	}
	for name, roots := range map[string][]string{"ownerRoots": config.OwnerRoots, "bindingRoots": config.BindingRoots} {
		local := make(map[string]bool, len(roots))
		for _, root := range roots {
			if local[root] {
				return nil, fmt.Errorf("%s contains a duplicate root", name)
			}
			local[root] = true
			if err := validateRoot(root); err != nil {
				return nil, fmt.Errorf("invalid %s: %w", name, err)
			}
		}
	}
	for _, root := range append(append([]string(nil), config.OwnerRoots...), config.BindingRoots...) {
		if pathsOverlap(config.RegistryRoot, root) {
			return nil, errors.New("registryRoot must be separate from ownerRoots and bindingRoots")
		}
	}
	if config.NodePath == config.EntryPath {
		return nil, errors.New("nodePath and entryPath must be distinct")
	}
	for name, path := range map[string]string{"nodePath": config.NodePath, "entryPath": config.EntryPath} {
		if err := validateRegularPath(path); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", name, err)
		}
	}
	return &config, nil
}

func (c *CursorConfig) LoadRegistration(terminalID string) (*CursorRegistration, error) {
	if c == nil || !validSegment(terminalID) {
		return nil, errors.New("invalid Cursor terminal ID")
	}
	sum := sha256.Sum256([]byte(terminalID))
	path := filepath.Join(c.RegistryRoot, hex.EncodeToString(sum[:])+".json")
	return c.loadRegistrationPath(path, terminalID, "")
}

// LoadRegistrationRef reopens a daemon-produced Cursor ref and verifies both
// its configured registry location and digest. It is the static Reader seam;
// it performs no runtime or process inspection.
func (c *CursorConfig) LoadRegistrationRef(ref Ref) (*CursorRegistration, error) {
	if c == nil || ref.Source != "allinone:cursor-acp" || ref.Agent != "cursor" || ref.Kind != "path" || len(ref.ManifestDigest) != sha256.Size*2 {
		return nil, errors.New("invalid Cursor manifest ref")
	}
	digest, err := hex.DecodeString(ref.ManifestDigest)
	if err != nil || hex.EncodeToString(digest) != ref.ManifestDigest {
		return nil, errors.New("invalid Cursor manifest digest")
	}
	if filepath.Dir(ref.Value) != c.RegistryRoot || filepath.Ext(ref.Value) != ".json" {
		return nil, errors.New("Cursor manifest ref is outside configured registry")
	}
	registration, err := c.loadRegistrationPath(ref.Value, "", ref.ManifestDigest)
	if err == nil && registration == nil {
		err = errors.New("Cursor manifest ref is unavailable")
	}
	return registration, err
}

func (c *CursorConfig) loadRegistrationPath(path, terminalID, requiredDigest string) (*CursorRegistration, error) {
	if err := validateRoot(c.RegistryRoot); err != nil {
		return nil, errors.New("Cursor registry root is no longer private and canonical")
	}
	raw, err := readPrivateFile(path, cursorSmallRecordMax, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest CursorManifest
	if err := decodeExactObject(raw, &manifest, "schemaVersion", "source", "agent", "workspaceId", "paneId", "terminalId", "ownerDir", "ownerId", "caseId", "cwd", "nativeSessionId", "ownerIdentity", "bindingPath", "nodePath", "entryPath"); err != nil {
		return nil, fmt.Errorf("invalid Cursor manifest: %w", err)
	}
	if terminalID == "" {
		terminalID = manifest.TerminalID
	}
	terminalHash := sha256.Sum256([]byte(terminalID))
	expectedPath := filepath.Join(c.RegistryRoot, hex.EncodeToString(terminalHash[:])+".json")
	if path != expectedPath {
		return nil, errors.New("Cursor manifest filename does not match terminal ID")
	}
	if err := c.validateManifest(manifest, terminalID); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	digestText := hex.EncodeToString(digest[:])
	if requiredDigest != "" && subtle.ConstantTimeCompare([]byte(digestText), []byte(requiredDigest)) != 1 {
		return nil, errors.New("Cursor manifest digest does not match ref")
	}
	return &CursorRegistration{Manifest: manifest, Path: path, Digest: digestText}, nil
}

func (c *CursorConfig) LoadBinding(manifest CursorManifest) (CursorBinding, error) {
	var binding CursorBinding
	raw, err := readPrivateFile(manifest.BindingPath, cursorSmallRecordMax, 0o600)
	if err == nil {
		err = decodeExactObject(raw, &binding, "schemaVersion", "ownerDir", "ownerId", "caseId", "cwd", "identity", "terminalId")
	}
	if err != nil {
		return binding, fmt.Errorf("invalid Cursor binding: %w", err)
	}
	if binding.SchemaVersion != 1 || binding.OwnerDir != manifest.OwnerDir || binding.OwnerID != manifest.OwnerID || binding.CaseID != manifest.CaseID || binding.Cwd != manifest.Cwd || binding.TerminalID != manifest.TerminalID || !SameCursorIdentity(binding.Identity, manifest.OwnerIdentity) {
		return binding, errors.New("Cursor binding does not match manifest")
	}
	return binding, nil
}

func (c *CursorConfig) LoadClaim(manifest CursorManifest) (CursorClaim, error) {
	var claim CursorClaim
	terminalDir := filepath.Join(manifest.OwnerDir, "terminals", manifest.TerminalID)
	if err := validatePrivateDirectory(filepath.Join(manifest.OwnerDir, "terminals")); err != nil {
		return claim, err
	}
	if err := validatePrivateDirectory(terminalDir); err != nil {
		return claim, err
	}
	raw, err := readPrivateFile(filepath.Join(terminalDir, "claim.json"), cursorSmallRecordMax, 0o600)
	if err == nil {
		err = decodeExactObject(raw, &claim, "schemaVersion", "terminalId", "claimId", "identity")
	}
	if err != nil || claim.SchemaVersion != 1 || claim.TerminalID != manifest.TerminalID || !validBounded(claim.ClaimID, cursorIDMax) || !validIdentity(claim.Identity) {
		if err == nil {
			err = errors.New("claim fields do not match")
		}
		return claim, fmt.Errorf("invalid Cursor claim: %w", err)
	}
	return claim, nil
}

func (c *CursorConfig) LoadSession(manifest CursorManifest) (CursorSession, error) {
	raw, err := readPrivateFile(filepath.Join(manifest.OwnerDir, "session.json"), cursorSessionRecordMax, 0o600)
	if err != nil {
		return CursorSession{}, err
	}
	parsed, err := parseCursorSession(raw, manifest, false)
	return parsed.CursorSession, err
}

func SameCursorIdentity(a, b CursorProcessIdentity) bool {
	return validIdentity(a) && validIdentity(b) && a == b
}

func (c *CursorConfig) validateManifest(m CursorManifest, terminalID string) error {
	if m.SchemaVersion != 1 || m.Source != "allinone:cursor-acp" || m.Agent != "Cursor ACP" || m.TerminalID != terminalID || !validSegment(m.OwnerID) || !validSegment(m.TerminalID) {
		return errors.New("invalid Cursor manifest identity fields")
	}
	if !validBounded(m.WorkspaceID, 256) || !validBounded(m.PaneID, 256) || !validBounded(m.CaseID, cursorIDMax) || !validBounded(m.NativeSessionID, cursorIDMax) || !validIdentity(m.OwnerIdentity) {
		return errors.New("invalid Cursor manifest bounded fields")
	}
	if m.NodePath != c.NodePath || m.EntryPath != c.EntryPath {
		return errors.New("Cursor manifest fixed paths do not match config")
	}
	if err := validatePrivateDirectory(m.OwnerDir); err != nil || filepath.Base(m.OwnerDir) != m.OwnerID || !withinPrivateRoot(m.OwnerDir, c.OwnerRoots) {
		return errors.New("Cursor manifest owner directory is outside configured roots")
	}
	if !withinPrivateRoot(m.BindingPath, c.BindingRoots) {
		return errors.New("Cursor manifest binding is outside configured roots")
	}
	if err := validateDirectoryPath(m.Cwd, false); err != nil {
		return fmt.Errorf("invalid Cursor cwd: %w", err)
	}
	if err := validateRegularPath(m.BindingPath); err != nil {
		return fmt.Errorf("invalid Cursor binding path: %w", err)
	}
	if err := validateRegularPath(m.OwnerIdentity.Executable); err != nil {
		return fmt.Errorf("invalid Cursor owner executable: %w", err)
	}
	return nil
}

func validIdentity(identity CursorProcessIdentity) bool {
	return identity.PID > 0 && identity.PID <= 1<<31-1 && validBounded(identity.StartTime, cursorIDMax) && validBounded(identity.BootID, cursorIDMax) && validBounded(identity.CommandLine, cursorIDMax) && validateRegularPath(identity.Executable) == nil
}

func validSegment(value string) bool {
	return cursorSegment.MatchString(value) && value != "." && value != ".."
}

func validBounded(value string, max int) bool {
	return value != "" && utf8.ValidString(value) && len([]byte(value)) <= max
}

func requiredJSON(object map[string]json.RawMessage, key string, target any) error {
	raw, ok := object[key]
	if !ok {
		return errors.New("missing field")
	}
	return json.Unmarshal(raw, target)
}

func decodeExact(raw []byte, target any) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func decodeExactObject(raw []byte, target any, keys ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil || len(object) != len(keys) {
		return errors.New("JSON object does not have the exact schema")
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return errors.New("JSON object does not have the exact schema")
		}
	}
	return decodeExact(raw, target)
}

func canonicalAbsolute(path string) error {
	if !validBounded(path, cursorPathMax) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("path must be a bounded canonical absolute path")
	}
	if err := noSymlinkPath(path); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("path must not contain symbolic links")
	}
	return nil
}

func noSymlinkPath(path string) error {
	volume := filepath.VolumeName(path)
	current := volume + string(os.PathSeparator)
	relative := path[len(current):]
	for _, part := range splitPath(relative) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symbolic link in path")
		}
	}
	return nil
}

func splitPath(path string) []string {
	var parts []string
	for path != "." && path != "" && path != string(os.PathSeparator) {
		dir, base := filepath.Split(path)
		if base != "" {
			parts = append([]string{base}, parts...)
		}
		path = filepath.Clean(dir)
	}
	return parts
}

func validateRoot(path string) error { return validateDirectoryPath(path, true) }

func validatePrivateDirectory(path string) error { return validateDirectoryPath(path, true) }

func validateDirectoryPath(path string, private bool) error {
	if err := canonicalAbsolute(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return errors.New("path is not an existing directory")
	}
	if private && info.Mode().Perm() != 0o700 {
		return errors.New("directory mode must be 0700")
	}
	return nil
}

func validateRegularPath(path string) error {
	if err := canonicalAbsolute(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	return nil
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && len(rel) > 0 && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func pathsOverlap(one, two string) bool {
	return pathContains(one, two) || pathContains(two, one)
}

func pathContains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func withinPrivateRoot(path string, roots []string) bool {
	for _, root := range roots {
		if validateRoot(root) == nil && withinAny(path, []string{root}) {
			return true
		}
	}
	return false
}

func readPrivateFile(path string, maxBytes int64, mode os.FileMode) ([]byte, error) {
	if err := canonicalAbsolute(path); err != nil {
		return nil, err
	}
	file, err := openCursorNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > maxBytes || before.Mode().Perm() != mode {
		return nil, errors.New("private record has invalid type, size, or mode")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(raw)) != before.Size() {
		return nil, errors.New("private record could not be read completely")
	}
	after, err := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	canonicalErr := canonicalAbsolute(path)
	if err != nil || pathErr != nil || canonicalErr != nil || !cursorFileStable(before, after) || !os.SameFile(after, pathInfo) {
		return nil, errors.New("private record changed while being read")
	}
	return raw, nil
}

//go:build darwin || linux

package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCursorLocalFilesLoadExactPrivateRecords(t *testing.T) {
	f := newCursorLocalFixture(t)
	config, err := LoadCursorConfig(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := config.LoadRegistration(f.manifest.TerminalID)
	if err != nil {
		t.Fatal(err)
	}
	if registration == nil || registration.Manifest != f.manifest || len(registration.Digest) != 64 {
		t.Fatalf("unexpected registration: %#v", registration)
	}
	ref := Ref{Source: CursorACPSource, Agent: CursorACPAgent, Kind: "path", Value: registration.Path, ManifestDigest: registration.Digest}
	if reopened, err := config.LoadRegistrationRef(ref); err != nil || reopened.Digest != registration.Digest {
		t.Fatalf("digest-bound ref did not reopen: %#v, %v", reopened, err)
	}
	first := byte('0')
	if ref.ManifestDigest[0] == '0' {
		first = '1'
	}
	ref.ManifestDigest = string(first) + ref.ManifestDigest[1:]
	if _, err := config.LoadRegistrationRef(ref); err == nil {
		t.Fatal("wrong manifest digest was accepted")
	}
	if _, err := config.LoadBinding(f.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadClaim(f.manifest); err != nil {
		t.Fatal(err)
	}
	session, err := config.LoadSession(f.manifest)
	if err != nil || session.Revision != 1 || session.NativeSessionID != f.manifest.NativeSessionID {
		t.Fatalf("unexpected session %#v: %v", session, err)
	}
	if missing, err := config.LoadRegistration("another-terminal"); err != nil || missing != nil {
		t.Fatalf("missing direct lookup = %#v, %v", missing, err)
	}
}

func TestCursorConfigRejectsUnsafeAndNonExactFiles(t *testing.T) {
	f := newCursorLocalFixture(t)
	raw, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected"] = true
	writeCursorJSON(t, f.configPath, object)
	if _, err := LoadCursorConfig(f.configPath); err == nil {
		t.Fatal("unknown config field was accepted")
	}
	delete(object, "unexpected")
	writeCursorJSON(t, f.configPath, object)
	if err := os.Chmod(f.configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCursorConfig(f.configPath); err == nil {
		t.Fatal("public config mode was accepted")
	}

	fifo := filepath.Join(f.root, "config-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCursorConfig(fifo); err == nil {
		t.Fatal("FIFO config was accepted")
	}
}

func TestCursorManifestDigestAndSchemaFailClosed(t *testing.T) {
	f := newCursorLocalFixture(t)
	config, err := LoadCursorConfig(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := config.LoadRegistration(f.manifest.TerminalID)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := registration.Path
	var object map[string]any
	raw, _ := os.ReadFile(manifestPath)
	_ = json.Unmarshal(raw, &object)
	object["paneId"] = "other-pane"
	writeCursorJSON(t, manifestPath, object)
	changed, err := config.LoadRegistration(f.manifest.TerminalID)
	if err != nil || changed.Digest == registration.Digest {
		t.Fatalf("digest did not bind raw manifest bytes: %#v, %v", changed, err)
	}
	object["unknown"] = true
	writeCursorJSON(t, manifestPath, object)
	if _, err := config.LoadRegistration(f.manifest.TerminalID); err == nil {
		t.Fatal("manifest with unknown field was accepted")
	}
}

func TestCursorConfigRequiresRegistrySeparationButAllowsOwnerBindingOverlap(t *testing.T) {
	for _, test := range []struct {
		name  string
		roots func(*cursorLocalFixture) (string, string)
	}{
		{name: "registry equals owner", roots: func(f *cursorLocalFixture) (string, string) { return f.ownerRoot, f.ownerRoot }},
		{name: "registry contains owner", roots: func(f *cursorLocalFixture) (string, string) {
			return f.registry, cursorMkdir(f.t, filepath.Join(f.registry, "owner-root"), 0o700)
		}},
		{name: "owner contains registry", roots: func(f *cursorLocalFixture) (string, string) {
			return cursorMkdir(f.t, filepath.Join(f.ownerRoot, "registry"), 0o700), f.ownerRoot
		}},
		{name: "registry equals binding", roots: func(f *cursorLocalFixture) (string, string) { return f.bindingRoot, f.bindingRoot }},
		{name: "registry contains binding", roots: func(f *cursorLocalFixture) (string, string) {
			return f.registry, cursorMkdir(f.t, filepath.Join(f.registry, "binding-root"), 0o700)
		}},
		{name: "binding contains registry", roots: func(f *cursorLocalFixture) (string, string) {
			return cursorMkdir(f.t, filepath.Join(f.bindingRoot, "registry"), 0o700), f.bindingRoot
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCursorLocalFixture(t)
			registry, overlap := test.roots(&f)
			config := f.config
			config.RegistryRoot = registry
			if strings.Contains(test.name, "owner") {
				config.OwnerRoots = []string{overlap}
			} else {
				config.BindingRoots = []string{overlap}
			}
			writeCursorJSON(t, f.configPath, config)
			if _, err := LoadCursorConfig(f.configPath); err == nil {
				t.Fatal("overlapping registry root was accepted")
			}
		})
	}

	f := newCursorLocalFixture(t)
	config := f.config
	config.BindingRoots = []string{f.ownerRoot}
	writeCursorJSON(t, f.configPath, config)
	if _, err := LoadCursorConfig(f.configPath); err != nil {
		t.Fatalf("owner/binding root overlap must remain allowed: %v", err)
	}
}

func TestCursorPrivateReadersRejectSymlinksOversizeAndUnsafeModes(t *testing.T) {
	t.Run("symlinks", func(t *testing.T) {
		f := newCursorLocalFixture(t)
		fileLink := filepath.Join(f.root, "config-link.json")
		if err := os.Symlink(f.configPath, fileLink); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCursorConfig(fileLink); err == nil {
			t.Fatal("config file symlink was accepted")
		}
		parentLink := filepath.Join(filepath.Dir(f.root), filepath.Base(f.root)+"-link")
		if err := os.Symlink(f.root, parentLink); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(parentLink) })
		if _, err := LoadCursorConfig(filepath.Join(parentLink, "config.json")); err == nil {
			t.Fatal("config parent symlink was accepted")
		}
		actual := filepath.Join(f.registry, "actual.json")
		if err := os.Rename(f.manifestPath, actual); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(actual, f.manifestPath); err != nil {
			t.Fatal(err)
		}
		if _, err := f.loaded.LoadRegistration(f.manifest.TerminalID); err == nil {
			t.Fatal("manifest file symlink was accepted")
		}
	})

	for _, test := range []struct {
		name string
		max  int
		path func(cursorLocalFixture) string
		load func(cursorLocalFixture) error
	}{
		{name: "config", max: cursorSmallRecordMax, path: func(f cursorLocalFixture) string { return f.configPath }, load: func(f cursorLocalFixture) error { _, err := LoadCursorConfig(f.configPath); return err }},
		{name: "manifest", max: cursorSmallRecordMax, path: func(f cursorLocalFixture) string { return f.manifestPath }, load: func(f cursorLocalFixture) error {
			_, err := f.loaded.LoadRegistration(f.manifest.TerminalID)
			return err
		}},
		{name: "binding", max: cursorSmallRecordMax, path: func(f cursorLocalFixture) string { return f.bindingPath }, load: func(f cursorLocalFixture) error { _, err := f.loaded.LoadBinding(f.manifest); return err }},
		{name: "claim", max: cursorSmallRecordMax, path: func(f cursorLocalFixture) string { return f.claimPath }, load: func(f cursorLocalFixture) error { _, err := f.loaded.LoadClaim(f.manifest); return err }},
		{name: "session", max: cursorSessionRecordMax, path: func(f cursorLocalFixture) string { return f.sessionPath }, load: func(f cursorLocalFixture) error { _, err := f.loaded.LoadSession(f.manifest); return err }},
	} {
		t.Run("oversize "+test.name, func(t *testing.T) {
			f := newCursorLocalFixture(t)
			writeCursorRaw(t, test.path(f), []byte(strings.Repeat("x", test.max+1)))
			if err := test.load(f); err == nil {
				t.Fatal("oversized private record was accepted")
			}
		})
	}

	for _, test := range []struct {
		name string
		path func(cursorLocalFixture) string
		load func(cursorLocalFixture) error
	}{
		{name: "registry root", path: func(f cursorLocalFixture) string { return f.registry }, load: func(f cursorLocalFixture) error {
			_, err := f.loaded.LoadRegistration(f.manifest.TerminalID)
			return err
		}},
		{name: "owner root", path: func(f cursorLocalFixture) string { return f.ownerRoot }, load: func(f cursorLocalFixture) error {
			_, err := f.loaded.LoadRegistration(f.manifest.TerminalID)
			return err
		}},
		{name: "binding root", path: func(f cursorLocalFixture) string { return f.bindingRoot }, load: func(f cursorLocalFixture) error {
			_, err := f.loaded.LoadRegistration(f.manifest.TerminalID)
			return err
		}},
		{name: "manifest", path: func(f cursorLocalFixture) string { return f.manifestPath }, load: func(f cursorLocalFixture) error {
			_, err := f.loaded.LoadRegistration(f.manifest.TerminalID)
			return err
		}},
		{name: "binding", path: func(f cursorLocalFixture) string { return f.bindingPath }, load: func(f cursorLocalFixture) error { _, err := f.loaded.LoadBinding(f.manifest); return err }},
		{name: "claim", path: func(f cursorLocalFixture) string { return f.claimPath }, load: func(f cursorLocalFixture) error { _, err := f.loaded.LoadClaim(f.manifest); return err }},
		{name: "claim directory", path: func(f cursorLocalFixture) string { return filepath.Dir(f.claimPath) }, load: func(f cursorLocalFixture) error { _, err := f.loaded.LoadClaim(f.manifest); return err }},
		{name: "session", path: func(f cursorLocalFixture) string { return f.sessionPath }, load: func(f cursorLocalFixture) error { _, err := f.loaded.LoadSession(f.manifest); return err }},
	} {
		t.Run("mode "+test.name, func(t *testing.T) {
			f := newCursorLocalFixture(t)
			if err := os.Chmod(test.path(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := test.load(f); err == nil {
				t.Fatal("unsafe private mode was accepted")
			}
		})
	}
}

func TestCursorSessionIdentityAllowsMissingStatusAndContentGrowth(t *testing.T) {
	f := newCursorLocalFixture(t)
	writeCursorJSON(t, f.sessionPath, map[string]any{"schemaVersion": 1, "revision": 2, "savedAt": "2026-09-06T12:00:01Z", "session": map[string]any{"schemaVersion": 1, "ownerId": f.manifest.OwnerID, "cwd": f.manifest.Cwd, "nativeSessionId": f.manifest.NativeSessionID, "operations": []any{map[string]any{"publicText": "grown"}}, "output": map[string]any{"chars": 5}}})
	session, err := f.loaded.LoadSession(f.manifest)
	if err != nil || session.Revision != 2 || session.Status != "" {
		t.Fatalf("identity-only session load failed: %#v %v", session, err)
	}
}

type cursorLocalFixture struct {
	t            *testing.T
	root         string
	registry     string
	ownerRoot    string
	bindingRoot  string
	configPath   string
	manifestPath string
	bindingPath  string
	claimPath    string
	sessionPath  string
	config       CursorConfig
	loaded       *CursorConfig
	manifest     CursorManifest
}

func newCursorLocalFixture(t *testing.T) cursorLocalFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := cursorMkdir(t, filepath.Join(root, "registry"), 0o700)
	ownerRoot := cursorMkdir(t, filepath.Join(root, "owners"), 0o700)
	ownerDir := cursorMkdir(t, filepath.Join(ownerRoot, "owner-one"), 0o700)
	bindingRoot := cursorMkdir(t, filepath.Join(root, "bindings"), 0o700)
	cwd := cursorMkdir(t, filepath.Join(root, "cwd"), 0o755)
	terminals := cursorMkdir(t, filepath.Join(ownerDir, "terminals"), 0o700)
	terminalID := "terminal-one"
	terminalDir := cursorMkdir(t, filepath.Join(terminals, terminalID), 0o700)
	nodePath := filepath.Join(root, "node")
	entryPath := filepath.Join(root, "cursor-terminal.mjs")
	if err := os.WriteFile(nodePath, []byte("node"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entryPath, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownerIdentity := CursorProcessIdentity{PID: 101, StartTime: "owner-start", BootID: "boot", Executable: nodePath, CommandLine: "owner command"}
	clientIdentity := CursorProcessIdentity{PID: 202, StartTime: "client-start", BootID: "boot", Executable: nodePath, CommandLine: nodePath + " " + entryPath + " " + filepath.Join(bindingRoot, "binding.json")}
	bindingPath := filepath.Join(bindingRoot, "binding.json")
	manifest := CursorManifest{SchemaVersion: 1, Source: "allinone:cursor-acp", Agent: "Cursor ACP", WorkspaceID: "workspace-one", PaneID: "workspace-one:p1", TerminalID: terminalID, OwnerDir: ownerDir, OwnerID: "owner-one", CaseID: "case-one", Cwd: cwd, NativeSessionID: "native-one", OwnerIdentity: ownerIdentity, BindingPath: bindingPath, NodePath: nodePath, EntryPath: entryPath}
	writeCursorJSON(t, bindingPath, CursorBinding{SchemaVersion: 1, OwnerDir: ownerDir, OwnerID: "owner-one", CaseID: "case-one", Cwd: cwd, Identity: ownerIdentity, TerminalID: terminalID})
	claimPath := filepath.Join(terminalDir, "claim.json")
	sessionPath := filepath.Join(ownerDir, "session.json")
	writeCursorJSON(t, claimPath, CursorClaim{SchemaVersion: 1, TerminalID: terminalID, ClaimID: "claim-one", Identity: clientIdentity})
	writeCursorJSON(t, sessionPath, map[string]any{"schemaVersion": 1, "revision": 1, "savedAt": "2026-09-06T12:00:00Z", "session": map[string]any{"schemaVersion": 1, "ownerId": "owner-one", "cwd": cwd, "nativeSessionId": "native-one", "status": "ready", "operations": []any{}}})
	encoded, _ := json.Marshal(manifest)
	digest := sha256.Sum256([]byte(terminalID))
	manifestPath := filepath.Join(registry, hex.EncodeToString(digest[:])+".json")
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	config := CursorConfig{SchemaVersion: 1, RegistryRoot: registry, OwnerRoots: []string{ownerRoot}, BindingRoots: []string{bindingRoot}, NodePath: nodePath, EntryPath: entryPath, RuntimeSession: ""}
	writeCursorJSON(t, configPath, config)
	loaded, err := LoadCursorConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return cursorLocalFixture{t: t, root: root, registry: registry, ownerRoot: ownerRoot, bindingRoot: bindingRoot, configPath: configPath, manifestPath: manifestPath, bindingPath: bindingPath, claimPath: claimPath, sessionPath: sessionPath, config: config, loaded: loaded, manifest: manifest}
}

func cursorMkdir(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCursorJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCursorRaw(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

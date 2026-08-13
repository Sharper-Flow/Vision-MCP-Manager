package ownership

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeRootResolutionAndSafety(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		explicit string
		env      map[string]string
		uid      int
		tempDir  string
		want     string
	}{
		{
			name:     "explicit wins",
			explicit: "/run/user/1000/vision",
			env:      map[string]string{"XDG_RUNTIME_DIR": "/xdg/runtime"},
			uid:      1000,
			tempDir:  t.TempDir(),
			want:     "/run/user/1000/vision",
		},
		{
			name:    "xdg is used when explicit is empty",
			env:     map[string]string{"XDG_RUNTIME_DIR": "/xdg/runtime"},
			uid:     1000,
			tempDir: t.TempDir(),
			want:    "/xdg/runtime/vision/backends",
		},
		{
			name:    "private fallback uses uid and temp dependency",
			env:     map[string]string{},
			uid:     4242,
			tempDir: "/tmp",
			want:    "/tmp/vision-4242/backends",
		},
		{name: "relative explicit rejected", explicit: "relative/root", uid: 1000, tempDir: "/tmp"},
		{name: "relative xdg rejected", env: map[string]string{"XDG_RUNTIME_DIR": "relative"}, uid: 1000, tempDir: "/tmp"},
		{name: "relative temp rejected", env: map[string]string{}, uid: 1000, tempDir: "relative"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RuntimeRoot(tc.explicit, tc.env, tc.uid, tc.tempDir)
			if tc.explicit == "relative/root" || tc.env["XDG_RUNTIME_DIR"] == "relative" || tc.tempDir == "relative" {
				if err == nil {
					t.Fatal("RuntimeRoot() accepted relative path")
				}
				return
			}
			if err != nil {
				t.Fatalf("RuntimeRoot() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("RuntimeRoot() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewStoreRejectsExistingNonPrivateRootWithoutChangingMode(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(root); err == nil {
		t.Fatal("accepted group-writable root")
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o775 {
		t.Fatalf("mode changed to %o", got)
	}
}

func TestLeaseStoreIsPrivateAtomicAndGenerationSafe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("lease directory mode = %o, want 700", got)
	}

	rawOldToken := "owner-token-that-must-not-persist"
	old := Lease{
		Version:        LeaseSchemaVersion,
		Generation:     7,
		ServerName:     "browser/playwright",
		DaemonID:       "daemon-a",
		OwnerTokenHash: TokenHash(rawOldToken),
		ConfigHash:     strings.Repeat("a", 64),
		LeaderPID:      101,
		LeaderPGID:     101,
		LeaderStart:    500,
		BootID:         "boot-a",
		Executable:     "/vision/browser",
		CreatedAt:      time.Unix(10, 0),
	}
	if err := store.Record("browser/playwright", old); err != nil {
		t.Fatal(err)
	}

	path, err := store.Path("browser/playwright")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lease mode = %o, want 600", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), rawOldToken) {
		t.Fatalf("persisted lease contains raw owner token")
	}
	var decoded Lease
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("persisted lease is not valid JSON: %v", err)
	}
	got, err := store.Read("browser/playwright")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != old.Generation || got.OwnerTokenHash != old.OwnerTokenHash {
		t.Fatalf("Read() = %#v, want generation/hash from recorded lease", got)
	}
	unknownFields := map[string]any{}
	if err := json.Unmarshal(raw, &unknownFields); err != nil {
		t.Fatal(err)
	}
	unknownFields["owner_token"] = "must-not-appear-in-error"
	injected, err := json.Marshal(unknownFields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, injected, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read("browser/playwright"); err == nil || strings.Contains(err.Error(), "must-not-appear-in-error") {
		t.Fatalf("injected lease result = %v", err)
	}
	if err := store.Record("browser/playwright", old); err != nil {
		t.Fatal(err)
	}

	newLease := old
	newLease.Generation = old.Generation + 1
	rawNewToken := "new-owner-token"
	newLease.OwnerTokenHash = TokenHash(rawNewToken)
	if err := store.Record("browser/playwright", newLease); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseGeneration("browser/playwright", old.Generation); err == nil {
		t.Fatal("ReleaseGeneration(old generation) error = nil, want stale-generation rejection")
	}
	got, err = store.Read("browser/playwright")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != newLease.Generation {
		t.Fatalf("stale release deleted new generation %d", got.Generation)
	}
	if err := store.ReleaseGeneration("browser/playwright", newLease.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read("browser/playwright"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read() after current release error = %v, want not-exist", err)
	}
}

func TestLeaseStoreRejectsUnsafeNamesAndSymlinkTargets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{Version: LeaseSchemaVersion, Generation: 1, DaemonID: "daemon-a"}
	for _, name := range []string{"", "\x00bad"} {
		if err := store.Record(name, lease); err == nil {
			t.Errorf("Record(%q) error = nil, want filename rejection", name)
		}
	}
	target := filepath.Join(root, "sentinel")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := store.Path("browser/playwright")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := store.Record("browser/playwright", Lease{Version: LeaseSchemaVersion, Generation: 1, DaemonID: "daemon-a"}); err == nil {
		t.Fatal("Record() through symlink error = nil, want rejection")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "keep" {
		t.Fatalf("symlink target bytes = %q, want unchanged", contents)
	}

	unsafeRoot := t.TempDir()
	if err := os.Chmod(unsafeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(unsafeRoot); err == nil {
		t.Fatal("NewStore(unsafe mode) error = nil, want rejection")
	}
	unsafeInfo, err := os.Stat(unsafeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if unsafeInfo.Mode().Perm() != 0o755 {
		t.Fatalf("unsafe root changed to %o", unsafeInfo.Mode().Perm())
	}
	left, err := store.Path("a/b")
	if err != nil {
		t.Fatal(err)
	}
	right, err := store.Path("a-b")
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("server-name encoding collision")
	}
	if _, err := store.Path(strings.Repeat("x", 186)); err != nil {
		t.Fatalf("boundary name rejected: %v", err)
	}
	if _, err := store.Path(strings.Repeat("x", 187)); err == nil {
		t.Fatal("overlong encoded filename accepted")
	}
	validLease := lease
	validLease.ServerName = "hash-test"
	validLease.OwnerTokenHash = strings.Repeat("a", 64)
	validLease.ConfigHash = strings.Repeat("b", 64)
	validLease.Executable = "/bin/true"
	validLease.CreatedAt = time.Unix(1, 0)
	for _, bad := range []Lease{
		func() Lease { v := validLease; v.OwnerTokenHash = strings.Repeat("A", 64); return v }(),
		func() Lease { v := validLease; v.ConfigHash = "not-a-hash"; return v }(),
	} {
		if err := store.Record("hash-test", bad); err == nil {
			t.Error("Record() accepted malformed hash")
		}
	}
	rootLink := filepath.Join(t.TempDir(), "runtime-link")
	if err := os.Symlink(t.TempDir(), rootLink); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(rootLink); err == nil {
		t.Fatal("NewStore(symlink root) error = nil, want rejection")
	}
}

func TestOwnerTokenAndConfigHashContracts(t *testing.T) {
	t.Parallel()

	first, err := GenerateOwnerToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateOwnerToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 32 || first == second || strings.ContainsAny(first, "/+ =") {
		t.Fatalf("GenerateOwnerToken() produced weak/non-URL-safe token %q", first)
	}
	firstHash, secondHash := TokenHash(first), TokenHash(second)
	if firstHash == "" || firstHash == secondHash {
		t.Fatal("TokenHash is not deterministic and collision-resistant for generated tokens")
	}

	identity := ServerIdentity{
		Name:      "playwright",
		Command:   "/usr/bin/browser",
		Args:      []string{"--port", "16287"},
		URL:       "http://127.0.0.1:16287/mcp",
		Transport: "managed-http",
		Env:       map[string]string{"TZ": "UTC"},
	}
	mapOrder := identity
	mapOrder.Env = map[string]string{"LANG": "C", "TZ": "UTC"}
	identityHash, mapOrderHash := ConfigHash(identity), ConfigHash(mapOrder)
	if identityHash == "" || identityHash != mapOrderHash {
		t.Fatal("ConfigHash is not deterministic")
	}
	for _, mutate := range []func(*ServerIdentity){
		func(v *ServerIdentity) { v.Command = "/usr/bin/other" },
		func(v *ServerIdentity) { v.Args = []string{"--port", "16288"} },
		func(v *ServerIdentity) { v.URL = "http://127.0.0.1:16288/mcp" },
		func(v *ServerIdentity) { v.Transport = "http" },
	} {
		changed := identity
		mutate(&changed)
		if ConfigHash(identity) == ConfigHash(changed) {
			t.Errorf("ConfigHash did not change for identity mutation: %#v", changed)
		}
	}
}

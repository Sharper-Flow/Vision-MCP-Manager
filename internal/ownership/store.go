package ownership

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const leaseFileSuffix = ".lease"

// Store is a private, atomic lease store rooted at one same-user-private
// runtime directory. NewStore validates that security boundary; this package
// deliberately does not broaden it with openat/TOCTOU mechanisms.
type Store struct {
	root string
	mu   sync.Mutex
}

// LeaseRecord is one directory entry returned by List.  Err is intentionally
// associated with the name so one malformed lease cannot hide other leases.
// It never contains the file's raw contents.
type LeaseRecord struct {
	ServerName string
	Lease      Lease
	Err        error
}

// NewStore creates or validates root. Existing roots must be owned by the
// current uid, must not be symlinks, and must not grant group/world access.
func NewStore(root string) (*Store, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("runtime root must be absolute")
	}
	root = filepath.Clean(root)
	if err := ensurePrivateRoot(root); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// ensurePrivateRoot validates every component and creates missing components
// one at a time. The nearest existing ancestor may be a normal system parent
// such as /tmp; the lease root and every newly-created component are private.
func ensurePrivateRoot(root string) error {
	root = filepath.Clean(root)
	components := make([]string, 0)
	for path := root; ; path = filepath.Dir(path) {
		components = append(components, path)
		if path == filepath.Dir(path) {
			break
		}
	}
	for _, path := range components {
		if _, err := os.Lstat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect runtime path: %w", err)
		}
	}
	for i := len(components) - 1; i >= 0; i-- {
		path := components[i]
		info, err := os.Lstat(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect runtime path: %w", err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				return fmt.Errorf("create runtime path: %w", err)
			}
			info, err = os.Lstat(path)
			if err != nil {
				return fmt.Errorf("verify runtime path: %w", err)
			}
			if !ownerMatches(info) || info.Mode().Perm() != 0o700 {
				return fmt.Errorf("created runtime path is unsafe")
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("runtime path is unsafe")
		}
		if path == root {
			if !ownerMatches(info) || info.Mode().Perm() != 0o700 {
				return fmt.Errorf("private runtime component is unsafe")
			}
		}
	}
	return nil
}

// Root returns the validated filesystem root.
func (s *Store) Root() string { return s.root }

// Path returns the deterministic flat lease path for serverName.
func (s *Store) Path(serverName string) (string, error) {
	name, err := sanitizeServerName(serverName)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, name+leaseFileSuffix), nil
}

// Record atomically replaces a lease after validating its identity and target.
func (s *Store) Record(serverName string, lease Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.Path(serverName)
	if err != nil {
		return err
	}
	if lease.ServerName == "" {
		lease.ServerName = serverName
	}
	if lease.ServerName != serverName {
		return fmt.Errorf("lease server name mismatch")
	}
	if err := validateLease(lease); err != nil {
		return err
	}
	return s.atomicWrite(path, lease)
}

// Read returns a validated lease and rejects unsafe filesystem objects.
func (s *Store) Read(serverName string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.Path(serverName)
	if err != nil {
		return Lease{}, err
	}
	return s.readPath(path, serverName)
}

// List returns every lease file in deterministic server-name order. Invalid
// entries are represented by a per-entry error and do not prevent valid
// entries from being returned.
func (s *Store) List() ([]LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	result := make([]LeaseRecord, 0)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), leaseFileSuffix) || strings.HasPrefix(entry.Name(), ".lease-") {
			continue
		}
		encoded := strings.TrimSuffix(entry.Name(), leaseFileSuffix)
		nameBytes, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			result = append(result, LeaseRecord{ServerName: fmt.Sprintf("invalid-lease-%04d", len(result)+1), Err: fmt.Errorf("invalid lease file")})
			continue
		}
		name := string(nameBytes)
		record := LeaseRecord{ServerName: name}
		lease, readErr := s.readPath(filepath.Join(s.root, entry.Name()), name)
		if readErr != nil {
			record.Err = readErr
		} else {
			record.Lease = lease
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ServerName < result[j].ServerName })
	return result, nil
}

// ReleaseGeneration removes a lease only when its current generation still
// equals generation. The store mutex protects all operations in this process;
// the second identity read immediately before unlink avoids deleting a newer
// generation observed between the initial read and removal. This package
// assumes one daemon owns a store; cross-process CAS requires a filesystem
// lock or unlink-by-inode primitive and is outside this pure package.
func (s *Store) ReleaseGeneration(serverName string, generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.Path(serverName)
	if err != nil {
		return err
	}
	lease, err := s.readPath(path, serverName)
	if err != nil {
		return err
	}
	if lease.Generation != generation {
		return fmt.Errorf("lease generation is stale")
	}
	latest, err := s.readPath(path, serverName)
	if err != nil {
		return err
	}
	if latest.Generation != generation || latest.OwnerTokenHash != lease.OwnerTokenHash || latest.ConfigHash != lease.ConfigHash {
		return fmt.Errorf("lease identity changed")
	}
	if err := rejectExistingTarget(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove lease: %w", err)
	}
	return syncDir(s.root)
}

func (s *Store) atomicWrite(path string, lease Lease) error {
	data, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("marshal lease: %w", err)
	}
	if err := rejectExistingTarget(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(s.root, ".lease-*")
	if err != nil {
		return fmt.Errorf("create lease temporary file: %w", err)
	}
	tmpName := tmp.Name()
	clean := true
	defer func() {
		if clean {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set lease mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write lease: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync lease: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close lease: %w", err)
	}
	if err := rejectExistingTarget(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install lease: %w", err)
	}
	clean = false
	if err := syncDir(s.root); err != nil {
		return fmt.Errorf("sync lease directory: %w", err)
	}
	return nil
}

func (s *Store) readPath(path, serverName string) (Lease, error) {
	if err := validateLeaseTarget(path); err != nil {
		return Lease{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Lease{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return Lease{}, fmt.Errorf("read lease: %w", err)
	}
	var lease Lease
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lease); err != nil {
		return Lease{}, fmt.Errorf("invalid lease")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Lease{}, fmt.Errorf("invalid lease")
	}
	if err := validateLease(lease); err != nil {
		return Lease{}, err
	}
	if lease.ServerName != serverName {
		return Lease{}, fmt.Errorf("lease server name mismatch")
	}
	return lease, nil
}

func validateLease(lease Lease) error {
	if lease.Version != LeaseSchemaVersion || lease.Generation == 0 || lease.ServerName == "" || len(lease.ServerName) > 1024 || strings.ContainsRune(lease.ServerName, '\x00') || lease.DaemonID == "" || len(lease.DaemonID) > 128 || lease.OwnerTokenHash == "" || lease.ConfigHash == "" || lease.LeaderPID <= 0 || lease.LeaderPGID <= 0 || lease.LeaderStart == 0 || lease.BootID == "" || lease.Executable == "" || lease.CreatedAt.IsZero() {
		return fmt.Errorf("invalid lease")
	}
	if !validHash(lease.OwnerTokenHash) || !validHash(lease.ConfigHash) {
		return fmt.Errorf("invalid lease identity")
	}
	return nil
}

func sanitizeServerName(name string) (string, error) {
	if name == "" || len(name) > 1024 || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("invalid server name")
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(name))
	if len(encoded)+len(leaseFileSuffix) > 255 {
		return "", fmt.Errorf("invalid server name")
	}
	return encoded, nil
}

func validHash(value string) bool {
	if len(value) != sha256HexLength {
		return false
	}
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

const sha256HexLength = 64

func rejectExistingTarget(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lease target is a symlink")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("lease target is not a regular file")
	}
	return nil
}

func validateLeaseTarget(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe lease target")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("unsafe lease mode")
	}
	if !ownerMatches(info) {
		return fmt.Errorf("unsafe lease owner")
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

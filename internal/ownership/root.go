package ownership

import (
	"fmt"
	"os"
	"path/filepath"
)

// RuntimeRoot resolves the private runtime directory without consulting the
// process environment implicitly. The supplied env map makes precedence
// explicit and deterministic for callers and tests.
func RuntimeRoot(explicit string, env map[string]string, uid int, tempDir string) (string, error) {
	if uid < 0 {
		return "", fmt.Errorf("invalid runtime uid")
	}
	if explicit != "" {
		if !filepath.IsAbs(explicit) {
			return "", fmt.Errorf("runtime root must be absolute")
		}
		return filepath.Clean(explicit), nil
	}
	if base := env["XDG_RUNTIME_DIR"]; base != "" {
		if !filepath.IsAbs(base) {
			return "", fmt.Errorf("runtime base must be absolute")
		}
		return filepath.Join(base, "vision", "backends"), nil
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if !filepath.IsAbs(tempDir) {
		return "", fmt.Errorf("temporary runtime base must be absolute")
	}
	return filepath.Join(tempDir, fmt.Sprintf("vision-%d", uid), "backends"), nil
}

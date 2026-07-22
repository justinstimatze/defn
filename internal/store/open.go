// Open helpers: DSN routing to the storage backend.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenBackend opens the storage backend whichever DEFN_BACKEND env var
// selects. Since the Dolt backend was retired in Phase 4, "sqlite" is
// the only valid value (also the default); anything else is an error.
// Kept as a switch so a future backend can slot in without touching
// callers.
func OpenBackend(path string) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEFN_BACKEND"))) {
	case "", "sqlite":
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("abs path: %w", err)
		}
		if err := os.MkdirAll(absPath, 0755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
		return OpenSQLite(filepath.Join(absPath, "defn.db"))
	default:
		return nil, fmt.Errorf("unknown DEFN_BACKEND=%q (only \"sqlite\" is supported)", os.Getenv("DEFN_BACKEND"))
	}
}

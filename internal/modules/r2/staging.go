package r2

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// CleanupStagedUploads removes upload staging files left behind by a crashed
// or force-killed process. Normal request paths always clean up after
// themselves, so at startup every remaining staging file is an orphan.
func CleanupStagedUploads(dir string, logger *slog.Logger) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".r2-upload-") {
			continue
		}
		if os.Remove(filepath.Join(dir, entry.Name())) == nil {
			removed++
		}
	}
	if removed > 0 && logger != nil {
		logger.Info("removed stale staged uploads", "count", removed, "dir", dir)
	}
}

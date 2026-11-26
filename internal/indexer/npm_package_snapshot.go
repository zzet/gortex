package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/resolver"
)

// manifestRacyWindow is how long after a manifest last changed its size and
// modification time cannot prove it unchanged. A rewrite of the same size
// inside one filesystem timestamp tick keeps both, and some filesystems tick
// as coarsely as 2 s. A stamp taken that soon after the change is checked
// again instead of trusted, as Git does for racily clean index entries.
const manifestRacyWindow = 2 * time.Second

type npmPackageFileStamp struct{ size, modified int64 }

type npmPackageSnapshot struct {
	revision          uint64
	roots, workspaces map[string]string
	files             map[string]npmPackageFileStamp
	stampedAt         time.Time
	lookup            resolver.NpmPackageLookup
}

func npmPackageStamp(file string) npmPackageFileStamp {
	if info, err := os.Stat(file); err == nil {
		return npmPackageFileStamp{info.Size(), info.ModTime().UnixNano()}
	}
	return npmPackageFileStamp{}
}

// unchanged reports whether every watched file still has the stamp the
// snapshot recorded. A file that changed within manifestRacyWindow of the
// recording could have been rewritten with an identical stamp, so it counts
// as changed until a later rebuild records it outside that window.
func (s *npmPackageSnapshot) unchanged() bool {
	racy := s.stampedAt.Add(-manifestRacyWindow).UnixNano()
	for file, stamp := range s.files {
		if stamp.modified >= racy || npmPackageStamp(file) != stamp {
			return false
		}
	}
	return true
}

// Watch every directory prefix of each supported workspace glob. New members,
// including package.json added to an existing empty directory, invalidate it.
// Glob expansion runs only on a rebuild, never on an unchanged resolver pass.
func watchNpmWorkspacePaths(root string, patterns []string, watch func(string)) {
	for _, pattern := range patterns {
		if filepath.IsAbs(pattern) || hasDotDotSegment(pattern) {
			continue
		}
		parts := strings.Split(filepath.ToSlash(strings.TrimSuffix(pattern, "/")), "/")
		for depth := 1; depth <= len(parts); depth++ {
			prefix := filepath.Join(root, filepath.FromSlash(strings.Join(parts[:depth], "/")))
			watch(prefix)
			matches, _ := filepath.Glob(prefix)
			for _, dir := range matches {
				info, err := os.Stat(dir)
				if err != nil || !info.IsDir() {
					continue
				}
				watch(dir)
				if depth == len(parts) {
					watch(filepath.Join(dir, "package.json"))
				}
			}
		}
	}
}

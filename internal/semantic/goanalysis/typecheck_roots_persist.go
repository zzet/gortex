package goanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/platform"
)

// Persisted recent handle roots.
//
// The warm-up lists a checkout's recently touched packages right after its
// dirty ones (planWarmupTargets). The recent roots were kept in memory
// only, so a restarted daemon started every checkout with the largest
// packages first. Each change of a module directory's recent roots (a pass
// over roots in a new order, see noteRoots) is written, off the pass's
// path, to one small JSON file per module directory under the daemon's
// cache directory, and read back the first time the warm-up (or a pass)
// asks for that directory's recent roots.

// recentRootsFile is one module directory's persisted recent roots.
type recentRootsFile struct {
	ModuleDir string   `json:"module_dir"`
	Roots     []string `json:"roots"`
}

// SetRecentRootsDir sets the directory the recent handle roots persist in;
// "" disables persistence. Without a call, the daemon's cache directory is
// used (never inside a test binary).
func (p *Provider) SetRecentRootsDir(dir string) {
	if p == nil {
		return
	}
	r := &p.warm
	r.mu.Lock()
	r.rootsDir, r.rootsDirSet = dir, true
	r.mu.Unlock()
}

// rootsDirLocked is the persistence directory; the caller holds r.mu.
func (r *warmupRegistry) rootsDirLocked() string {
	if r.rootsDirSet {
		return r.rootsDir
	}
	if testing.Testing() {
		return ""
	}
	return filepath.Join(platform.CacheDir(), "gotypes-recent-roots")
}

func recentRootsPath(dir, loadDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(loadDir)))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")
}

// loadPersistedRootsLocked reads loadDir's persisted recent roots once and
// appends those not already known (in-memory roots are newer). The caller
// holds r.mu.
func (r *warmupRegistry) loadPersistedRootsLocked(loadDir string) {
	if r.rootsLoaded == nil {
		r.rootsLoaded = map[string]bool{}
	}
	if r.rootsLoaded[loadDir] {
		return
	}
	r.rootsLoaded[loadDir] = true
	dir := r.rootsDirLocked()
	if dir == "" {
		return
	}
	data, err := os.ReadFile(recentRootsPath(dir, loadDir))
	if err != nil {
		return
	}
	var file recentRootsFile
	if json.Unmarshal(data, &file) != nil || filepath.Clean(file.ModuleDir) != filepath.Clean(loadDir) {
		return
	}
	if r.recent == nil {
		r.recent = map[string][]string{}
	}
	cur := r.recent[loadDir]
	seen := make(map[string]bool, len(cur))
	for _, root := range cur {
		seen[root] = true
	}
	for _, root := range file.Roots {
		root = filepath.Clean(root)
		if !seen[root] && len(cur) < warmupRecentRoots {
			seen[root] = true
			cur = append(cur, root)
		}
	}
	r.recent[loadDir] = cur
}

// persistRootsLocked writes loadDir's recent roots in the background; a
// write never replaces a newer one. The caller holds r.mu.
func (r *warmupRegistry) persistRootsLocked(loadDir string, roots []string) {
	dir := r.rootsDirLocked()
	if dir == "" {
		return
	}
	if r.rootsSeq == nil {
		r.rootsSeq = map[string]uint64{}
	}
	r.rootsSeq[loadDir]++
	seq := r.rootsSeq[loadDir]
	data, err := json.Marshal(recentRootsFile{ModuleDir: loadDir, Roots: append([]string(nil), roots...)})
	if err != nil {
		return
	}
	r.persists.Add(1)
	go func() {
		defer r.persists.Done()
		r.persistMu.Lock()
		defer r.persistMu.Unlock()
		if r.persisted == nil {
			r.persisted = map[string]uint64{}
		}
		if r.persisted[loadDir] >= seq {
			return
		}
		if os.MkdirAll(dir, 0o700) != nil {
			return
		}
		path := recentRootsPath(dir, loadDir)
		tmp, err := os.CreateTemp(dir, ".recent-roots-*")
		if err != nil {
			return
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
			_ = os.Remove(tmp.Name())
			return
		}
		r.persisted[loadDir] = seq
	}()
}

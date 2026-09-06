// Package merkle builds a content-addressed Merkle tree of a
// repository for incremental re-index. Each file hashes to BLAKE3 of
// its content; each directory hashes to BLAKE3 of its sorted child
// entries, so a directory's hash changes iff some file under it
// changed. The per-file content hash makes change detection immune to
// the mtime false positives — a touched-but-unchanged file — that the
// bare-mtime path re-indexes needlessly, and the directory hashes
// answer "did anything under X change" in a single comparison.
package merkle

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zeebo/blake3"
)

// FileNode records one file's content hash and the mtime at which it
// was hashed. The mtime gates re-hashing: an unchanged mtime lets Build
// reuse the prior hash without re-reading the file.
type FileNode struct {
	Hash  string `json:"hash"`
	Mtime int64  `json:"mtime"`
	// Salt is an opaque per-file discriminator mixed into the leaf
	// hash. It carries the language extractor version, so a file whose
	// content is unchanged but whose extractor logic was upgraded still
	// diffs as changed and is re-extracted. An empty Salt reproduces the
	// pre-salt leaf exactly, so adopting salts is zero-cost and fully
	// back-compatible until a language's extractor version is bumped.
	Salt string `json:"salt,omitempty"`
}

// leaf is the value mixed into the parent directory's hash for this
// file. An empty Salt yields exactly the content hash, so trees built
// before salting — and files whose extractor is at the baseline
// version — hash identically.
func (n FileNode) leaf() string {
	if n.Salt == "" {
		return n.Hash
	}
	return n.Hash + "\x01" + n.Salt
}

// Tree is a content-addressed Merkle tree of one repository snapshot.
type Tree struct {
	// Root is the BLAKE3 hash of the whole tree — equal across two
	// snapshots iff no indexed file changed.
	Root string `json:"root"`
	// Files maps a forward-slash repo-relative path to its node.
	Files map[string]FileNode `json:"files"`
	// Dirs maps a forward-slash repo-relative directory ("" is the
	// repo root) to the aggregate hash of its subtree.
	Dirs map[string]string `json:"dirs"`
}

// MergeScope combines a rebuilt scope with untouched leaves from the previous
// repository baseline. A nil contains predicate means a full replacement.
// Leaves outside the scope are copied verbatim, including empty hashes and
// old extractor salts: this operation does not read or certify those files.
// Neither input tree is mutated.
func MergeScope(prior, scoped *Tree, contains func(string) bool) *Tree {
	merged := &Tree{
		Files: make(map[string]FileNode),
		Dirs:  make(map[string]string),
	}
	if prior != nil && contains != nil {
		for rel, leaf := range prior.Files {
			if !contains(rel) {
				merged.Files[rel] = leaf
			}
		}
	}
	if scoped != nil {
		for rel, leaf := range scoped.Files {
			if contains == nil || contains(rel) {
				merged.Files[rel] = leaf
			}
		}
	}
	merged.aggregate()
	return merged
}

// Build constructs a Merkle tree for the files (forward-slash repo-
// relative paths) under rootAbs. When prior is non-nil, a file whose
// on-disk mtime matches prior's reuses the prior content hash and is
// not re-read, so a rebuild reads only the mtime-changed files. A file
// that cannot be read is recorded with an empty hash so a diff always
// flags it.
//
// saltFor, when non-nil, returns a per-file salt (e.g. the language
// extractor version) mixed into the leaf hash. A salt change re-flags a
// file as changed even when its content and mtime did not move, so an
// extractor-logic upgrade re-extracts the affected files — without a
// re-read, since the content hash is reused. A nil saltFor (or one
// returning "") leaves every leaf equal to its content hash.
func Build(rootAbs string, relPaths []string, prior *Tree, saltFor func(rel string) string) *Tree {
	return BuildWithKnownFiles(rootAbs, relPaths, prior, saltFor, nil)
}

// BuildWithKnownFiles builds the same tree as Build while accepting content
// identities captured by the caller's authoritative file read. A valid known
// node is used before any stat or read, so a full index can persist the exact
// bytes it parsed without rereading the repository or racing a later mutation.
// Paths absent from knownFiles retain Build's stat/prior/read fallback.
func BuildWithKnownFiles(
	rootAbs string,
	relPaths []string,
	prior *Tree,
	saltFor func(rel string) string,
	knownFiles map[string]FileNode,
) *Tree {
	salt := func(rel string) string {
		if saltFor == nil {
			return ""
		}
		return saltFor(rel)
	}
	t := &Tree{
		Files: make(map[string]FileNode, len(relPaths)),
		Dirs:  make(map[string]string),
	}
	for _, rel := range relPaths {
		rel = filepath.ToSlash(rel)
		if known, ok := knownFiles[rel]; ok && known.Hash != "" {
			known.Salt = salt(rel)
			t.Files[rel] = known
			continue
		}
		abs := filepath.Join(rootAbs, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil {
			t.Files[rel] = FileNode{Salt: salt(rel)} // unreadable — always treated as changed
			continue
		}
		mtime := info.ModTime().UnixNano()
		if prior != nil {
			if pn, ok := prior.Files[rel]; ok && pn.Mtime == mtime && pn.Hash != "" {
				// mtime unchanged — reuse the content hash, skip the read,
				// but re-stamp the salt so an extractor-version bump still
				// surfaces in the leaf.
				t.Files[rel] = FileNode{Hash: pn.Hash, Mtime: mtime, Salt: salt(rel)}
				continue
			}
		}
		h, err := hashFile(abs)
		if err != nil {
			t.Files[rel] = FileNode{Mtime: mtime, Salt: salt(rel)}
			continue
		}
		t.Files[rel] = FileNode{Hash: h, Mtime: mtime, Salt: salt(rel)}
	}
	t.aggregate()
	return t
}

// hashFile streams a file's content through BLAKE3.
func hashFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// aggregate computes every directory's hash bottom-up and sets Root. A
// directory hash is BLAKE3 over its immediate children sorted by name,
// each entry "name\x00<kind><childHash>\n", so it changes iff any
// descendant file's content changed or a file was added or removed.
func (t *Tree) aggregate() {
	files := map[string][]string{}          // dir -> ["name\x00f<hash>", ...]
	subdirs := map[string]map[string]bool{} // dir -> set of immediate child dir names
	allDirs := map[string]bool{"": true}

	markDir := func(d string) {
		for d != "" {
			allDirs[d] = true
			p := parentDir(d)
			if subdirs[p] == nil {
				subdirs[p] = map[string]bool{}
			}
			subdirs[p][baseName(d)] = true
			d = p
		}
	}
	for rel, node := range t.Files {
		d := parentDir(rel)
		markDir(d)
		files[d] = append(files[d], baseName(rel)+"\x00f"+node.leaf())
	}

	// Deepest directories first, so a directory's child-dir hashes are
	// already computed when its own hash is taken.
	dirs := make([]string, 0, len(allDirs))
	for d := range allDirs {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return depth(dirs[i]) > depth(dirs[j]) })

	for _, d := range dirs {
		entries := append([]string(nil), files[d]...)
		for name := range subdirs[d] {
			child := name
			if d != "" {
				child = d + "/" + name
			}
			entries = append(entries, name+"\x00d"+t.Dirs[child])
		}
		sort.Strings(entries)
		h := blake3.New()
		for _, e := range entries {
			_, _ = h.Write([]byte(e))
			_, _ = h.Write([]byte{'\n'})
		}
		t.Dirs[d] = hex.EncodeToString(h.Sum(nil))
	}
	t.Root = t.Dirs[""]
}

// Diff returns the files whose content changed (added or modified) and
// the files removed, relative to prior. When the root hashes match the
// whole comparison is skipped — nothing changed anywhere.
func (t *Tree) Diff(prior *Tree) (changed, removed []string) {
	if prior == nil {
		for rel := range t.Files {
			changed = append(changed, rel)
		}
		sort.Strings(changed)
		return changed, nil
	}
	if t.Root != "" && t.Root == prior.Root {
		return nil, nil
	}
	for rel, node := range t.Files {
		pn, ok := prior.Files[rel]
		if !ok || pn.leaf() != node.leaf() || node.Hash == "" {
			changed = append(changed, rel)
		}
	}
	for rel := range prior.Files {
		if _, ok := t.Files[rel]; !ok {
			removed = append(removed, rel)
		}
	}
	sort.Strings(changed)
	sort.Strings(removed)
	return changed, removed
}

// SubtreeChanged reports whether anything under relDir changed since
// prior — a single hash comparison, no per-file work. relDir is a
// forward-slash repo-relative directory ("" is the whole repo).
func (t *Tree) SubtreeChanged(relDir string, prior *Tree) bool {
	if prior == nil {
		return true
	}
	return t.Dirs[relDir] != prior.Dirs[relDir]
}

// Save writes the tree to path as JSON via a unique temp file +
// rename, so a concurrent or interrupted write never leaves a torn
// file.
func (t *Tree) Save(path string) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".merkle-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a tree previously written by Save. A missing or corrupt
// file yields (nil, nil): the caller treats a nil prior as "rebuild
// from scratch", which is correct, just not incremental.
func Load(path string) (*Tree, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var t Tree
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, nil
	}
	if t.Files == nil {
		t.Files = map[string]FileNode{}
	}
	if t.Dirs == nil {
		t.Dirs = map[string]string{}
	}
	return &t, nil
}

func parentDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func depth(p string) int {
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}

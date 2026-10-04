package indexer

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

// disambiguateBareTypesViaImportsBatch resolves every registry from one
// FindNodesByNames projection. Registry indexes contain value copies and invoke
// the resolver repeatedly; prefetching here prevents those copies—and multiple
// repositories—from multiplying SQLite name queries.
func (mi *MultiIndexer) disambiguateBareTypesViaImportsBatch(registries []*contracts.Registry, g graph.Store) {
	srcCache := map[string][]byte{}
	importCache := map[string]map[string]string{}
	nameSet := map[string]struct{}{}

	// A bare type is resolved through the imports of the file that mentions
	// it — the file defining the contract's symbol. For a route registered in
	// one file and handled in another that is not c.FilePath, which names the
	// registration site, so resolve the symbol's own file first. One batched
	// node lookup, mirroring the FindNodesByNames projection below.
	srcByContract := handlerSourceFiles(registries, g)
	srcFile := func(c contracts.Contract) string {
		if p := srcByContract[c.SymbolID]; p != "" {
			return p
		}
		return c.FilePath
	}

	for _, cr := range registries {
		if cr == nil {
			continue
		}
		for _, c := range cr.All() {
			src := srcFile(c)
			if c.Meta == nil || !isImportResolvableLang(src) {
				continue
			}
			for _, key := range []string{"response_type", "request_type"} {
				name, _ := c.Meta[key].(string)
				if name == "" || strings.Contains(name, "::") {
					continue
				}
				nameSet[name] = struct{}{}
				if isRustFile(src) {
					for _, candidateName := range mi.rustImportCandidateNames(src, name, srcCache) {
						nameSet[candidateName] = struct{}{}
					}
				}
			}
		}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	candidatesByName := g.FindNodesByNames(names)

	for _, cr := range registries {
		if cr == nil {
			continue
		}
		mi.disambiguateBareTypesViaImportsPrefetched(cr, srcCache, importCache, candidatesByName, srcByContract)
	}
}

// handlerSourceFiles maps a contract's SymbolID to the file its symbol is
// defined in. Contracts whose symbol is unknown or absent from the graph are
// omitted; callers fall back to Contract.FilePath for those.
func handlerSourceFiles(registries []*contracts.Registry, g graph.Store) map[string]string {
	idSet := map[string]struct{}{}
	for _, cr := range registries {
		if cr == nil {
			continue
		}
		for _, c := range cr.All() {
			if c.SymbolID != "" {
				idSet[c.SymbolID] = struct{}{}
			}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make(map[string]string, len(ids))
	for id, n := range g.GetNodesByIDs(ids) {
		if n != nil && n.FilePath != "" {
			out[id] = n.FilePath
		}
	}
	return out
}

func (mi *MultiIndexer) disambiguateBareTypesViaImportsPrefetched(
	cr *contracts.Registry,
	srcCache map[string][]byte,
	importCache map[string]map[string]string,
	candidatesByName map[string][]*graph.Node,
	srcByContract map[string]string,
) {
	for _, c := range cr.All() {
		if c.Meta == nil {
			continue
		}
		// The imports to consult are the ones in the file defining this
		// contract's symbol, not the file it was registered in.
		src := c.FilePath
		if p := srcByContract[c.SymbolID]; p != "" {
			src = p
		}
		if !isImportResolvableLang(src) {
			continue
		}
		patched := false
		items := cr.ByID(c.ID)
		for i := range items {
			// FilePath still identifies which registry entry this is —
			// registration site, unchanged.
			if items[i].FilePath != c.FilePath || items[i].Meta == nil {
				continue
			}
			for _, key := range []string{"response_type", "request_type"} {
				name, _ := items[i].Meta[key].(string)
				if name == "" || strings.Contains(name, "::") {
					continue
				}
				resolved := mi.resolveBareTypeViaImportsPrefetched(src, name, srcCache, importCache, candidatesByName)
				if resolved == "" {
					continue
				}
				items[i].Meta[key] = resolved
				patched = true
			}
		}
		if patched {
			cr.ReplaceByID(c.ID, items)
		}
	}
}

// resolveBareTypeViaImports looks up `name` among the bare-type
// candidates in the merged graph and returns the unambiguous match
// reachable via an import statement in `srcFile`. Returns "" when
// the lookup is still ambiguous or no candidate matches an import
// (so the caller leaves the bare name in place).
func (mi *MultiIndexer) resolveBareTypeViaImports(
	srcFile, name string,
	g graph.Store,
	srcCache map[string][]byte,
	importCache map[string]map[string]string,
) string {
	names := []string{name}
	if isRustFile(srcFile) {
		names = append(names, mi.rustImportCandidateNames(srcFile, name, srcCache)...)
	}
	return mi.resolveBareTypeViaImportsPrefetched(srcFile, name, srcCache, importCache, g.FindNodesByNames(names))
}

func (mi *MultiIndexer) resolveBareTypeViaImportsPrefetched(
	srcFile, name string,
	srcCache map[string][]byte,
	importCache map[string]map[string]string,
	candidatesByName map[string][]*graph.Node,
) string {
	if isRustFile(srcFile) {
		src := mi.cachedSource(srcFile, srcCache)
		if len(src) == 0 {
			return ""
		}
		rustFacts, ok := rustImportFactsForName(string(src), srcFile, name)
		if !ok {
			return ""
		}
		return mi.resolveRustUseFactsTargetPrefetched(rustFacts, candidatesByName, srcCache)
	}

	candidates := candidatesByName[name]
	if len(candidates) == 0 {
		return ""
	}
	var typed []*graph.Node
	for _, n := range candidates {
		if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
			typed = append(typed, n)
		}
	}
	if len(typed) < 2 {
		// A single TS candidate would already have been caught by
		// UpgradeBareTypeRefs, so this pass handles ambiguous candidates only.
		return ""
	}

	imports, ok := importCache[srcFile]
	if !ok {
		src := mi.cachedSource(srcFile, srcCache)
		if len(src) == 0 {
			importCache[srcFile] = nil
			return ""
		}
		imports = mi.parseImportsFor(string(src), srcFile)
		importCache[srcFile] = imports
	}
	if len(imports) == 0 {
		return ""
	}
	wantFile, found := imports[name]
	if !found {
		return ""
	}

	// Follow re-export chains while retaining the source leaf through every
	// alias. The follower is depth-bounded and cycle-safe.
	reachable, unsafe := mi.followReExportChainChecked(wantFile, name, srcCache)
	if unsafe {
		return ""
	}
	var hit string
	for _, n := range typed {
		if !reachable[n.FilePath] {
			continue
		}
		if hit != "" && hit != n.ID {
			return ""
		}
		hit = n.ID
	}
	return hit
}

func (mi *MultiIndexer) resolveRustUseFactsTargetPrefetched(
	facts []rustUseFact,
	candidatesByName map[string][]*graph.Node,
	srcCache map[string][]byte,
) string {
	seen := map[string]bool{}
	var hit string
	for _, fact := range facts {
		chain := mi.followReExportChainDetailed(fact.fromFile, fact.sourceName, srcCache)
		if chain.unsafe {
			return ""
		}
		for file, names := range chain.names {
			for name := range names {
				for _, node := range candidatesByName[name] {
					if node == nil || node.FilePath != file || seen[node.ID] {
						continue
					}
					if node.Kind != graph.KindType && node.Kind != graph.KindInterface {
						continue
					}
					seen[node.ID] = true
					if hit != "" && hit != node.ID {
						return ""
					}
					hit = node.ID
				}
			}
		}
	}
	return hit
}

func (mi *MultiIndexer) rustImportCandidateNames(srcFile, name string, srcCache map[string][]byte) []string {
	src := mi.cachedSource(srcFile, srcCache)
	if len(src) == 0 {
		return nil
	}
	facts, ok := rustImportFactsForName(string(src), srcFile, name)
	if !ok {
		return nil
	}
	set := map[string]struct{}{}
	for _, fact := range facts {
		chain := mi.followReExportChainDetailed(fact.fromFile, fact.sourceName, srcCache)
		for _, names := range chain.names {
			for candidateName := range names {
				set[candidateName] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for candidateName := range set {
		out = append(out, candidateName)
	}
	return out
}

// tsAliasCache caches the per-repo Collection of tsconfig/jsconfig
// alias maps. Loaded lazily on first lookup for a repo ROOT PATH and
// reused across all import resolutions in the same session. A nil
// entry means "scanned, no usable config" — distinct from "not yet
// scanned" (missing key).
//
// It holds WORKING-COPY answers only. The key is a root, and a root cannot
// tell two snapshots of one checkout apart, so a build that reads a committed
// tree neither consults this cache nor fills it: it loads its own scopes out
// of its own source and memoises them on the Indexer that owns that source
// (Indexer.tsAliasCollection). Sharing this map between the live index and a
// committed build is exactly how the checkout's tsconfig would have decided a
// committed generation's alias edges.
var (
	tsAliasCache   = map[string]*tsalias.Collection{}
	tsAliasCacheMu sync.Mutex
)

// tsAliasMapFor returns the nearest-ancestor alias map for srcFile
// (and the repo prefix the resolved path should be prefixed with).
// srcFile is a repo-prefixed path; we determine the repo by matching
// it against tracked repos, walk that repo's filesystem root for
// config files (cached), and pick the scope nearest to srcFile.
//
// Returns (nil, "") when the repo can't be located or no usable
// config exists — callers must handle that as "no alias resolution
// available, fall through to bare-name behaviour."
func (mi *MultiIndexer) tsAliasMapFor(srcFile string) (*tsalias.Map, string) {
	if srcFile == "" {
		return nil, ""
	}
	for _, m := range mi.AllMetadata() {
		prefix := m.RepoPrefix
		// A tracked repo always carries a prefix; a metadata entry without
		// one is malformed rather than a lone-repo special case, so it
		// cannot claim srcFile. Nor can a repo whose prefix does not lead
		// the path.
		if prefix == "" || !strings.HasPrefix(srcFile, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(srcFile, prefix+"/")
		coll := mi.tsAliasCollectionFor(prefix, m.RootPath)
		if coll == nil {
			return nil, prefix
		}
		return coll.FindForFile(path.Dir(rel)), prefix
	}
	return nil, ""
}

// tsAliasCollectionFor returns the tsconfig / jsconfig alias scopes a tracked
// repository's path-alias lookups must go through.
//
// The scopes come from the tracked-repo metadata's root, which is where this
// scan always read: only a registered indexer that has declared SOURCE
// authority takes over, and then through that indexer's own memo, because a
// snapshot's scopes must neither be looked up in nor written to the
// process-wide root-keyed cache. An indexer with no source installed reads the
// same working copy the metadata root names, so deferring to it would swap the
// root for no gain — the same preference readFileFromAnyRepo's manifestTreeFor
// applies, for the same reason.
func (mi *MultiIndexer) tsAliasCollectionFor(prefix, root string) *tsalias.Collection {
	// GetIndexer holds mi.mu only for this lookup; no I/O happens under it.
	if idx := mi.GetIndexer(prefix); idx != nil {
		if tree := idx.manifestTree(); tree.sourced() {
			return idx.tsAliasCollection()
		}
	}
	return loadTSAliasCollection(root)
}

// loadTSAliasCollection returns the WORKING COPY's alias scopes at rootPath,
// cached process-wide by root. Only an unsourced tree may ask: see the note on
// tsAliasCache.
func loadTSAliasCollection(rootPath string) *tsalias.Collection {
	tsAliasCacheMu.Lock()
	defer tsAliasCacheMu.Unlock()
	if c, ok := tsAliasCache[rootPath]; ok {
		return c
	}
	c := tsalias.Load(rootPath)
	tsAliasCache[rootPath] = c
	return c
}

// tsAliasCollectionForTree loads the alias scopes out of one tree.
//
// A working-copy tree keeps the process-wide root-keyed cache it has always
// used. A sourced tree is loaded through tsalias.LoadTree over the snapshot
// itself — every config file it holds, read out of it, with its multi-target
// aliases grounded in its own bytes — and never touches that cache. A sourced
// tree that cannot enumerate answers "no scopes" rather than falling back to
// the checkout: a build that declared source authority must not read the
// working copy, and a wrong alias scope is a wrong EDGE, not a missing one.
func tsAliasCollectionForTree(tree manifestTree) *tsalias.Collection {
	if !tree.sourced() {
		return loadTSAliasCollection(tree.root())
	}
	if src, ok := tree.(tsalias.Tree); ok {
		return tsalias.LoadTree(src)
	}
	return nil
}

// readFileFromAnyRepo finds the bytes for a repo-prefixed file path by
// walking tracked-repo metadata. Mirrors readNodeSource but takes the path
// directly so callers don't need a graph node.
//
// The read goes through the owning repository's manifest tree, so a repo
// whose indexer is reading a snapshot is read out of that snapshot rather
// than off its checkout.
func (mi *MultiIndexer) readFileFromAnyRepo(filePath string) ([]byte, bool) {
	if filePath == "" {
		return nil, false
	}
	for _, m := range mi.AllMetadata() {
		prefix := m.RepoPrefix
		if prefix == "" || !strings.HasPrefix(filePath, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(filePath, prefix+"/")
		data, ok := mi.manifestTreeFor(prefix, m.RootPath).readFile(rel)
		if ok {
			return data, true
		}
	}
	return nil, false
}

// manifestTreeFor returns the tree a tracked repository's out-of-payload
// reads must go through.
//
// The read root stays the tracked-repo metadata's, which is where this read
// always came from: only a registered indexer that has declared SOURCE
// authority takes over, because that is the one case where reading the
// working copy would hand a committed build the checkout's bytes. An indexer
// with no source installed reads the same working copy as the metadata root,
// so deferring to it would swap the root for no gain — and the two roots are
// only guaranteed to agree because every registration site derives both from
// one filepath.Abs. Preferring the metadata root keeps that coincidence from
// being load-bearing.
func (mi *MultiIndexer) manifestTreeFor(prefix, root string) manifestTree {
	// GetIndexer holds mi.mu only for this lookup; no I/O happens under it.
	if idx := mi.GetIndexer(prefix); idx != nil {
		if tree := idx.manifestTree(); tree.sourced() {
			return tree
		}
	}
	return newDiskManifestTree(root)
}

// joinPath joins a root and relative path with a single separator. It
// stays a string operation rather than a filepath.Join: the relative half
// is a repo-relative slash path addressed the way a content source keys
// its entries, and cleaning it here would fold a name a snapshot holds
// verbatim.
func joinPath(root, rel string) string {
	if root == "" {
		return rel
	}
	if strings.HasSuffix(root, "/") {
		return root + rel
	}
	return root + "/" + rel
}

// readDiskFile is a small indirection so tests can swap in an
// in-memory fixture without touching the on-disk reader.
var readDiskFile = func(absPath string) ([]byte, bool) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, false
	}
	return data, true
}

// manifestTree serves the build-configuration files the resolver reads
// outside the indexed file set — compile_commands.json, package.json,
// tsconfig.json / jsconfig.json, and the directory probes that go with them —
// for one repository root.
//
// These reads are the last place a build can pick up bytes that do not
// belong to the tree it describes. A generation built from a committed tree
// parses its payload through the installed content source, but a compile
// database or a package manifest read straight off the checkout describes
// whatever the working copy happens to hold at that instant, and an edge
// derived from it is then not reproducible from the tree the generation
// claims. A tree backed by a source answers out of that snapshot and never
// falls back to the working copy: a path the source cannot serve is absent,
// not looked up on disk.
type manifestTree interface {
	// root is the repository root the tree is addressed against. A compile
	// database records absolute paths, so normalising them still needs it.
	root() string
	// sourced reports whether a content source backs the tree, which is what
	// decides whether a process-wide, root-keyed cache may hold an answer.
	sourced() bool
	// readFile returns the content at a repo-relative slash path.
	readFile(rel string) ([]byte, bool)
	// isFile reports whether a repo-relative slash path is readable content.
	isFile(rel string) bool
	// isDir reports whether a repo-relative slash path is a directory, FOLLOWING
	// a symlink on the working copy. It is deliberately not answerable from
	// topLevelDirs: that probe reports what a directory listing calls a
	// directory (a symlink is a symlink), and the conventional-include-root
	// question has always been "does this name resolve to a directory", which
	// a symlinked include/ answers yes.
	isDir(rel string) bool
	// matchFiles expands a repo-relative slash glob to the files it matches,
	// in lexicographic order.
	matchFiles(glob string) []string
	// matchDirs expands a repo-relative slash glob to the directories it
	// matches, in lexicographic order.
	matchDirs(glob string) []string
	// topLevelDirs returns the tree's top-level directories, each mapped to
	// whether it DIRECTLY holds a file with one of the given extensions.
	// Directories whose name starts with a dot are not reported. Presence and
	// contents are answered together because a source can only answer either
	// by enumerating, and one enumeration per index pass is the budget.
	topLevelDirs(exts ...string) map[string]bool
}

// diskManifestTree reads the working copy at a root. It is what the live
// index uses, and what a build with no content source installed falls back
// to, so its answers are byte-for-byte the ones these readers gave before a
// tree stood between them and the os package.
type diskManifestTree struct{ rootPath string }

var _ manifestTree = diskManifestTree{}

// newDiskManifestTree returns the working-copy tree at root. An empty root
// answers nothing rather than resolving against the process CWD.
func newDiskManifestTree(root string) diskManifestTree {
	return diskManifestTree{rootPath: root}
}

func (t diskManifestTree) root() string  { return t.rootPath }
func (t diskManifestTree) sourced() bool { return false }

func (t diskManifestTree) readFile(rel string) ([]byte, bool) {
	if t.rootPath == "" || rel == "" {
		return nil, false
	}
	return readDiskFile(joinPath(t.rootPath, rel))
}

func (t diskManifestTree) isFile(rel string) bool {
	if t.rootPath == "" || rel == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(t.rootPath, filepath.FromSlash(rel)))
	return err == nil && !fi.IsDir()
}

// isDir is os.Stat, so it follows a symlink, and it needs no listing of the
// parent — byte-for-byte the probe the conventional-include-root clause used
// before a tree stood in front of it. A root that cannot be listed at all
// (readable children, unlistable directory) still answers here.
func (t diskManifestTree) isDir(rel string) bool {
	if t.rootPath == "" || rel == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(t.rootPath, filepath.FromSlash(rel)))
	return err == nil && fi.IsDir()
}

func (t diskManifestTree) matchFiles(glob string) []string { return t.match(glob, false) }
func (t diskManifestTree) matchDirs(glob string) []string  { return t.match(glob, true) }

// match expands a slash glob against the working copy, keeping only the
// matches whose kind — file or directory — the caller asked for.
func (t diskManifestTree) match(glob string, wantDir bool) []string {
	if t.rootPath == "" || glob == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(t.rootPath, filepath.FromSlash(glob)))
	if err != nil {
		return nil
	}
	var out []string
	for _, abs := range matches {
		fi, statErr := os.Stat(abs)
		if statErr != nil || fi.IsDir() != wantDir {
			continue
		}
		rel, relErr := filepath.Rel(t.rootPath, abs)
		if relErr != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

func (t diskManifestTree) topLevelDirs(exts ...string) map[string]bool {
	if t.rootPath == "" {
		return nil
	}
	entries, err := os.ReadDir(t.rootPath)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		out[name] = dirHoldsExt(filepath.Join(t.rootPath, name), exts)
	}
	return out
}

// sourceManifestTree reads one immutable snapshot through a content source.
// rootPath is carried for path normalisation only — no answer of this tree
// touches the working copy, and a nil source answers nothing, because a
// build that declared source authority must not silently fall back to disk.
type sourceManifestTree struct {
	rootPath string
	src      source.ContentSource
}

var (
	_ manifestTree = sourceManifestTree{}
	_ tsalias.Tree = sourceManifestTree{}
)

func (t sourceManifestTree) root() string  { return t.rootPath }
func (t sourceManifestTree) sourced() bool { return true }

// Files, Read and Exists are the tsalias.Tree view of the snapshot: they let
// the tsconfig / jsconfig path-alias scan read the tree the generation
// describes instead of walking the checkout. Files is the one probe here that
// costs a whole walk, so its caller memoises the Collection it builds.
//
// A snapshot has no directories to prune, so it offers every path and lets
// the loader apply the skip rule (tsalias.LoadTree does, to every path it is
// offered) — which is what keeps the two trees agreeing about which configs a
// repository HAS. A symlink is skipped for the reason isFile gives: a source
// hands back link text, not the file's bytes.
func (t sourceManifestTree) Files(visit func(rel string)) {
	if t.src == nil || visit == nil {
		return
	}
	t.scan("", func(meta source.FileMeta) {
		if meta.Symlink {
			return
		}
		visit(meta.Path)
	})
}

func (t sourceManifestTree) Read(rel string) ([]byte, bool) { return t.readFile(rel) }
func (t sourceManifestTree) Exists(rel string) bool         { return t.isFile(rel) }

func (t sourceManifestTree) readFile(rel string) ([]byte, bool) {
	if t.src == nil || rel == "" {
		return nil, false
	}
	data, _, err := readSourceFile(t.src, rel)
	if err != nil {
		return nil, false
	}
	return data, true
}

// isFile reports content the snapshot can serve. A symlink is not content:
// a source hands back the link text rather than following it, so treating
// one as a file would offer a reader bytes that are not the file's.
func (t sourceManifestTree) isFile(rel string) bool {
	if t.src == nil || rel == "" {
		return false
	}
	meta, err := t.src.Stat(rel)
	return err == nil && !meta.Symlink
}

// isDir reports whether the snapshot holds anything under rel. A source has no
// directory entries of its own — a directory exists exactly when something in
// it does, which is git's rule and the one matchDirs already applies. A
// symlink is therefore never a directory here: a source hands back its link
// text as a blob and never descends it, so nothing is ever under it.
func (t sourceManifestTree) isDir(rel string) bool {
	if t.src == nil || rel == "" {
		return false
	}
	found := false
	t.scan(rel+"/", func(source.FileMeta) { found = true })
	return found
}

// matchFiles keeps only entries the snapshot can serve as content, which is
// the same question isFile answers: a symlink's bytes are its link text, so
// matching one would hand its reader something that is not the file. Without
// this the two probes disagree and a symlinked build/compile_commands.json is
// matched, read as a path string and silently dropped by the JSON decode.
func (t sourceManifestTree) matchFiles(glob string) []string {
	if glob == "" {
		return nil
	}
	var out []string
	t.scan(globLiteralPrefix(glob), func(meta source.FileMeta) {
		if meta.Symlink {
			return
		}
		if ok, err := path.Match(glob, meta.Path); ok && err == nil {
			out = append(out, meta.Path)
		}
	})
	sort.Strings(out)
	return out
}

// matchDirs matches the ancestor directories of the snapshot's entries. A
// directory exists in a source exactly when something in it does, which is
// also git's rule, so an empty directory is not a match here or on disk.
func (t sourceManifestTree) matchDirs(glob string) []string {
	if glob == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	t.scan(globLiteralPrefix(glob), func(meta source.FileMeta) {
		for dir := path.Dir(meta.Path); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			if _, done := seen[dir]; done {
				// Every ancestor of a directory already considered has
				// itself already been considered.
				break
			}
			seen[dir] = struct{}{}
			if ok, err := path.Match(glob, dir); ok && err == nil {
				out = append(out, dir)
			}
		}
	})
	sort.Strings(out)
	return out
}

func (t sourceManifestTree) topLevelDirs(exts ...string) map[string]bool {
	want := make(map[string]struct{}, len(exts))
	for _, e := range exts {
		want[e] = struct{}{}
	}
	out := map[string]bool{}
	t.scan("", func(meta source.FileMeta) {
		p := meta.Path
		slash := strings.Index(p, "/")
		if slash <= 0 {
			return // a file at the root names no directory
		}
		top := p[:slash]
		if strings.HasPrefix(top, ".") {
			return
		}
		if _, present := out[top]; !present {
			out[top] = false
		}
		// Only a DIRECT child counts, which is the shape the working-copy
		// probe reads with one ReadDir per top-level entry.
		if strings.Contains(p[slash+1:], "/") {
			return
		}
		if _, ok := want[path.Ext(p)]; ok {
			out[top] = true
		}
	})
	return out
}

// errManifestScanDone ends a scan that has passed every path its glob's
// literal prefix could still match. It never escapes scan.
var errManifestScanDone = errors.New("indexer: manifest scan complete")

// scan visits the snapshot's paths in lexicographic order and stops at the
// first path that has left lit behind. Walk's ordering is part of the
// ContentSource contract, so a scan for `build*/compile_commands.json` reads
// the "build" block of a tree and nothing after it; an empty lit is a whole
// walk, which only the top-level directory probe asks for.
func (t sourceManifestTree) scan(lit string, visit func(source.FileMeta)) {
	if t.src == nil {
		return
	}
	_ = t.src.Walk(context.Background(), func(meta source.FileMeta) error {
		p := meta.Path
		if lit != "" && !strings.HasPrefix(p, lit) {
			if p > lit {
				return errManifestScanDone
			}
			return nil
		}
		visit(meta)
		return nil
	})
}

// globLiteralPrefix returns the leading portion of a slash glob that no
// metacharacter can alter — the prefix every match is guaranteed to carry.
func globLiteralPrefix(glob string) string {
	if i := strings.IndexAny(glob, `*?[\`); i >= 0 {
		return glob[:i]
	}
	return glob
}

// dirHoldsExt reports whether dir directly holds a file with one of the
// given extensions.
func dirHoldsExt(dir string, exts []string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		for _, want := range exts {
			if ext == want {
				return true
			}
		}
	}
	return false
}

// tsImportRe matches `import { A, B as C } from '...'`,
// `import type { A } from '...'`, `import A from '...'`, and
// `import * as A from '...'`. Capture groups:
//
//	1: named-import body (between `{` and `}`) — empty for default /
//	   namespace imports, in which case group 4 carries the bound
//	   name.
//	2: default / namespace identifier (the bare ident or `* as X`)
//	3: module path
var tsImportRe = regexp.MustCompile(
	`(?m)^\s*import\s+(?:type\s+)?(?:\{([^}]*)\}|([A-Za-z_$][\w$]*|\*\s+as\s+[A-Za-z_$][\w$]*))(?:\s*,\s*\{([^}]*)\})?\s+from\s+['"]([^'"]+)['"]`,
)

// parseTSImports walks the import lines of a TypeScript / JavaScript
// source file and returns name → absolute repo-prefixed file path.
// `srcFile` is the importing file's own repo-prefixed path; it
// anchors relative module specifiers like `'./schema'`. Bare module
// specifiers (`'react'`) are skipped — they don't resolve to a graph
// file the local repo owns. tsconfig-style path aliases (`@/lib/api`,
// `$utils/format`) ARE resolved when an alias map is provided.
func parseTSImports(src, srcFile string, aliasMap *tsalias.Map, repoPrefix string) map[string]string {
	matches := tsImportRe.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		return nil
	}
	out := map[string]string{}
	srcDir := path.Dir(srcFile)
	for _, m := range matches {
		named := m[1]
		defaultOrStar := m[2]
		extraNamed := m[3]
		modulePath := m[4]
		resolved := resolveTSModulePath(modulePath, srcDir, aliasMap, repoPrefix)
		if resolved == "" {
			continue
		}
		for _, name := range splitTSImportClause(named) {
			out[name] = resolved
		}
		for _, name := range splitTSImportClause(extraNamed) {
			out[name] = resolved
		}
		if defaultOrStar != "" {
			ident := defaultOrStar
			if strings.HasPrefix(ident, "*") {
				if i := strings.LastIndex(ident, " "); i >= 0 {
					ident = strings.TrimSpace(ident[i+1:])
				}
			}
			if ident != "" {
				out[ident] = resolved
			}
		}
	}
	return out
}

// splitTSImportClause unpacks a brace-delimited import list like
// `Foo, Bar as Baz, type Qux` into the local-binding names a caller
// would reference (`Foo`, `Baz`, `Qux`). The `type` keyword and
// `as <alias>` rebinds are normalised; commas inside the body are
// the only separator we care about.
func splitTSImportClause(body string) []string {
	if body == "" {
		return nil
	}
	parts := strings.Split(body, ",")
	out := make([]string, 0, len(parts))
	for _, raw := range parts {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		entry = strings.TrimPrefix(entry, "type ")
		entry = strings.TrimSpace(entry)
		if i := strings.Index(entry, " as "); i >= 0 {
			entry = strings.TrimSpace(entry[i+4:])
		}
		if entry == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// resolveTSModulePath turns a TS/JS module specifier into the
// repo-prefixed file path of the imported source, or "" when the
// specifier is unresolvable (third-party module with no matching
// alias). Resolution order:
//
//  1. Relative path (`./foo`, `../bar/baz`) — anchored at srcDir.
//  2. tsconfig path alias (`@/lib/foo`, `$utils/format`) — looked up
//     against aliasMap; the resolved repo-relative target is prefixed
//     with repoPrefix so it lines up with graph FilePaths.
//
// We don't probe the disk; the caller matches the resolved path
// against a candidate's FilePath, so we just append the canonical
// `.ts` extension when none is present. `.tsx` / `.js` / `.jsx`
// paths are returned as-is when the user wrote them explicitly.
// Directory imports resolving to `index.*` are NOT handled — the
// resolver returns the bare-stem path; if the candidate type lives
// in `<dir>/index.ts` the upgrade falls through and the bare name
// is left in place (acceptable: the dashboard still renders the
// bare type chip).
func resolveTSModulePath(modulePath, srcDir string, aliasMap *tsalias.Map, repoPrefix string) string {
	if modulePath == "" {
		return ""
	}
	if strings.HasPrefix(modulePath, "./") || strings.HasPrefix(modulePath, "../") {
		joined := path.Clean(path.Join(srcDir, modulePath))
		switch path.Ext(joined) {
		case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".svelte", ".vue":
			return joined
		}
		return joined + ".ts"
	}
	// Bare specifier — try alias resolution. tsalias.Resolve returns a
	// repo-relative path with the extension stripped; we re-prefix it
	// with repoPrefix so it lines up with graph FilePaths, then add
	// the canonical `.ts` so the caller can match against an indexed
	// file. Returning "" still leaves the bare type name in place.
	if aliasMap == nil {
		return ""
	}
	repoRel := tsalias.Resolve(aliasMap, modulePath)
	if repoRel == "" {
		return ""
	}
	full := repoRel
	if repoPrefix != "" {
		full = repoPrefix + "/" + repoRel
	}
	switch path.Ext(full) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".svelte", ".vue":
		return full
	}
	return full + ".ts"
}

// maxReExportDepth bounds barrel re-export chain following so a
// pathological circular `export * from` set can't loop forever.
const maxReExportDepth = 8

// tsReExportRe matches `export ... from '...'` re-export statements:
//
//	export * from './x'
//	export * as ns from './x'   (namespace — ignored for name-following)
//	export { A, B as C } from './x'
//	export type { T } from './x'
var tsReExportRe = regexp.MustCompile(
	`export\s+(?:type\s+)?(?:(\*(?:\s+as\s+\w+)?)|\{([^}]*)\})\s*from\s*['"]([^'"]+)['"]`)

// tsReExport is one parsed `export ... from` re-export statement.
type tsReExport struct {
	star bool
	// names maps an exported name to its name in the source module
	// (they differ under `export { Real as Public }`).
	names    map[string]string
	fromFile string
}

// tsFileCandidates expands a resolved TS/JS module path into the
// concrete files it might be on disk. resolveTSModulePath always
// guesses `.ts`, but the real file is often `.tsx`, or the specifier
// pointed at a directory whose entry point is `index.ts` (the barrel).
// Single-file-component extensions (`.svelte`, `.vue`) are matched as
// direct files only — they carry no `index.svelte` / `index.vue`
// directory-barrel convention, so a bare directory import never
// expands to one.
func tsFileCandidates(resolved string) []string {
	if resolved == "" {
		return nil
	}
	stem := resolved
	switch path.Ext(resolved) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".svelte", ".vue":
		stem = strings.TrimSuffix(resolved, path.Ext(resolved))
	}
	out := []string{resolved}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".d.ts", ".mts", ".cts"} {
		out = append(out, stem+ext, stem+"/index"+ext)
	}
	// Single-file components resolve to a direct file at the stem,
	// never to a directory index.
	for _, ext := range []string{".svelte", ".vue"} {
		out = append(out, stem+ext)
	}
	seen := make(map[string]bool, len(out))
	uniq := out[:0]
	for _, c := range out {
		if !seen[c] {
			seen[c] = true
			uniq = append(uniq, c)
		}
	}
	return uniq
}

// parseTSReExports extracts barrel re-export statements from a TS/JS
// source file, resolving each `from` specifier the same way
// parseTSImports resolves an import.
func parseTSReExports(src, srcFile string, aliasMap *tsalias.Map, repoPrefix string) []tsReExport {
	matches := tsReExportRe.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		return nil
	}
	srcDir := path.Dir(srcFile)
	var out []tsReExport
	for _, m := range matches {
		resolved := resolveTSModulePath(m[3], srcDir, aliasMap, repoPrefix)
		if resolved == "" {
			continue
		}
		re := tsReExport{fromFile: resolved}
		if strings.TrimSpace(m[1]) == "*" {
			re.star = true
			out = append(out, re)
			continue
		}
		if m[1] != "" {
			// `export * as ns` — a namespace re-export; it does not
			// transparently forward individual names.
			continue
		}
		re.names = map[string]string{}
		for _, raw := range strings.Split(m[2], ",") {
			entry := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "type "))
			if entry == "" {
				continue
			}
			orig, exported := entry, entry
			if i := strings.Index(entry, " as "); i >= 0 {
				orig = strings.TrimSpace(entry[:i])
				exported = strings.TrimSpace(entry[i+4:])
			}
			if orig != "" && exported != "" {
				re.names[exported] = orig
			}
		}
		if len(re.names) > 0 {
			out = append(out, re)
		}
	}
	return out
}

// cachedSource reads file's bytes through the per-pass cache, using
// readFileFromAnyRepo for misses. A nil cache entry records "absent".
func (mi *MultiIndexer) cachedSource(file string, srcCache map[string][]byte) []byte {
	if src, hit := srcCache[file]; hit {
		return src
	}
	data, found := mi.readFileFromAnyRepo(file)
	if !found {
		srcCache[file] = nil
		return nil
	}
	srcCache[file] = data
	return data
}

// reExportEdge is one parsed re-export statement in a language-neutral
// shape: a `star` glob re-export (`export *` / `pub use mod::*`) or a
// `names` map of exported-name → name-in-the-source-module. fromFile is
// the resolved repo-prefixed path of the module being re-exported.
type reExportEdge struct {
	star     bool
	names    map[string]string
	fromFile string
	// Rust-only provenance retained from the source use-tree. TypeScript
	// re-exports leave these empty.
	sourceModule  string
	visibility    string
	ambiguousName map[string]bool
}

// rustUseRe matches both private imports and visible re-exports. Capture
// groups retain visibility, the source module, glob/list shape, and the full
// symbol entry so aliases can be followed without degrading to a basename.
// The regex deliberately accepts a conservative ASCII identifier subset;
// Unicode identifiers remain unresolved instead of being guessed.
var rustUseRe = regexp.MustCompile(
	`(?m)\b(pub(?:\s*\([^)]*\))?\s+)?use\s+([\w#:]+?)\s*::\s*(?:(\*)|\{([^}]*)\}|([\w#]+(?:\s+as\s+[\w#]+)?))\s*;`)

type rustUseFact struct {
	fromFile     string
	sourceModule string
	sourceName   string
	localName    string
	visibility   string
	glob         bool
}

// rustFileCandidates expands a resolved Rust module path into the
// concrete files it might be on disk. A module `foo` lives either in
// `foo.rs` or in `foo/mod.rs`; this mirrors tsFileCandidates' role of
// turning a logical module reference into matchable file paths. The
// input is the `.rs`-suffixed module path resolveRustModulePath
// produces; the suffix is stripped to recover the stem.
const rustLogicalCrateRootFile = ".gortex-crate-root.rs"

func rustLogicalCrateRoot(crateRoot string) string {
	return path.Join(crateRoot, rustLogicalCrateRootFile)
}

func rustFileCandidates(modulePath string) []string {
	if path.Base(modulePath) == rustLogicalCrateRootFile {
		root := path.Dir(modulePath)
		return []string{path.Join(root, "lib.rs"), path.Join(root, "main.rs")}
	}
	if modulePath == "" {
		return nil
	}
	stem := strings.TrimSuffix(modulePath, ".rs")
	if strings.HasSuffix(stem, "/mod") {
		// Already a directory-module file path — keep it verbatim.
		return []string{stem + ".rs"}
	}
	return []string{stem + ".rs", stem + "/mod.rs"}
}

// rustCrateRoot returns the crate source root for a repo-prefixed Rust
// file — the nearest ancestor directory named `src`, or the file's own
// directory when the layout has no `src` (a flat crate). `crate::`
// paths are anchored here.
func rustCrateRoot(srcFile string) string {
	dir := path.Dir(srcFile)
	for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if path.Base(d) == "src" {
			return d
		}
	}
	return dir
}

// resolveRustModulePath turns a Rust module path (the part of a
// `pub use` before its final symbol / glob / list) into a repo-prefixed,
// `.rs`-suffixed module path, anchored against the re-exporting file.
// `crate::` is anchored at the crate src root; `self::` at the current
// module directory; `super::` at the parent. A leading bare segment is
// conservatively left unresolved because Rust 2018 may interpret it as an
// external crate and Cargo dependency identity is not available here. The
// `.rs` suffix lets downstream language dispatch recognise the result
// as Rust even though a logical module name carries no extension;
// rustFileCandidates strips it back to a stem.
func resolveRustModulePath(modPath, srcFile string) string {
	segs := strings.Split(strings.TrimSpace(modPath), "::")
	for len(segs) > 0 && segs[0] == "" {
		segs = segs[1:]
	}
	if len(segs) == 0 {
		return ""
	}
	crateRoot := rustCrateRoot(srcFile)
	var dir string
	switch segs[0] {
	case "crate":
		dir = crateRoot
		segs = segs[1:]
		if len(segs) == 0 {
			return rustCrateModuleFile(srcFile)
		}
	case "self":
		dir = rustModuleDirectory(srcFile)
		segs = segs[1:]
		if len(segs) == 0 {
			return path.Clean(srcFile)
		}
	case "super":
		dir = rustModuleDirectory(srcFile)
		for len(segs) > 0 && segs[0] == "super" {
			if dir == crateRoot || !strings.HasPrefix(dir, crateRoot+"/") {
				return ""
			}
			dir = path.Dir(dir)
			segs = segs[1:]
		}
	default:
		// In Rust 2018 a leading bare segment may name an external crate.
		// Without Cargo dependency facts it is unsafe to reinterpret it as
		// a same-named local module; callers must use crate/self/super for
		// conservative local resolution.
		return ""
	}
	if len(segs) == 0 {
		if dir == crateRoot {
			return rustCrateModuleFile(srcFile)
		}
		return path.Clean(dir) + ".rs"
	}
	return path.Clean(path.Join(dir, path.Join(segs...))) + ".rs"
}

func rustCrateModuleFile(srcFile string) string {
	clean := path.Clean(srcFile)
	root := rustCrateRoot(clean)
	if path.Dir(clean) == root {
		switch path.Base(clean) {
		case "lib.rs", "main.rs":
			return clean
		}
	}
	// Nested modules do not encode whether their crate root is a library or
	// binary. Keep a logical root so both lib.rs and main.rs remain candidates;
	// exact graph identity later selects one or rejects ambiguity.
	return rustLogicalCrateRoot(root)
}

func rustModuleDirectory(srcFile string) string {
	dir := path.Dir(srcFile)
	base := path.Base(srcFile)
	switch base {
	case "lib.rs", "main.rs", "mod.rs":
		return dir
	}
	return strings.TrimSuffix(srcFile, path.Ext(srcFile))
}

// maskRustNonCode blanks comments and string literals while preserving byte
// positions and newlines. The use parser remains intentionally shallow (it
// does not parse recursive use trees), but apparent `use` text in comments,
// normal strings, byte strings, and raw strings cannot become import facts.
func maskRustNonCode(src string) string {
	masked := []byte(src)
	mask := func(start, end int) {
		for i := start; i < end; i++ {
			if masked[i] != '\n' && masked[i] != '\r' {
				masked[i] = ' '
			}
		}
	}
	isIdentContinue := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
	}
	rawStart := func(start int) (quote, hashes int, ok bool) {
		if start > 0 && isIdentContinue(src[start-1]) {
			return 0, 0, false
		}
		i := start
		if src[i] == 'b' {
			i++
			if i >= len(src) || src[i] != 'r' {
				return 0, 0, false
			}
		}
		if src[i] != 'r' {
			return 0, 0, false
		}
		i++
		for i < len(src) && src[i] == '#' {
			hashes++
			i++
		}
		if i >= len(src) || src[i] != '"' {
			return 0, 0, false
		}
		return i, hashes, true
	}

	for i := 0; i < len(src); {
		switch {
		case i+1 < len(src) && src[i] == '/' && src[i+1] == '/':
			start := i
			i += 2
			for i < len(src) && src[i] != '\n' {
				i++
			}
			mask(start, i)
		case i+1 < len(src) && src[i] == '/' && src[i+1] == '*':
			start, depth := i, 1
			i += 2
			for i < len(src) && depth > 0 {
				switch {
				case i+1 < len(src) && src[i] == '/' && src[i+1] == '*':
					depth++
					i += 2
				case i+1 < len(src) && src[i] == '*' && src[i+1] == '/':
					depth--
					i += 2
				default:
					i++
				}
			}
			mask(start, i)
		case src[i] == 'r' || src[i] == 'b':
			quote, hashes, ok := rawStart(i)
			if !ok {
				i++
				continue
			}
			start := i
			i = quote + 1
			for i < len(src) {
				if src[i] != '"' {
					i++
					continue
				}
				end := i + 1
				for end < len(src) && end-i-1 < hashes && src[end] == '#' {
					end++
				}
				if end-i-1 == hashes {
					i = end
					break
				}
				i++
			}
			mask(start, i)
		case src[i] == '"':
			start := i
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					if i > len(src) {
						i = len(src)
					}
					continue
				}
				i++
				if src[i-1] == '"' {
					break
				}
			}
			mask(start, i)
		default:
			i++
		}
	}
	return string(masked)
}

// parseRustReExports extracts `pub use` re-export statements from a
// Rust source file, resolving each module path to a repo-prefixed
// module stem the same way parseTSReExports resolves an `export ...
// from` specifier. Restricted visibility is retained as source text; this
// resolver does not interpret the restriction's access scope.
func parseRustUseFacts(src, srcFile string) []rustUseFact {
	matches := rustUseRe.FindAllStringSubmatch(maskRustNonCode(src), -1)
	if len(matches) == 0 {
		return nil
	}
	var out []rustUseFact
	for _, match := range matches {
		visibility := strings.TrimSpace(match[1])
		baseModule := strings.TrimSpace(match[2])
		if match[3] == "*" {
			fromFile := resolveRustModulePath(baseModule, srcFile)
			if fromFile != "" {
				out = append(out, rustUseFact{
					fromFile: fromFile, sourceModule: baseModule,
					visibility: visibility, glob: true,
				})
			}
			continue
		}
		entries := []string{match[5]}
		if match[4] != "" {
			entries = strings.Split(match[4], ",")
		}
		for _, raw := range entries {
			if fact, ok := rustUseFactForEntry(baseModule, raw, srcFile, visibility); ok {
				out = append(out, fact)
			}
		}
	}
	return out
}

func rustUseFactForEntry(baseModule, raw, srcFile, visibility string) (rustUseFact, bool) {
	orig, local := splitRustUseEntry(raw)
	orig = strings.Trim(strings.TrimSpace(orig), ":")
	if orig == "" || local == "" || orig == "self" {
		return rustUseFact{}, false
	}
	parts := strings.Split(orig, "::")
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return rustUseFact{}, false
		}
	}
	sourceName := parts[len(parts)-1]
	sourceModule := strings.Trim(strings.TrimSpace(baseModule), ":")
	if len(parts) > 1 {
		sourceModule += "::" + strings.Join(parts[:len(parts)-1], "::")
	}
	fromFile := resolveRustModulePath(sourceModule, srcFile)
	if fromFile == "" {
		return rustUseFact{}, false
	}
	return rustUseFact{
		fromFile: fromFile, sourceModule: sourceModule,
		sourceName: sourceName, localName: local, visibility: visibility,
	}, true
}

func parseRustReExports(src, srcFile string) []reExportEdge {
	type groupKey struct {
		fromFile, sourceModule, visibility string
	}
	var out []reExportEdge
	groups := map[groupKey]int{}
	for _, fact := range parseRustUseFacts(src, srcFile) {
		if fact.visibility == "" {
			continue
		}
		key := groupKey{fact.fromFile, fact.sourceModule, fact.visibility}
		if fact.glob {
			out = append(out, reExportEdge{
				star: true, fromFile: fact.fromFile,
				sourceModule: fact.sourceModule, visibility: fact.visibility,
			})
			continue
		}
		if index, ok := groups[key]; ok {
			edge := &out[index]
			if edge.ambiguousName[fact.localName] {
				continue
			}
			if _, duplicate := edge.names[fact.localName]; duplicate {
				if edge.ambiguousName == nil {
					edge.ambiguousName = map[string]bool{}
				}
				edge.ambiguousName[fact.localName] = true
				delete(edge.names, fact.localName)
				continue
			}
			edge.names[fact.localName] = fact.sourceName
			continue
		}
		groups[key] = len(out)
		out = append(out, reExportEdge{
			names:    map[string]string{fact.localName: fact.sourceName},
			fromFile: fact.fromFile, sourceModule: fact.sourceModule,
			visibility: fact.visibility,
		})
	}
	return out
}

// splitRustUseEntry normalises a single `use`-list entry, resolving an
// `Orig as Public` rebind to (sourceName, exportedName). A qualified plain
// entry retains its full source path while exporting only its target leaf.
func splitRustUseEntry(raw string) (orig, exported string) {
	entry := strings.TrimSpace(raw)
	if entry == "" {
		return "", ""
	}
	orig = entry
	if i := strings.Index(entry, " as "); i >= 0 {
		orig = strings.TrimSpace(entry[:i])
		exported = strings.TrimSpace(entry[i+4:])
	} else {
		parts := strings.Split(strings.Trim(orig, ":"), "::")
		exported = strings.TrimSpace(parts[len(parts)-1])
	}
	return orig, exported
}

// parseRustImports walks the `use` statements of a Rust source file
// and returns local-binding-name → the repo-prefixed module stem the
// name was imported from. It is the Rust counterpart of parseTSImports:
// the caller follows a re-export chain from the returned module to
// reach the symbol's real definition. Glob imports (`use mod::*`) are
// skipped — they bind no specific name to follow.
func parseRustImports(src, srcFile string) map[string]string {
	out := map[string]string{}
	ambiguous := map[string]bool{}
	for _, fact := range parseRustUseFacts(src, srcFile) {
		if fact.glob || fact.localName == "" {
			continue
		}
		if _, exists := out[fact.localName]; exists {
			ambiguous[fact.localName] = true
			continue
		}
		if !ambiguous[fact.localName] {
			out[fact.localName] = fact.fromFile
		}
	}
	for name := range ambiguous {
		delete(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func rustImportFactsForName(src, srcFile, name string) ([]rustUseFact, bool) {
	facts := parseRustUseFacts(src, srcFile)
	var explicit []rustUseFact
	for _, fact := range facts {
		if !fact.glob && fact.localName == name {
			explicit = append(explicit, fact)
		}
	}
	if len(explicit) > 0 {
		// Rust rejects duplicate local bindings even when they happen to point
		// at the same source. Never let source order choose a winner.
		if len(explicit) != 1 {
			return nil, false
		}
		return explicit, true
	}

	var globs []rustUseFact
	seen := map[string]bool{}
	for _, fact := range facts {
		if !fact.glob {
			continue
		}
		fact.sourceName = name
		fact.localName = name
		key := fact.fromFile + "\x00" + fact.sourceName
		if seen[key] {
			continue
		}
		seen[key] = true
		globs = append(globs, fact)
	}
	return globs, len(globs) > 0
}

// isRustFile reports whether a repo-prefixed path is a Rust source
// file — the re-export follower parses these with the `pub use`
// grammar rather than the TS `export ... from` grammar.
func isRustFile(filePath string) bool {
	return path.Ext(filePath) == ".rs"
}

// fileCandidatesFor expands a logical module/file reference into the
// concrete on-disk file paths it might map to, picking the TS or Rust
// expansion by file extension.
func fileCandidatesFor(file string) []string {
	if isRustFile(file) {
		return rustFileCandidates(file)
	}
	return tsFileCandidates(file)
}

// parseImportsFor maps each imported local-binding name to the
// repo-prefixed module / file it was imported from, dispatching to the
// TS (`import ... from`) or Rust (`use`) parser by file extension.
func (mi *MultiIndexer) parseImportsFor(src, srcFile string) map[string]string {
	if isRustFile(srcFile) {
		return parseRustImports(src, srcFile)
	}
	aliasMap, aliasPrefix := mi.tsAliasMapFor(srcFile)
	return parseTSImports(src, srcFile, aliasMap, aliasPrefix)
}

// reExportsFor reads the re-export statements out of one source file,
// dispatching to the TS (`export ... from`) or Rust (`pub use`) parser
// by the file's extension. Returns nil for any other language.
func (mi *MultiIndexer) reExportsFor(src, srcPath string) []reExportEdge {
	if isRustFile(srcPath) {
		return parseRustReExports(src, srcPath)
	}
	aliasMap, aliasPrefix := mi.tsAliasMapFor(srcPath)
	tsRe := parseTSReExports(src, srcPath, aliasMap, aliasPrefix)
	if len(tsRe) == 0 {
		return nil
	}
	out := make([]reExportEdge, len(tsRe))
	for i, re := range tsRe {
		out[i] = reExportEdge{star: re.star, names: re.names, fromFile: re.fromFile}
	}
	return out
}

// reExportChainResult records concrete files and symbol names reached while
// following TypeScript or Rust re-exports, plus whether ambiguity made the
// chain unsafe to use for resolution.
type reExportChainResult struct {
	files  map[string]bool
	names  map[string]map[string]bool
	unsafe bool
}

func addReExportChainTarget(result *reExportChainResult, file, name string) {
	for _, candidate := range fileCandidatesFor(file) {
		result.files[candidate] = true
		if result.names[candidate] == nil {
			result.names[candidate] = map[string]bool{}
		}
		result.names[candidate][name] = true
	}
}

func (mi *MultiIndexer) followReExportChainChecked(startFile, name string, srcCache map[string][]byte) (map[string]bool, bool) {
	result := mi.followReExportChainDetailed(startFile, name, srcCache)
	return result.files, result.unsafe
}

func (mi *MultiIndexer) followReExportChainDetailed(startFile, name string, srcCache map[string][]byte) reExportChainResult {
	result := reExportChainResult{
		files: map[string]bool{}, names: map[string]map[string]bool{},
	}
	addReExportChainTarget(&result, startFile, name)
	type step struct{ file, name string }
	visiting := map[step]bool{}
	visited := map[step]bool{}

	var visit func(step, int)
	visit = func(current step, depth int) {
		if visiting[current] {
			result.unsafe = true
			return
		}
		if visited[current] {
			return
		}
		visiting[current] = true
		defer func() {
			delete(visiting, current)
			visited[current] = true
		}()

		type sourceFile struct {
			data []byte
			path string
		}
		var sources []sourceFile
		for _, candidate := range fileCandidatesFor(current.file) {
			if data := mi.cachedSource(candidate, srcCache); len(data) > 0 {
				sources = append(sources, sourceFile{data: data, path: candidate})
			}
		}
		if len(sources) == 0 {
			return
		}

		forwarded := map[step]bool{}
		for _, source := range sources {
			named := map[step]bool{}
			globs := map[step]bool{}
			namedMatches := 0
			for _, re := range mi.reExportsFor(string(source.data), source.path) {
				if re.ambiguousName[current.name] {
					result.unsafe = true
					continue
				}
				if re.star {
					globs[step{file: re.fromFile, name: current.name}] = true
					continue
				}
				if original, ok := re.names[current.name]; ok {
					namedMatches++
					named[step{file: re.fromFile, name: original}] = true
				}
			}

			// Explicit re-exports shadow globs within one concrete module file.
			// Alternate lib/main roots remain parallel candidates, and exact graph
			// identity decides between them after the full chain is collected.
			selected := globs
			switch namedMatches {
			case 0:
			case 1:
				selected = named
			default:
				result.unsafe = true
			}
			for target := range selected {
				forwarded[target] = true
			}
		}
		if result.unsafe {
			return
		}
		if depth >= maxReExportDepth && len(forwarded) > 0 {
			result.unsafe = true
			return
		}
		for target := range forwarded {
			addReExportChainTarget(&result, target.file, target.name)
			visit(target, depth+1)
		}
	}

	visit(step{file: startFile, name: name}, 0)
	return result
}

// isImportResolvableLang reports whether the contract source file
// uses an import system this resolver can parse. TypeScript and
// JavaScript files use ES-module imports we understand; Svelte / Vue
// single-file components carry the same ES-module `import` /
// `export ... from` syntax in their script block; Rust `use`
// declarations are parsed for `pub use` re-export following. Go uses
// package qualification which the in-file pass already handles
// (and would have produced an unambiguous resolution at extraction
// time).
func isImportResolvableLang(filePath string) bool {
	switch path.Ext(filePath) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".svelte", ".vue", ".rs":
		return true
	}
	return false
}

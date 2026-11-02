package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	wire "github.com/gortexhq/gcx-go"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// freshnessRiderFor returns a small structured freshness block for a
// file-reading tool whose target file has changed on disk since it was
// indexed — so an agent reading a just-edited file sees, inline, that the
// graph view may lag the working tree. It returns nil (no rider, zero
// extra tokens) for the overwhelmingly common fresh case, for non-file
// tools, and in multi-repo mode (where the legacy single-indexer's
// staleness signal is all-noise — per-repo watchers own freshness there).
//
// The check is O(1): one map lookup + one stat on the single file the
// tool targets, only for the handful of read/source tools.
func (s *Server) freshnessRiderFor(toolName string, req mcp.CallToolRequest) map[string]any {
	if s.indexer == nil && s.multiIndexer == nil {
		return nil
	}
	if os.Getenv("GORTEX_NO_FRESHNESS_RIDER") == "1" {
		return nil
	}
	raw := targetRepoFileRaw(toolName, req)
	if raw == "" {
		return nil
	}
	// Route the target to its OWNING indexer. In multi-repo mode this is the
	// per-repo sub-indexer that tracks the file's own mtimes / SHA — so the
	// staleness signal is reclaimed in the flagship deployment instead of
	// being suppressed wholesale. In single-repo mode it is the lone indexer.
	owner, repoRel := s.indexerForRel(raw)
	if owner == nil || repoRel == "" {
		return nil
	}
	state := owner.TrackedFileState(repoRel)
	mismatch := s.detectWorktreeMismatch()

	// Extractor-version staleness: which languages' extractors this binary
	// bumped since the OWNING repo was indexed. Fetched alongside the index
	// state so a graph indexed by an older extractor is flagged even when the
	// touched file's content is unchanged — per-language, so the advisory
	// names the exact languages to reindex rather than implying a full rebuild.
	var indexState graph.RepoIndexState
	var haveState bool
	// Extractor-version staleness, narrowed to THIS file's language. The
	// advisory rides on a per-file response, so a language the file is not
	// written in is noise the reader cannot act on: the reindex it asks
	// for would not touch the file in hand. Narrowing also keeps the
	// banner off repositories that hold no file of the stale language at
	// all — the comparison is against the baseline version every language
	// implicitly carries, so a bumped language is "behind" in every
	// repository indexed by an older binary, Go-only ones included.
	var staleLangs []string
	if r, ok := graph.Store(s.graph).(graph.RepoIndexStateReader); ok {
		if st, found, _ := r.GetRepoIndexState(owner.RepoPrefix()); found {
			indexState, haveState = st, true
			if fileLang := indexer.ExtractorLangForFile(repoRel); fileLang != "" &&
				slices.Contains(indexer.ExtractorVersionStaleLangs(st.ExtractorVersions), fileLang) {
				staleLangs = []string{fileLang}
			}
		}
	}

	// Whole-index "frozen" banner: when the file watcher is degraded (inotify
	// or FD exhaustion) the graph may silently lag every live edit, not just
	// this file — a condition distinct from per-file staleness. Surface it even
	// when the targeted file itself looks fresh.
	frozen := s.watchDegradedReason()

	if state == indexer.FileFresh && !mismatch && len(staleLangs) == 0 && frozen == "" {
		return nil
	}
	out := map[string]any{"file": repoRel}
	if frozen != "" {
		out["index_frozen"] = true
		out["index_frozen_hint"] = frozen
	}
	if p := owner.RepoPrefix(); p != "" {
		out["repo"] = p
	}
	switch state {
	case indexer.FileStale:
		out["stale"] = true
		out["hint"] = "this file changed on disk since it was last indexed; the graph view may lag the working tree"
		if haveState {
			if indexState.IndexedSHA != "" {
				out["indexed_sha"] = shortFreshSHA(indexState.IndexedSHA)
			}
			if indexState.Dirty {
				out["working_tree_dirty_at_index"] = true
			}
		}
	case indexer.FileMissing:
		out["missing"] = true
		out["hint"] = "this file is recorded in the graph but no longer exists on disk; it was deleted or moved since indexing"
	}
	if len(staleLangs) > 0 {
		out["extractor_stale_langs"] = staleLangs
		out["extractor_stale_hint"] = "this file's language extractor was upgraded since indexing; reindex to pick up the newer extraction (gortex index .)"
	}
	if mismatch {
		out["worktree_mismatch"] = true
		out["worktree_hint"] = "the working directory is a linked git worktree the indexed graph does not cover — results reflect another checkout"
	}
	return out
}

// watchDegradedReason returns the file watcher's degraded reason (inotify / FD
// exhaustion) when the index may be silently frozen, or "" when watching is
// healthy or unavailable. Read through an interface assertion so the rider
// stays decoupled from the concrete *indexer.Watcher behind the server's
// watcher field.
func (s *Server) watchDegradedReason() string {
	watcher := s.currentWatcher()
	if watcher == nil {
		return ""
	}
	if dr, ok := watcher.(interface{ DegradedReason() string }); ok {
		return dr.DegradedReason()
	}
	return ""
}

// indexerForRel routes a graph file path (as it appears in tool output or
// requests — repo-prefixed in multi-repo mode) to the per-repo indexer that
// owns it, returning that indexer and the repo-relative subpath its mtime keys
// use. In single-repo mode it returns the lone indexer with the configured
// prefix stripped. Returns (nil, "") when the path maps to no tracked repo.
func (s *Server) indexerForRel(graphPath string) (*indexer.Indexer, string) {
	graphPath = filepath.ToSlash(graphPath)
	if s.multiIndexer == nil {
		if s.indexer == nil {
			return nil, ""
		}
		rel := graphPath
		if p := s.indexer.RepoPrefix(); p != "" {
			rel = strings.TrimPrefix(rel, p+"/")
		}
		return s.indexer, rel
	}
	abs := s.multiIndexer.ResolveFilePath(graphPath)
	if abs == "" {
		return nil, ""
	}
	owner, prefix := s.multiIndexer.IndexerForFile(abs)
	if owner == nil {
		return nil, ""
	}
	return owner, strings.TrimPrefix(graphPath, prefix+"/")
}

// pathIndexability answers, for a file the graph holds no nodes for, whether
// the walk would ever hold it — "excluded by design" versus "not indexed yet".
//
// indexerForRel fails closed on a bare unprefixed path more than one repo
// could own (two JS repos each holding node_modules/react/index.js), which is
// the shape the PreToolUse hook sends. So a nil owner falls through to a vote,
// taken only on unanimity — a split answer leaves enforcement on.
func (s *Server) pathIndexability(graphPath string) fileNotIndexedState {
	if idx, rel := s.indexerForRel(graphPath); idx != nil {
		skip, answered := idx.PathIndexability(rel)
		if !answered {
			return fileNotIndexedState{}
		}
		return skipState(skip)
	}
	if s.multiIndexer == nil {
		return fileNotIndexedState{}
	}
	rel := filepath.ToSlash(graphPath)
	if prefix, stripped, ok := s.splitRepoPrefix(rel); ok {
		// The path names its own corpus, so one repo owns it. A vote would ask
		// the others about a path spelled for this one, which they answer for
		// a file of their own whenever the spelling collides.
		idx := s.multiIndexer.GetIndexer(prefix)
		if idx == nil {
			return fileNotIndexedState{}
		}
		skip, answered := idx.PathIndexability(stripped)
		if !answered {
			return fileNotIndexedState{}
		}
		return skipState(skip)
	}
	votes := make([]pathSkipVote, 0, 4)
	for _, prefix := range s.multiIndexer.RepoPrefixes() {
		idx := s.multiIndexer.GetIndexer(prefix)
		if idx == nil {
			continue
		}
		skip, answered := idx.PathIndexability(rel)
		votes = append(votes, pathSkipVote{Skip: skip, Answered: answered})
	}
	agreed, ok := unanimousPathSkip(votes)
	if !ok {
		return fileNotIndexedState{}
	}
	return skipState(agreed)
}

// splitRepoPrefix separates a graph path that names its corpus into that repo
// prefix and the path relative to the repo's root. Longest match wins, so a
// repo nested under another resolves to the inner one.
func (s *Server) splitRepoPrefix(graphPath string) (prefix, rel string, ok bool) {
	for _, p := range s.multiIndexer.RepoPrefixes() {
		if p == "" || len(p) <= len(prefix) {
			continue
		}
		if rest, found := strings.CutPrefix(graphPath, p+"/"); found {
			prefix, rel, ok = p, rest, true
		}
	}
	return prefix, rel, ok
}

// pathSkipVote is one repo's answer; Answered is false when it could not
// answer at all.
type pathSkipVote struct {
	Skip     indexer.PathSkip
	Answered bool
}

// unanimousPathSkip takes the verdict only when every repo that could answer
// agreed. Abstentions are dropped: an un-rooted repo returns the zero PathSkip,
// bit-identical to "indexable", so counting it let one silent repo disagree
// with every repo that looked. ok is false on no answers or a conflict.
func unanimousPathSkip(votes []pathSkipVote) (indexer.PathSkip, bool) {
	var agreed *indexer.PathSkip
	for _, v := range votes {
		if !v.Answered {
			continue
		}
		if agreed == nil {
			skip := v.Skip
			agreed = &skip
			continue
		}
		if *agreed != v.Skip {
			return indexer.PathSkip{}, false
		}
	}
	if agreed == nil {
		return indexer.PathSkip{}, false
	}
	return *agreed, true
}

func skipState(skip indexer.PathSkip) fileNotIndexedState {
	return fileNotIndexedState{Unindexable: skip.Skipped, Excluded: skip.ByRule}
}

// detectWorktreeMismatch reports (once per server, cached) whether the
// current working directory is a linked git worktree that the indexed graph
// does not cover — i.e. the agent is working in a worktree but the graph
// reflects a different checkout, so its results may not match the files on
// disk. Single-repo only; multi-repo routing owns its own worktree scoping.
func (s *Server) detectWorktreeMismatch() bool {
	s.worktreeMismatchOnce.Do(func() {
		if s.indexer == nil || s.multiIndexer != nil {
			return
		}
		cwd, err := os.Getwd()
		if err != nil || cwd == "" {
			return
		}
		if !indexer.ResolveWorktree(cwd).IsWorktree {
			return // not a linked worktree — nothing to warn about
		}
		root := s.indexer.RootPath()
		if root == "" {
			return
		}
		// The cwd is a linked worktree. If the indexed root does not contain
		// it, the graph reflects another checkout.
		cwdResolved, _ := filepath.EvalSymlinks(cwd)
		rootResolved, _ := filepath.EvalSymlinks(root)
		if cwdResolved == "" {
			cwdResolved = cwd
		}
		if rootResolved == "" {
			rootResolved = root
		}
		if !pathWithin(cwdResolved, rootResolved) {
			s.worktreeMismatch = true
		}
	})
	return s.worktreeMismatch
}

// pathWithin reports whether child is equal to or nested under parent, on a
// slash-segment boundary (so /a/bc is not "within" /a/b).
func pathWithin(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// targetRepoFileRaw extracts the single file a read tool targets, verbatim
// (no prefix stripping) so the caller can route it to its owning repo. Returns
// "" for tools that are not file-scoped.
func targetRepoFileRaw(toolName string, req mcp.CallToolRequest) string {
	var raw string
	switch toolName {
	case "read_file", "get_file_summary", "get_editing_context":
		raw = req.GetString("path", "")
	case "get_symbol_source", "get_symbol", "get_cfg":
		id := req.GetString("id", "")
		if i := strings.Index(id, "::"); i >= 0 {
			raw = id[:i]
		}
	default:
		return ""
	}
	if raw == "" {
		return ""
	}
	return filepath.ToSlash(raw)
}

// targetRepoRelFile extracts the repo-relative path of the single file a
// read tool targets, or "" when the tool is not file-scoped. A leading
// repo prefix is stripped so the result matches the indexer's mtime keys;
// a path that does not match is simply reported not-stale (an unknown key
// reads as fresh), so imperfect normalization is safe.
func targetRepoRelFile(toolName string, req mcp.CallToolRequest, prefix string) string {
	raw := targetRepoFileRaw(toolName, req)
	if raw == "" {
		return ""
	}
	if prefix != "" {
		raw = strings.TrimPrefix(raw, prefix+"/")
	}
	return raw
}

// isFreshnessListTool reports whether a tool returns a list of graph hits
// whose underlying files should be swept for on-disk drift / deletion.
func isFreshnessListTool(name string) bool {
	switch name {
	case "search_symbols", "find_usages", "smart_context", "get_callers":
		return true
	}
	return false
}

// freshnessPathKeys are the JSON keys whose string value names a file across
// the list-shaped tool results (search_symbols / find_usages / smart_context /
// get_callers).
var freshnessPathKeys = map[string]bool{
	"file": true, "file_path": true, "path": true, "filePath": true,
}

// maxFreshnessSweep bounds how many distinct files one list result is swept
// for, so a pathological response can't turn the rider into a hot loop.
const maxFreshnessSweep = 256

// decorateListResultWithFreshness sweeps the file paths a list result
// references and attaches a `freshness` block naming the hits that are stale
// or missing on disk — each carrying its owning repo prefix plus (for stale)
// the indexed SHA / dirty provenance, so the signal is per-repo rather than a
// flat workspace banner. Only JSON-object payloads are touched; GCX / TOON /
// array wire formats the caller opted into pass through unchanged, and a clean
// sweep adds nothing.
func (s *Server) decorateListResultWithFreshness(res *mcp.CallToolResult) *mcp.CallToolResult {
	if res == nil || (s.indexer == nil && s.multiIndexer == nil) {
		return res
	}
	if os.Getenv("GORTEX_NO_FRESHNESS_RIDER") == "1" {
		return res
	}
	text, ok := singleTextContent(res)
	if !ok || text == "" {
		return res
	}
	var asObj map[string]any
	if json.Unmarshal([]byte(text), &asObj) != nil {
		return res // GCX / TOON / array — not a JSON object
	}
	if _, exists := asObj["freshness"]; exists {
		return res
	}

	var staleFiles, missingFiles []map[string]any
	for _, p := range collectGraphFilePaths(asObj) {
		owner, repoRel := s.indexerForRel(p)
		if owner == nil || repoRel == "" {
			continue
		}
		switch owner.TrackedFileState(repoRel) {
		case indexer.FileStale:
			staleFiles = append(staleFiles, s.freshFileProvenance(owner, repoRel, true))
		case indexer.FileMissing:
			missingFiles = append(missingFiles, s.freshFileProvenance(owner, repoRel, false))
		}
	}
	if len(staleFiles) == 0 && len(missingFiles) == 0 {
		return res
	}
	rider := map[string]any{}
	if len(staleFiles) > 0 {
		rider["stale_files"] = staleFiles
		rider["stale_hint"] = "some result files changed on disk since indexing; re-read them before acting on the graph view"
	}
	if len(missingFiles) > 0 {
		rider["missing_files"] = missingFiles
		rider["missing_hint"] = "some result files are recorded in the graph but no longer on disk; they were deleted or moved since indexing"
	}
	asObj["freshness"] = rider
	body, err := json.Marshal(asObj)
	if err != nil {
		return res
	}
	return rebuildTextResult(res, string(body))
}

// freshFileProvenance builds one stale/missing entry: the repo-relative file,
// its owning repo prefix, and — when withSHA — the repo's indexed SHA and
// working-tree-dirty flag. This per-repo provenance is the multi-repo upgrade
// over a single flat staleness banner.
func (s *Server) freshFileProvenance(owner *indexer.Indexer, repoRel string, withSHA bool) map[string]any {
	entry := map[string]any{"file": repoRel}
	if p := owner.RepoPrefix(); p != "" {
		entry["repo"] = p
	}
	if withSHA {
		if r, ok := graph.Store(s.graph).(graph.RepoIndexStateReader); ok {
			if st, found, _ := r.GetRepoIndexState(owner.RepoPrefix()); found {
				if st.IndexedSHA != "" {
					entry["indexed_sha"] = shortFreshSHA(st.IndexedSHA)
				}
				if st.Dirty {
					entry["working_tree_dirty_at_index"] = true
				}
			}
		}
	}
	return entry
}

// collectGraphFilePaths walks a decoded list result and gathers the distinct
// graph-relative file paths referenced under the common path keys, bounded by
// maxFreshnessSweep. Absolute paths and directory values are skipped /
// harmless — they resolve to no tracked file.
func collectGraphFilePaths(v any) []string {
	var out []string
	seen := make(map[string]bool)
	var walk func(any)
	walk = func(node any) {
		if len(out) >= maxFreshnessSweep {
			return
		}
		switch t := node.(type) {
		case map[string]any:
			for k, val := range t {
				if sv, ok := val.(string); ok {
					if freshnessPathKeys[k] && sv != "" && !filepath.IsAbs(sv) && !seen[sv] {
						seen[sv] = true
						out = append(out, sv)
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// decorateResultWithFreshness attaches the freshness rider under the
// "freshness" key for JSON objects and uses the shared line renderer for
// compact formats. An absent rider leaves the response byte-identical.
func decorateResultWithFreshness(res *mcp.CallToolResult, rider map[string]any) *mcp.CallToolResult {
	if len(rider) == 0 {
		return res
	}
	text, ok := singleTextContent(res)
	if !ok || text == "" {
		return res
	}
	var asObj map[string]any
	if json.Unmarshal([]byte(text), &asObj) != nil {
		return renderFreshnessLine(res, rider)
	}
	if existing, ok := asObj["freshness"].(map[string]any); ok {
		for key, value := range rider {
			existing[key] = value
		}
	} else {
		asObj["freshness"] = rider
	}
	body, err := json.Marshal(asObj)
	if err != nil {
		return res
	}
	return rebuildTextResult(res, string(body))
}

func renderFreshnessLine(res *mcp.CallToolResult, fields map[string]any) *mcp.CallToolResult {
	state, _ := fields["graph_freshness"].(string)
	scope, _ := fields["freshness_scope"].(string)
	projected := fields["file_view"] == "projected" && fields["readback_exact"] == true
	if state == "" && projected {
		state, scope = "projected", "file"
	}
	if state == "" {
		if fields["stale"] != true && fields["missing"] != true && fields["index_frozen"] != true {
			return res
		}
		state, scope = "pending", "file"
	}
	if state == "" {
		return res
	}
	if scope == "" {
		scope = "relations"
	}
	count := fields["pending_file_count"]
	if count == nil {
		count = 1
	}
	line := fmt.Sprintf("%s: %s (%v files)", scope, state, count)
	if state == "projected" {
		line = "file: projected (readback exact)"
	}
	body, ok := singleTextContent(res)
	if !ok || strings.HasPrefix(body, line+"\n") {
		return res
	}
	if strings.HasPrefix(body, wire.Tag) {
		body, _ = injectGCXHeaderMeta(body, fields)
		head, rows, _ := strings.Cut(body, wire.RowSep)
		if strings.HasPrefix(rows, wire.CommentPrefix+" "+line+wire.RowSep) {
			return rebuildTextResult(res, body)
		}
		body = head + wire.RowSep + wire.CommentPrefix + " " + line + wire.RowSep + rows
	} else {
		body = line + "\n" + body
	}
	return rebuildTextResult(res, body)
}

// freshnessFor is the single tier producer. Canonical paths are read from
// their watcher; checkout routes retain their own scoped receipt authority.
func (s *Server) freshnessFor(ctx context.Context, scope string, ids, files []string, snapshots ...[]*mutationReceipt) relationFreshness {
	label := relationFreshness{scope: scope}
	var projectedFiles []string
	if len(files) > 0 {
		label.projected = true
		for _, file := range files {
			projected := s.hasRequestProjectionFact(ctx, file)
			label.projected = label.projected && projected
			if scope != "file" && projected {
				projectedFiles = append(projectedFiles, file)
			}
		}
	}
	paths, watched := s.canonicalTierPaths(ctx)
	var receipts []*mutationReceipt
	if !watched {
		receipts = s.laggingMutationReceipts(ctx)
	}
	startup := s.multiIndexer != nil && s.multiIndexer.RelationsPending()
	var snapshot toolFreshnessSnapshot
	if len(snapshots) > 0 {
		snapshot, _ = ctx.Value(toolFreshnessSnapshotKey{}).(toolFreshnessSnapshot)
	}
	deferredFiles := requestDeferredProjectionFiles(ctx)
	storeBusyFiles := requestStoreBusyFiles(ctx)
	gapCount := len(paths) + len(receipts) + len(snapshot.paths) + len(projectedFiles) + len(deferredFiles) + len(storeBusyFiles)
	if len(snapshots) > 0 {
		gapCount += len(snapshots[0])
	}
	if gapCount == 0 {
		if scope != "file" && (startup || snapshot.startup) {
			label.state = "pending"
		}
		return label // idle reads do not resolve seeds or enumerate incoming edges
	}
	inScope := s.relationScope(ctx, ids, files)
	type pathState struct {
		state      string
		generation uint64
	}
	states := make(map[string]pathState)
	put := func(path, state string, generation uint64) {
		if len(ids)+len(files) == 0 {
			if prefix := baseNarrowedRepoPrefix(requestViewFromContext(ctx)); prefix != "" && !strings.HasPrefix(s.repoRelative(path), prefix+"/") {
				return
			}
		}
		if len(ids)+len(files) > 0 && !inScope(path) {
			return
		}
		if scope == "file" && s.hasRequestProjectionFact(ctx, path) {
			return
		}
		key := s.repoRelative(path)
		if prior, exists := states[key]; !exists || generation >= prior.generation {
			states[key] = pathState{state, generation}
		}
	}
	// Exact local projection is independent of background relation/derived
	// publication, including external writes with no watcher receipt. Keep the
	// request's observed gap conservative instead of certifying graph freshness.
	for _, file := range projectedFiles {
		put(file, "pending", 0)
	}
	for file, reason := range deferredFiles {
		put(file, "pending", 0)
		if _, included := states[s.repoRelative(file)]; included {
			label.fileReason = reason
		}
	}
	for _, file := range storeBusyFiles {
		if len(ids)+len(files) > 0 && !inScope(file) {
			continue
		}
		if len(ids)+len(files) == 0 {
			if prefix := baseNarrowedRepoPrefix(requestViewFromContext(ctx)); prefix != "" && !strings.HasPrefix(s.repoRelative(file), prefix+"/") {
				continue
			}
		}
		key := s.repoRelative(file)
		prior := states[key]
		if prior.state != "failed" {
			states[key] = pathState{"pending", prior.generation}
		}
		label.storeBusy = true
	}
	putTiers := func(path string, tiers indexer.MutationTierState) {
		at := tiers.RelationsAt
		switch scope {
		case "file":
			at = tiers.FileGraphAt
		case "derived":
			at = tiers.DerivedAt
		}
		state := ""
		if tiers.Failed {
			state = "failed"
		} else if at.IsZero() || scope == "derived" && tiers.Partial {
			state = "pending"
		}
		if state != "" {
			put(path, state, tiers.Generation)
		}
	}
	if len(snapshots) > 0 {
		if snapshot, ok := ctx.Value(toolFreshnessSnapshotKey{}).(toolFreshnessSnapshot); ok {
			for path, tiers := range snapshot.paths {
				putTiers(path, tiers)
			}
		}
		for _, receipt := range snapshots[0] {
			state := "pending"
			if outcome := receipt.outcome(true); !outcome.Pending && (outcome.Err != nil || !outcome.Reindexed) {
				state = "failed"
			}
			put(receipt.path, state, receipt.generation)
		}
	}
	if watched {
		for path, tiers := range paths {
			putTiers(path, tiers)
		}
	} else {
		for _, receipt := range receipts {
			outcome := receipt.outcome(true)
			if !outcome.Pending && outcome.Err == nil && outcome.Reindexed {
				continue // a receipt can finish between selection and this read
			}
			state := "failed"
			if outcome.Pending {
				state = "pending"
			}
			put(receipt.path, state, receipt.generation)
		}
	}
	for path, state := range states {
		label.files = append(label.files, path)
		if label.state != "failed" {
			label.state = state.state
		}
	}
	slices.Sort(label.files)
	if len(snapshots) > 0 {
		if snapshot, ok := ctx.Value(toolFreshnessSnapshotKey{}).(toolFreshnessSnapshot); ok {
			startup = startup || snapshot.startup
		}
	}
	if label.state == "" && scope != "file" && startup {
		label.state = "pending"
	}
	return label
}

func (s *Server) canonicalTierPaths(ctx context.Context) (map[string]indexer.MutationTierState, bool) {
	watcher, watched := s.currentWatcher().(interface {
		TierPaths() map[string]indexer.MutationTierState
	})
	view := requestViewFromContext(ctx)
	if !watched || view != nil && view.readsOwnCheckout() {
		return nil, false
	}
	return watcher.TierPaths(), true
}

type toolFreshnessSnapshotKey struct{}
type toolFreshnessSnapshot struct {
	receipts []*mutationReceipt
	paths    map[string]indexer.MutationTierState
	startup  bool
}

func (s *Server) captureToolFreshness(ctx context.Context) context.Context {
	receipts := s.laggingMutationReceipts(ctx)
	paths, _ := s.canonicalTierPaths(ctx)
	startup := s.multiIndexer != nil && s.multiIndexer.RelationsPending()
	if len(receipts) == 0 && len(paths) == 0 && !startup {
		if _, exists := ctx.Value(toolFreshnessSnapshotKey{}).(toolFreshnessSnapshot); exists {
			return context.WithValue(ctx, toolFreshnessSnapshotKey{}, toolFreshnessSnapshot{})
		}
		return ctx
	}
	ctx = withRequestProjectionFacts(ctx)
	return context.WithValue(ctx, toolFreshnessSnapshotKey{}, toolFreshnessSnapshot{receipts: receipts, paths: paths, startup: startup})
}

func freshnessTargets(name string, req mcp.CallToolRequest) (scope string, ids, files []string) {
	if file := targetRepoFileRaw(name, req); file != "" {
		switch name {
		case "get_file_summary", "get_editing_context":
			return "relations", nil, []string{file}
		case "get_symbol":
			if req.GetString("detail", "brief") == "full" {
				return "relations", nil, []string{file}
			}
		}
		return "file", nil, []string{file}
	}
	switch name {
	case "search_symbols", "search_text":
		// A query is not a file identity. An explicit tier answer for these
		// tools covers the selected canonical repository's whole graph.
		if requestBoolArg(&req, requireFreshArgName) && !isCompact(req) {
			return "relations", nil, nil
		}
	case "verify_change":
		var changes []struct {
			SymbolID string `json:"symbol_id"`
		}
		if json.Unmarshal([]byte(req.GetString("changes", "")), &changes) == nil {
			for _, change := range changes {
				if change.SymbolID != "" {
					ids = append(ids, change.SymbolID)
				}
			}
		}
		if len(ids) > 0 {
			return "relations", ids, nil
		}
	case "get_edit_plan", "explain_change_impact", "check_guards":
		for _, id := range strings.Split(req.GetString("ids", ""), ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			return "relations", ids, nil
		}
	case "get_callers", "find_usages", "get_call_chain", "trace_call_chain", "get_dependencies", "get_dependents", "find_implementations", "walk_graph":
		for _, key := range []string{"id", "symbol_id", "start_id"} {
			if id := req.GetString(key, ""); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			return "relations", ids, nil
		}
	}
	return "", nil, nil
}

func (s *Server) decorateToolFreshness(ctx context.Context, name string, req mcp.CallToolRequest, res *mcp.CallToolResult) *mcp.CallToolResult {
	// The leaf may have read before a newer physical execution replaced its
	// borrowed source view. Refuse the entire read before labelling/rendering;
	// effectful responses retain their actual disk effects and receipts.
	if res != nil && !res.IsError && lateExactnessRefusalIsSafe(name) {
		if err := validateRequestProjections(ctx); err != nil {
			return mcp.NewToolResultError(err.Error())
		}
	}
	scope, ids, files := freshnessTargets(name, req)
	projected := false
	var label relationFreshness
	if scope != "" {
		if selected := takeRequestFreshness(&req).scope; selected != "" {
			scope = selected
		}
		label = s.freshnessFor(ctx, scope, ids, files)
		if snapshot, ok := ctx.Value(toolFreshnessSnapshotKey{}).(toolFreshnessSnapshot); ok {
			label = s.freshnessFor(ctx, scope, ids, files, snapshot.receipts)
		}
		projected = label.projected
		if label.state != "" || projected {
			fields := make(map[string]any)
			label.apply(fields)
			fields["freshness_scope"] = scope
			res = decorateResultWithFreshness(res, fields)
		}
	}
	beforeFileRiders := res
	if !projected {
		if rider := s.freshnessRiderFor(name, req); rider != nil {
			res = decorateResultWithFreshness(res, rider)
		}
	}
	if isFreshnessListTool(name) {
		res = s.decorateListResultWithFreshness(res)
	}
	// A disk-drift or missing-file rider must not be joined to a fresh claim.
	// Keep the producer's internal zero state unchanged; only an opted-in
	// successful response renders its explicit equivalent.
	if scope != "" && label.state == "" && res != nil && !res.IsError && res == beforeFileRiders {
		label = s.explicitFreshness(ctx, req, label)
		if label.state != "" {
			fields := make(map[string]any)
			label.apply(fields)
			res = decorateResultWithFreshness(res, fields)
		}
	}
	return fitToolFreshnessBudget(ctx, req, res)
}

func (s *Server) explicitFreshness(ctx context.Context, req mcp.CallToolRequest, label relationFreshness) relationFreshness {
	if label.state != "" || !requestBoolArg(&req, requireFreshArgName) || isCompact(req) {
		return label
	}
	// Absence of a local receipt is not proof for another checkout, a source
	// fallback, an immutable view, or an unwatched graph. Route freshness is
	// a different claim and cannot provide this tier authority.
	if _, watched := s.canonicalTierPaths(ctx); watched {
		label.state = "unknown"
		witness, ok := ctx.Value(explicitFreshnessPinKey{}).(*explicitFreshnessWitness)
		if !ok || witness.watcher != s.currentWatcher() || !slices.Equal(witness.prefixes, s.canonicalFreshnessPrefixes(ctx)) {
			return label
		}
		authority, tracked := s.currentWatcher().(interface{ TierAuthority(string) bool })
		if !tracked {
			return label
		}
		inScope := func(file string) bool {
			return slices.ContainsFunc(witness.prefixes, func(prefix string) bool { return strings.HasPrefix(file, prefix+"/") })
		}
		_, ids, files := freshnessTargets(req.Params.Name, req)
		for _, id := range ids {
			file := graph.IDFile(id)
			if file == "" || !inScope(file) {
				return label
			}
		}
		for _, file := range files {
			if !inScope(s.graphRelPath(ctx, file)) {
				return label
			}
		}
		for i, pin := range witness.pins {
			root, known := s.multiIndexer.RepoRoot(witness.prefixes[i])
			if !known || root != witness.roots[i] || s.multiIndexer.GetIndexer(witness.prefixes[i]) != witness.owners[i] || !authority.TierAuthority(witness.prefixes[i]) || pin.ValidateAcceptedCurrent() != nil {
				return label
			}
		}
		label.state = "fresh"
	}
	return label
}

type explicitFreshnessPinKey struct{}

type explicitFreshnessWitness struct {
	watcher  watcherHistory
	prefixes []string
	pins     []*graphview.BasePin
	owners   []*indexer.Indexer
	roots    []string
}

func (s *Server) canonicalFreshnessPrefixes(ctx context.Context) []string {
	view := requestViewFromContext(ctx)
	if OverlayViewFromContext(ctx) != nil || view != nil && (view.readsOwnCheckout() || view.rider != nil && !view.routeless && !view.rider.Exact) {
		return nil
	}
	if prefix := baseNarrowedRepoPrefix(view); prefix != "" {
		return []string{prefix}
	}
	if view != nil && view.reader != nil || s.multiIndexer == nil {
		return nil
	}
	// AUTO can serve the unnarrowed shared corpus. Witness every canonical
	// owner that reader may consult; do not substitute a BASE selector or a
	// narrower query merely to obtain a positive freshness observation.
	prefixes := s.multiIndexer.RepoPrefixes()
	slices.Sort(prefixes)
	return prefixes
}

// Capture accepted source witnesses for the actual canonical reader before
// the read. Projection, editor layers and other checkout/ref readers retain
// their existing scope and cannot borrow these canonical publication facts.
func (s *Server) captureExplicitFreshness(ctx context.Context, req mcp.CallToolRequest) (context.Context, func()) {
	if !requestBoolArg(&req, requireFreshArgName) || isCompact(req) || s.materializer == nil || s.materializer.Leases == nil || s.multiIndexer == nil {
		return ctx, nil
	}
	prefixes := s.canonicalFreshnessPrefixes(ctx)
	authority, tracked := s.currentWatcher().(interface{ TierAuthority(string) bool })
	if len(prefixes) == 0 || !tracked {
		return ctx, nil
	}
	witness := &explicitFreshnessWitness{watcher: s.currentWatcher(), prefixes: prefixes}
	release := func() {
		for _, pin := range witness.pins {
			pin.Release()
		}
	}
	for _, prefix := range prefixes {
		owner := s.multiIndexer.GetIndexer(prefix)
		root, known := s.multiIndexer.RepoRoot(prefix)
		if owner == nil || !known || root == "" || !authority.TierAuthority(prefix) {
			release()
			return ctx, nil
		}
		pin := s.materializer.Leases.AcquireBaseCorpus(prefix)
		witness.pins = append(witness.pins, pin)
		witness.owners = append(witness.owners, owner)
		witness.roots = append(witness.roots, root)
		if pin.ValidateAcceptedCurrent() != nil {
			release()
			return ctx, nil
		}
	}
	return context.WithValue(ctx, explicitFreshnessPinKey{}, witness), release
}

type toolFreshnessBudgetKey struct{}

// Reserve the rider before the existing per-shape fitters choose rows. The
// original cap remains request-local so a late publication gap is fitted too.
func (s *Server) reserveToolFreshnessBudget(ctx context.Context, name string, req mcp.CallToolRequest) (context.Context, mcp.CallToolRequest) {
	if _, reserved := ctx.Value(toolFreshnessBudgetKey{}).(int); reserved {
		return ctx, req
	}
	budget := effectiveBudget(req)
	if budget <= 0 {
		return ctx, req
	}
	scope, ids, files := freshnessTargets(name, req)
	if scope == "" {
		return ctx, req
	}
	if requested := takeRequestFreshness(&req).scope; requested != "" {
		scope = requested
	}
	label := s.freshnessFor(ctx, scope, ids, files)
	if snapshot, ok := ctx.Value(toolFreshnessSnapshotKey{}).(toolFreshnessSnapshot); ok {
		label = s.freshnessFor(ctx, scope, ids, files, snapshot.receipts)
	}
	label = s.explicitFreshness(ctx, req, label)
	if label.state == "" {
		return ctx, req
	}
	reserve := len(label.line()) + 1
	if !isCompact(req) {
		fields := make(map[string]any)
		label.apply(fields)
		encoded, _ := json.Marshal(fields)
		reserve += 2*len(encoded) + 64 // JSON block or escaped GCX header metadata
	}
	args := maps.Clone(req.GetArguments())
	if args == nil {
		args = make(map[string]any)
	}
	args["max_bytes"] = max(1, budget-reserve)
	req.Params.Arguments, req.Params.RawArguments = args, nil
	return context.WithValue(ctx, toolFreshnessBudgetKey{}, budget), req
}

func fitToolFreshnessBudget(ctx context.Context, req mcp.CallToolRequest, res *mcp.CallToolResult) *mcp.CallToolResult {
	budget, reserved := ctx.Value(toolFreshnessBudgetKey{}).(int)
	if !reserved {
		budget = effectiveBudget(req)
	}
	body, ok := singleTextContent(res)
	if !ok || budget <= 0 || len(body) <= budget {
		return res
	}
	var fields map[string]any
	if json.Unmarshal([]byte(body), &fields) == nil && fields != nil {
		rider, labelled := fields["freshness"].(map[string]any)
		if !labelled {
			return res
		}
		fitted, _ := applyBudget(fields, budget)
		encoded, err := json.Marshal(fitted)
		if err == nil && len(encoded) <= budget {
			return rebuildTextResult(res, string(encoded))
		}
		// An indivisible scalar can leave no answer rows to trim. Keep an
		// explicit partial response, including the full pending-file count.
		minimal := map[string]any{"freshness": maps.Clone(rider), budgetTruncatedKey: true}
		minimalRider := minimal["freshness"].(map[string]any)
		delete(minimalRider, "pending_files")
		encoded, err = json.Marshal(minimal)
		if err == nil && len(encoded) <= budget {
			return rebuildTextResult(res, string(encoded))
		}
		return mcp.NewToolResultError("freshness exceeds max_bytes")
	}
	line, _, _ := strings.Cut(body, "\n")
	labelled := strings.HasPrefix(line, "file: ") || strings.HasPrefix(line, "relations: ") || strings.HasPrefix(line, "derived: ")
	if strings.HasPrefix(body, wire.Tag) {
		trimmed, _ := trimGCXBytes([]byte(body), budget)
		if len(trimmed) <= budget {
			return rebuildTextResult(res, string(trimmed))
		}
		return mcp.NewToolResultError("freshness exceeds max_bytes")
	}
	if !labelled {
		return res
	}
	if isCompact(req) || req.GetString("format", "") == "text" {
		return rebuildTextResult(res, line+"\n"+trimTextToBudget(strings.TrimPrefix(body, line+"\n"), max(0, budget-len(line)-1)))
	}
	// TOON has no generic byte-cut operation. A newly admitted gap may have
	// outgrown the reservation; return a labelled refusal rather than a torn table.
	if len(line) <= budget {
		return mcp.NewToolResultError(line)
	}
	return mcp.NewToolResultError("freshness exceeds max_bytes")
}

func (s *Server) awaitWatchedFreshness(ctx context.Context, name string, req mcp.CallToolRequest, freshness requestFreshness) *mcp.CallToolResult {
	view := requestViewFromContext(ctx)
	if !freshness.requireFresh || view != nil && view.readsOwnCheckout() {
		return nil
	}
	if _, ok := s.currentWatcher().(interface {
		TierPaths() map[string]indexer.MutationTierState
	}); !ok {
		return nil
	}
	scope, ids, files := freshnessTargets(name, req)
	if scope == "" {
		return nil
	}
	if freshness.scope != "" {
		scope = freshness.scope
	}
	started := time.Now()
	deadline := freshness.effectiveDeadline(started, ctx)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		label := s.freshnessFor(ctx, scope, ids, files)
		if label.state == "" {
			annotateRequestFreshness(view, &requestFreshnessOutcome{fresh: true, deadline: deadline}, started)
			return nil
		}
		if label.state == "failed" || !time.Now().Before(deadline) {
			label.waited = time.Since(started)
			return label.requireFreshError()
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			label.waited = time.Since(started)
			return label.requireFreshError()
		}
	}
}

func (f relationFreshness) apply(result map[string]any) {
	if f.projected {
		result["file_view"] = "projected"
		result["readback_exact"] = true
	}
	if f.state == "" {
		return
	}
	if f.state != "fresh" {
		result["complete"] = false
	}
	result["graph_freshness"] = f.state
	if f.state == "pending" && f.fileReason != "" {
		result["file_reason"] = f.fileReason
	}
	if f.state == "pending" && f.storeBusy {
		result["reason"] = "store_busy"
	}
	scope := f.scope
	if scope == "" {
		scope = "relations"
	}
	result["freshness_scope"] = scope
	result["pending_file_count"] = len(f.files)
	result["pending_files"] = f.files[:min(maxLabelledPendingFiles, len(f.files))]
	if f.state == "failed" {
		result["recovery_hint"] = mutationFreshnessRecoveryHint()
	}
}

func (f relationFreshness) decorate(res *mcp.CallToolResult) *mcp.CallToolResult {
	if f.state == "" && !f.projected || res == nil || res.IsError {
		return res
	}
	body, ok := singleTextContent(res)
	if !ok || strings.HasPrefix(strings.TrimSpace(body), "{") {
		return res
	}
	fields := make(map[string]any)
	f.apply(fields)
	return renderFreshnessLine(res, fields)
}

func appendSourceFreshness(fields map[string]any, view *requestView) {
	fields["graph_freshness"] = "unknown"
	if view.sourceGraphPending {
		fields["graph_freshness"] = "pending"
	}
}

func attachReceiptTiers(payload map[string]any, receipt *mutationReceipt) {
	if receipt == nil || receipt.ticket == nil {
		return
	}
	attachMutationTierState(payload, receipt.ticket.TierState())
}

// Mutation status and source ACKs render the same immutable producer snapshot.
func attachMutationTierState(payload map[string]any, tiers indexer.MutationTierState) {
	for _, key := range []string{"tier_state", "ack_at", "file_graph_at", "relations_at", "derived_at"} {
		delete(payload, key)
	}
	if tiers.AckAt.IsZero() {
		return
	}
	for key, at := range map[string]time.Time{
		"ack_at": tiers.AckAt, "file_graph_at": tiers.FileGraphAt,
		"relations_at": tiers.RelationsAt, "derived_at": tiers.DerivedAt,
	} {
		if !at.IsZero() {
			payload[key] = at.UTC().Format(time.RFC3339Nano)
		}
	}
	states := make(map[string]string, 3)
	for name, at := range map[string]time.Time{"file_graph": tiers.FileGraphAt, "relations": tiers.RelationsAt, "derived": tiers.DerivedAt} {
		state := "pending"
		if !at.IsZero() {
			state = "fresh"
		}
		if tiers.Failed {
			state = "failed"
		} else if name == "derived" && tiers.Partial {
			state = "partial"
		}
		states[name] = state
	}
	payload["tier_state"] = states
}

// shortFreshSHA trims a git SHA to 12 chars for the rider.
func shortFreshSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// File exactness is physical projection evidence, independent of the ticket's
// graph/relations/derived producer. Copy the map so cached ACK snapshots remain
// immutable when a later replay observes a replacement or invalidation.
func attachMutationFileTier(payload map[string]any, exact bool) {
	states := make(map[string]string, 4)
	if previous, ok := payload["tier_state"].(map[string]string); ok {
		for key, value := range previous {
			states[key] = value
		}
	}
	states["file"] = "pending"
	if exact {
		states["file"] = "fresh"
	}
	payload["tier_state"] = states
}

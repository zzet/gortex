package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
)

// overlayViewCtxKey is the unexported context-key type for the
// per-request OverlaidView. The view is set by the tool-handler
// middleware (`wrapToolHandler`) and read by tool handlers via
// `s.readerFor(ctx)`. Unexported so external code can't smuggle a
// view onto an unrelated context.
type overlayViewCtxKey struct{}
type overlayRequestSnapshotCtxKey struct{}

const (
	overlayRequestSnapshotMaxFiles = exploreSourceLiteralOverlayMaxCoveredFiles + 1
	overlayRequestSnapshotMaxBytes = exploreSourceLiteralOverlayMaxBytes + exploreSourceLiteralOverlayMaxLineBytes
	overlaySimulationInputMaxBytes = exploreSourceLiteralOverlayMaxBytes
	overlaySimulationMaxSteps      = 16

	overlayLayerBaseNodesPerFileMax = 1024
	// BaseNodesMax bounds summaries retained across the layer build. Once the
	// budget is full, a later file may transiently return one emptiness sentinel.
	overlayLayerBaseNodesMax         = 4096
	overlayLayerParsedNodesMax       = 4096
	overlayLayerParsedEdgesMax       = 16384
	overlayLayerParsedResultBytesMax = 16 << 20
	overlayLayerUnresolvedNamesMax   = 1024
)

type overlayLayerLimitError struct {
	Resource string
	Limit    int
}

func (e *overlayLayerLimitError) Error() string {
	return fmt.Sprintf("overlay layer %s exceeds limit %d", e.Resource, e.Limit)
}

func validateBoundedOverlayExtractionUsage(
	result *parser.ExtractionResult,
	usage parser.ExtractionUsage,
	limits parser.ExtractionLimits,
) error {
	if usage.RawNodes < 1 || usage.RawEdges < 0 || usage.StdoutBytes < 0 || usage.ResultBytes < 0 {
		return fmt.Errorf("bounded extractor returned negative or missing usage")
	}
	if usage.StdoutBytes > usage.ResultBytes {
		return fmt.Errorf("bounded extractor result usage is smaller than stdout usage")
	}
	if len(result.Nodes) > usage.RawNodes || len(result.Edges) > usage.RawEdges {
		return fmt.Errorf("bounded extractor result exceeds reported raw usage")
	}
	if usage.RawNodes > limits.MaxNodes {
		return &overlayLayerLimitError{Resource: "parsed nodes", Limit: overlayLayerParsedNodesMax}
	}
	if usage.RawEdges > limits.MaxEdges {
		return &overlayLayerLimitError{Resource: "parsed edges", Limit: overlayLayerParsedEdgesMax}
	}
	if usage.StdoutBytes > int64(limits.MaxStdoutBytes) {
		return &overlayLayerLimitError{Resource: "parsed stdout bytes", Limit: overlayLayerParsedResultBytesMax}
	}
	if usage.ResultBytes > int64(limits.MaxResultBytes) {
		return &overlayLayerLimitError{Resource: "parsed result bytes", Limit: overlayLayerParsedResultBytesMax}
	}
	return nil
}

type stagedOverlayFile struct {
	graphPath string
	deleted   bool
	// repoPrefix / workspaceID / projectID are the repo identity stamped
	// onto every node this file contributes to the layer. All three are
	// resolved once per file at staging time so one layer build cannot
	// hand two nodes of the same file different scope identities.
	repoPrefix  string
	workspaceID string
	projectID   string
	baseNodes   []*graph.Node
	result      *parser.ExtractionResult
}

// overlayRequestSnapshot is the immutable raw-buffer cohort used to build one
// request's OverlaidView. OverlayFile strings are immutable; retaining this
// slice through handler return adds only the copied slice headers and prevents
// a second manager snapshot from observing a different editor state.
type overlayRequestSnapshot struct {
	sessionID string
	workspace string
	files     []daemon.OverlayFile
	canonical bool
}

func withOverlayRequestSnapshot(ctx context.Context, snapshot *overlayRequestSnapshot) context.Context {
	if ctx == nil || snapshot == nil {
		return ctx
	}
	return context.WithValue(ctx, overlayRequestSnapshotCtxKey{}, snapshot)
}

func overlayRequestSnapshotFromContext(ctx context.Context) (*overlayRequestSnapshot, bool) {
	if ctx == nil {
		return nil, false
	}
	snapshot, ok := ctx.Value(overlayRequestSnapshotCtxKey{}).(*overlayRequestSnapshot)
	return snapshot, ok && snapshot != nil
}

// WithOverlayView returns a child context carrying the
// shadow-graph view for the current `tools/call`. Tool handlers
// should not call this directly — `wrapToolHandler` is responsible
// for installing the view based on the calling session's overlay
// state.
func WithOverlayView(ctx context.Context, v *graph.OverlaidView) context.Context {
	if v == nil {
		return ctx
	}
	return context.WithValue(ctx, overlayViewCtxKey{}, v)
}

// OverlayViewFromContext returns the per-request view, or nil when
// the call isn't overlay-active. Tool handlers prefer
// `s.readerFor(ctx)` which folds this with the base graph; direct
// callers (the diff tool) use this when they need both base and
// overlay sides explicitly.
func OverlayViewFromContext(ctx context.Context) *graph.OverlaidView {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(overlayViewCtxKey{}).(*graph.OverlaidView); ok {
		return v
	}
	return nil
}

// readerFor returns the graph.Reader the calling tool handler should
// read through. When ctx carries an overlay view, that view is the
// reader and base is consulted through it. Otherwise the request's
// base reader is returned directly. The single helper keeps every read
// site overlay-aware with one line of plumbing.
//
// Never nil unless the server itself has no graph wired (test-only
// state). Callers that hot-loop on this should hoist the lookup —
// the helper is cheap but the indirection still matters at million-
// edge scales.
func (s *Server) readerFor(ctx context.Context) graph.Reader {
	if v := OverlayViewFromContext(ctx); v != nil {
		return v
	}
	return s.requestBaseReader(ctx)
}

// requestBaseReader is what this request reads when no editor buffer is in
// play, and what a buffer overlay composes on top of: the routed checkout
// view when the request resolved to one, the indexed corpus otherwise.
func (s *Server) requestBaseReader(ctx context.Context) graph.Reader {
	if view := requestViewFromContext(ctx); view.routed() {
		return view.reader
	}
	return s.graph
}

// nodeGetterFor returns the request-scoped node lookup: the session
// overlay view when one rides the context, the base graph otherwise.
// Output classification (is_test labels, usage summary, flavor
// resolution) must read the same view engineFor's query ran on, or an
// overlay-only owner classifies differently in the filter and the rows.
func (s *Server) nodeGetterFor(ctx context.Context) graph.NodeGetter {
	return s.readerFor(ctx)
}

// engineFor returns the query engine scoped to the calling request's
// graph reader. For non-overlay calls this is `s.engine` unchanged;
// for overlay-active calls the returned engine reads through the
// overlay view, so engine-level walks (`FindUsages`, `GetCallers`,
// `GetCallChain`, dependency / dependent walkers, hierarchy …) all
// see the editor-buffer state automatically.
//
// Cheap: WithReader is a shallow clone (one struct copy, shares
// search provider and rerank pipeline). Safe to call inside hot
// tool-handler paths.
// A routed request additionally hands the engine the stack it reads
// through, so candidate enumeration covers every generation's corpus
// and not just the indexed one underneath them. The layer slice is nil
// on a base request and WithViewLayers is then WithReader exactly, so
// nothing on that path changes.
func (s *Server) engineFor(ctx context.Context) *query.Engine {
	if s == nil || s.engine == nil {
		return nil
	}
	view := requestViewFromContext(ctx)
	if v := OverlayViewFromContext(ctx); v != nil {
		return s.engine.WithViewLayers(v, view.candidateLayers())
	}
	if view.routed() {
		return s.engine.WithViewLayers(view.reader, view.candidateLayers())
	}
	return s.engine
}

// overlayLayerCacheEntry is one (sessionID, content-hash sum) bucket
// in s.overlayLayerCache. The hash is over (sorted) overlay files'
// (path, content, deleted, base_sha) tuples; identical pushes from a
// long sequence of tool calls hit the same entry and reuse the same
// parsed layer without re-running the per-language extractors.
type overlayLayerCacheEntry struct {
	hash  string
	layer *graph.OverlayLayer
	// Files captured in the entry, for invalidation lookups and for
	// the diff tool's enumeration.
	files []string
}

func (s *Server) snapshotOverlayRequestForCtx(ctx context.Context) (*overlayRequestSnapshot, error) {
	if s == nil || s.overlays == nil {
		return nil, nil
	}
	sessionID := OverlayCohortIDFromContext(ctx)
	if sessionID == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workspace, files, err := s.overlays.SnapshotForBounded(
		sessionID,
		overlayRequestSnapshotMaxFiles,
		overlayRequestSnapshotMaxBytes,
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		if !errors.Is(err, daemon.ErrSessionNotFound) {
			return nil, err
		}
		// Pin the attempted empty cohort. A nested facade must not retry and
		// accidentally observe buffers pushed after the outer request began.
		return &overlayRequestSnapshot{sessionID: sessionID}, nil
	}
	return &overlayRequestSnapshot{sessionID: sessionID, workspace: workspace, files: files}, nil
}

// prepareOverlayRequest pins one manager snapshot and the graph view derived
// from it onto the request context. Reusing an already-prepared context is
// idempotent, which keeps nested facade calls on the same editor-buffer cohort.
func (s *Server) prepareOverlayRequest(ctx context.Context) (context.Context, *graph.OverlaidView, error) {
	if ctx == nil {
		return ctx, nil, nil
	}
	if !requestViewFromContext(ctx).acceptsBufferOverlay() {
		if OverlayViewFromContext(ctx) != nil {
			return ctx, nil, fmt.Errorf("editor overlay is forbidden on a read-only grace fallback")
		}
		return ctx, nil, nil
	}
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if ok && snapshot.sessionID != OverlayCohortIDFromContext(ctx) {
		return ctx, nil, fmt.Errorf("overlay request snapshot belongs to session %q, not %q", snapshot.sessionID, OverlayCohortIDFromContext(ctx))
	}
	if !ok {
		if OverlayViewFromContext(ctx) != nil {
			return ctx, nil, fmt.Errorf("overlay view has no pinned request snapshot")
		}
		var err error
		snapshot, err = s.snapshotOverlayRequestForCtx(ctx)
		if err != nil {
			return ctx, nil, err
		}
	}
	if snapshot == nil {
		return ctx, OverlayViewFromContext(ctx), nil
	}
	if view := OverlayViewFromContext(ctx); view != nil && !snapshot.canonical {
		return ctx, nil, fmt.Errorf("overlay view has a non-canonical request snapshot")
	}
	if err := s.canonicalizeOverlayRequestSnapshot(ctx, snapshot); err != nil {
		return ctx, nil, err
	}
	ctx = withOverlayRequestSnapshot(ctx, snapshot)
	if view := OverlayViewFromContext(ctx); view != nil {
		return ctx, view, nil
	}
	view, err := s.buildOverlayViewForCtx(ctx)
	if err != nil {
		return ctx, nil, err
	}
	if view != nil {
		ctx = WithOverlayView(ctx, view)
	}
	return ctx, view, nil
}

func canonicalOverlayGraphPath(candidate string) string {
	candidate = strings.TrimSpace(strings.ReplaceAll(candidate, "\\", "/"))
	if candidate == "" {
		return ""
	}
	candidate = path.Clean(candidate)
	if candidate == "." {
		return ""
	}
	return candidate
}

// canonicalizeOverlayRequestSnapshot validates and normalizes the pinned raw
// buffer cohort exactly once. Aliases resolving to one graph path collapse
// only when their replacement state is identical. SnapshotFor does not retain
// push chronology, so conflicting aliases fail closed instead of guessing
// which editor state is newer.
func (s *Server) canonicalizeOverlayRequestSnapshot(ctx context.Context, snapshot *overlayRequestSnapshot) error {
	if snapshot == nil || snapshot.canonical {
		return nil
	}
	if len(snapshot.files) == 0 {
		snapshot.canonical = true
		return nil
	}

	sort.SliceStable(snapshot.files, func(i, j int) bool {
		return snapshot.files[i].Path < snapshot.files[j].Path
	})
	type canonicalRecord struct {
		file    daemon.OverlayFile
		rawPath string
	}
	byPath := make(map[string]canonicalRecord, len(snapshot.files))
	for _, file := range snapshot.files {
		rawPath := file.Path
		absPath, err := s.resolveOverlayAbsPathForRequest(ctx, rawPath)
		if err != nil {
			return err
		}
		owner := s.pickIndexerForRequestPath(ctx, absPath)
		if absPath == "" || owner == nil {
			return fmt.Errorf("overlay path %q is outside the registered workspace", rawPath)
		}
		if snapshot.workspace != "" && owner.WorkspaceID() != snapshot.workspace {
			return fmt.Errorf("overlay path %q belongs to workspace %q, not registered workspace %q", rawPath, owner.WorkspaceID(), snapshot.workspace)
		}
		graphPath := canonicalOverlayGraphPath(s.resolveOverlayGraphPathForRequest(ctx, rawPath, absPath))
		if graphPath == "" {
			return fmt.Errorf("overlay path %q has no canonical graph path", rawPath)
		}
		if existing, ok := byPath[graphPath]; ok {
			if existing.file.Content != file.Content || existing.file.Deleted != file.Deleted || existing.file.BaseSHA != file.BaseSHA {
				return fmt.Errorf("conflicting overlay aliases %q and %q resolve to %q", existing.rawPath, rawPath, graphPath)
			}
			continue
		}
		file.Path = graphPath
		byPath[graphPath] = canonicalRecord{file: file, rawPath: rawPath}
	}

	paths := make([]string, 0, len(byPath))
	for graphPath := range byPath {
		paths = append(paths, graphPath)
	}
	sort.Strings(paths)
	files := make([]daemon.OverlayFile, 0, len(paths))
	for _, graphPath := range paths {
		files = append(files, byPath[graphPath].file)
	}
	snapshot.files = files
	snapshot.canonical = true
	return nil
}

// buildOverlayViewForCtx consumes only the raw snapshot installed by request
// preparation, ensuring parsing and later raw-buffer reads observe one cohort.
func (s *Server) buildOverlayViewForCtx(ctx context.Context) (*graph.OverlaidView, error) {
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || snapshot == nil || len(snapshot.files) == 0 {
		return nil, nil
	}
	if !snapshot.canonical {
		return nil, fmt.Errorf("overlay request snapshot is not canonical")
	}
	files := snapshot.files
	sessID := OverlayCohortIDFromContext(ctx)

	// Drift check up front for every overlay that carries a BaseSHA.
	// We do it here, before parsing, so a stale overlay never costs
	// the extractor time and the client gets a clear error.
	for _, ov := range files {
		if ov.BaseSHA == "" {
			continue
		}
		abs, resolveErr := s.resolveOverlayAbsPathForRequest(ctx, ov.Path)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if abs == "" {
			continue
		}
		if !overlaySHAMatches(abs, ov.BaseSHA) {
			return nil, fmt.Errorf("%w: %s", daemon.ErrOverlayDrift, ov.Path)
		}
	}

	hash := hashOverlayFiles(files)
	// The buffer layer composes over whatever answers THIS request — the
	// indexed corpus, or the routed checkout view the session's cwd bound to.
	// The parsed layer itself is base-independent, which is what lets the
	// cache below stay keyed by content alone.
	base := s.requestBaseReader(ctx)

	// Cache hit: same session pushed the same buffers; reuse the
	// parsed layer. The cache stores up to one entry per session; a
	// changed content hash evicts the prior entry.
	if v, ok := s.overlayLayerCache.Load(sessID); ok {
		if entry := v.(*overlayLayerCacheEntry); entry.hash == hash {
			return graph.NewOverlaidView(base, entry.layer), nil
		}
		s.overlayLayerCache.Delete(sessID)
	}

	// Cache miss: parse + resolve under a per-server build mutex so
	// two requests for the same fresh content don't duplicate the
	// extractor work. We re-check the cache under the lock.
	s.overlayLayerBuildMu.Lock()
	defer s.overlayLayerBuildMu.Unlock()
	if v, ok := s.overlayLayerCache.Load(sessID); ok {
		if entry := v.(*overlayLayerCacheEntry); entry.hash == hash {
			return graph.NewOverlaidView(base, entry.layer), nil
		}
	}

	layer, paths, err := s.constructOverlayLayer(ctx, files)
	if err != nil {
		return nil, err
	}
	if layer == nil {
		return nil, nil
	}
	s.overlayLayerCache.Store(sessID, &overlayLayerCacheEntry{
		hash:  hash,
		layer: layer,
		files: paths,
	})
	return graph.NewOverlaidView(base, layer), nil
}

// resolveOverlayAbsPath turns the overlay's caller-supplied path into
// the absolute filesystem path the indexer would have used. Accepts
// absolute paths, repo-prefixed paths (multi-repo), and repo-relative
// paths (single-repo). Returns ("", nil) for paths that don't
// resolve to any tracked workspace — those overlays are silently
// skipped, matching how the on-disk path treats untracked files.
func (s *Server) resolveOverlayAbsPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("overlay path is empty")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if s.multiIndexer != nil {
		if abs := s.multiIndexer.ResolveFilePath(p); abs != "" {
			return abs, nil
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			return filepath.Join(root, p), nil
		}
	}
	return "", nil
}

// resolveOverlayAbsPathForRequest resolves an editor path against the working
// copy selected for this request. The legacy helper above remains for
// view-independent administrative callers; buffer canonicalization, BaseSHA
// checks, parsing, and raw substitution must all use this one coherent root.
func (s *Server) resolveOverlayAbsPathForRequest(ctx context.Context, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("overlay path is empty")
	}
	if filepath.IsAbs(p) {
		abs := filepath.Clean(p)
		if err := requestViewPathRoot(ctx).validateAbsolute(abs); err != nil {
			return "", err
		}
		return abs, nil
	}
	abs, _, err := s.resolveFilePath(ctx, p)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// resolveOverlayGraphPath turns the overlay path into the
// `graph_path` form (repo-prefixed in multi-repo mode, repo-relative
// in single-repo mode) — the form `GetFileNodes` and friends use.
func (s *Server) resolveOverlayGraphPath(p, absPath string) string {
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			if idx, _ := s.multiIndexer.IndexerForFile(absPath); idx != nil {
				if root := idx.RootPath(); root != "" {
					if rel, ok := relativeWithinRoot(root, absPath); ok {
						return prefix + "/" + filepath.ToSlash(rel)
					}
				}
			}
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if rel, ok := relativeWithinRoot(root, absPath); ok {
				return filepath.ToSlash(rel)
			}
		}
	}
	// Fall back to caller-supplied path — this is the single-repo
	// repo-relative case where p is already the graph_path.
	return filepath.ToSlash(p)
}

// resolveOverlayGraphPathForRequest maps a file beneath the selected checkout
// back to the shared graph spelling. A routed worktree is intentionally not a
// registered MultiIndexer root, so the legacy owner lookup cannot derive this
// identity from the absolute path alone.
func (s *Server) resolveOverlayGraphPathForRequest(ctx context.Context, p, absPath string) string {
	view := requestViewPathRoot(ctx)
	if view.root != "" && view.contains(absPath) {
		if rel, ok := relativeWithinRoot(view.root, absPath); ok {
			rel = filepath.ToSlash(rel)
			if view.repoPrefix != "" {
				return path.Join(view.repoPrefix, rel)
			}
			return rel
		}
	}
	return s.resolveOverlayGraphPath(p, absPath)
}

// pickIndexerForPath chooses the per-repo Indexer (multi-repo) or
// the single Indexer (single-repo) that owns absPath. Returns nil
// when no Indexer owns the path (the overlay is silently skipped
// upstream).
func (s *Server) pickIndexerForPath(absPath string) *indexer.Indexer {
	if s.multiIndexer != nil {
		if idx, _ := s.multiIndexer.IndexerForFile(absPath); idx != nil {
			return idx
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if _, ok := relativeWithinRoot(root, absPath); ok {
				return s.indexer
			}
		}
	}
	return nil
}

// pickIndexerForRequestPath returns the canonical indexer that owns a file in
// the selected worktree. The indexer supplies parser/config identity; the file
// bytes still come from the routed checkout resolved above.
func (s *Server) pickIndexerForRequestPath(ctx context.Context, absPath string) *indexer.Indexer {
	view := requestViewPathRoot(ctx)
	if view.root != "" && view.contains(absPath) {
		if s.multiIndexer != nil && view.repoPrefix != "" {
			if root, ok := s.multiIndexer.RepoRoot(view.repoPrefix); ok {
				if idx, _ := s.multiIndexer.IndexerForFile(root); idx != nil {
					return idx
				}
			}
		}
		if s.indexer != nil {
			return s.indexer
		}
	}
	return s.pickIndexerForPath(absPath)
}

// constructOverlayLayer is the parse-and-resolve pass. For each
// overlay file:
//
//   - Resolve the absolute and graph-prefixed paths.
//   - For deleted: true overlays, mark the graph path as a tombstone
//     and continue (no parsing).
//   - For content overlays, look up the per-language extractor via
//     the indexer's parser.Registry and call Extract on the buffer.
//   - Apply the repo prefix to the result so IDs match base's
//     `<repo-prefix>/<file>::<symbol>` shape.
//   - Add the parsed nodes + edges to the layer.
//   - Track removed-by-overlay symbols (names that existed in base's
//     view of the file but disappeared) so FindNodesByName drops
//     those base hits.
//
// After parsing all files, run a local resolver pass that rewrites
// every `unresolved::*` edge in the overlay to point at a real node
// when one exists in `(layer ∪ base)`. The pass is conservative —
// simple name resolution — but covers the common cases: direct
// function calls, method calls in the same file, intra-package
// references.
func (s *Server) constructOverlayLayer(ctx context.Context, files []daemon.OverlayFile) (*graph.OverlayLayer, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateOverlayBuildEnvelope(files); err != nil {
		return nil, nil, err
	}
	if s.graph == nil {
		return nil, nil, nil
	}

	// Stage every bounded read and extraction before constructing the layer.
	// Extractors may reuse result pointers, so no prefix or graph mutation is
	// allowed until every file has passed its caps and cancellation checks.
	baseReader, _ := s.graph.(graph.BoundedFileNodeReader)
	staged := make([]stagedOverlayFile, 0, len(files))
	coveredPaths := make([]string, 0, len(files))
	baseNodeCount := 0
	parsedNodeCount := 0
	parsedEdgeCount := 0
	parsedResultBytes := int64(0)

	for _, ov := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		absPath, err := s.resolveOverlayAbsPathForRequest(ctx, ov.Path)
		if err != nil {
			return nil, nil, err
		}
		if absPath == "" {
			continue // untracked file — silent skip, matches disk path
		}
		graphPath := s.resolveOverlayGraphPathForRequest(ctx, ov.Path, absPath)
		coveredPaths = append(coveredPaths, graphPath)

		if ov.Deleted {
			baseNodes, err := readOverlayBaseNodes(ctx, baseReader, graphPath, &baseNodeCount)
			if err != nil {
				return nil, nil, err
			}
			staged = append(staged, stagedOverlayFile{
				graphPath: graphPath,
				deleted:   true,
				baseNodes: baseNodes,
			})
			continue
		}

		idx := s.pickIndexerForRequestPath(ctx, absPath)
		if idx == nil {
			continue
		}
		reg := idx.Registry()
		if reg == nil {
			continue
		}
		content := []byte(ov.Content)
		lang, ok := reg.DetectLanguageContent(absPath, content)
		if !ok {
			continue
		}
		root := idx.RootPath()
		relPath := graphPath
		if idx.RepoPrefix() != "" {
			relPath = strings.TrimPrefix(graphPath, idx.RepoPrefix()+"/")
		} else if root != "" {
			if r, ok := relativeWithinRoot(root, absPath); ok {
				relPath = filepath.ToSlash(r)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		var (
			result        *parser.ExtractionResult
			extractErr    error
			boundedUsage  parser.ExtractionUsage
			boundedLimits parser.ExtractionLimits
			bounded       bool
		)
		ext, extractorOK := reg.GetByLanguage(lang)
		if boundedExtractor, isBounded := ext.(parser.BoundedExtractor); extractorOK && isBounded {
			bounded = true
			boundedLimits = parser.DefaultExtractionLimits()
			boundedLimits.MaxNodes = overlayLayerParsedNodesMax - parsedNodeCount
			boundedLimits.MaxEdges = overlayLayerParsedEdgesMax - parsedEdgeCount
			remainingResultBytes := int64(overlayLayerParsedResultBytesMax) - parsedResultBytes
			boundedLimits.MaxStdoutBytes = int(remainingResultBytes)
			boundedLimits.MaxResultBytes = int(remainingResultBytes)
			result, boundedUsage, extractErr = boundedExtractor.ExtractBounded(
				ctx, relPath, content, boundedLimits,
			)
		} else {
			// Legacy extractors go through the indexer's admission lifecycle
			// so overlay parses share its crash isolation.
			result, extractErr = idx.ExtractBuffer(lang, relPath, content)
		}
		if result != nil {
			// Overlay construction never retains parse trees or constant-value
			// sidecars. Release/clear before every error and cancellation branch.
			result.ReleaseTree()
			result.ConstValues = nil
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if extractErr != nil {
			return nil, nil, fmt.Errorf("overlay parse %s: %w", ov.Path, extractErr)
		}
		if result == nil {
			return nil, nil, fmt.Errorf("overlay parse %s: extractor returned nil result", ov.Path)
		}
		if bounded {
			if err := validateBoundedOverlayExtractionUsage(result, boundedUsage, boundedLimits); err != nil {
				return nil, nil, fmt.Errorf("overlay parse %s: %w", ov.Path, err)
			}
		}

		// Keep the raw returned-slice checks as defense in depth for both optional
		// bounded implementations and trusted legacy extractors.
		if len(result.Nodes) > overlayLayerParsedNodesMax-parsedNodeCount {
			return nil, nil, &overlayLayerLimitError{
				Resource: "parsed nodes",
				Limit:    overlayLayerParsedNodesMax,
			}
		}
		if len(result.Edges) > overlayLayerParsedEdgesMax-parsedEdgeCount {
			return nil, nil, &overlayLayerLimitError{
				Resource: "parsed edges",
				Limit:    overlayLayerParsedEdgesMax,
			}
		}
		if bounded {
			parsedNodeCount += boundedUsage.RawNodes
			parsedEdgeCount += boundedUsage.RawEdges
			parsedResultBytes += boundedUsage.ResultBytes
		} else {
			parsedNodeCount += len(result.Nodes)
			parsedEdgeCount += len(result.Edges)
		}
		baseNodes, err := readOverlayBaseNodes(ctx, baseReader, graphPath, &baseNodeCount)
		if err != nil {
			return nil, nil, err
		}
		workspaceID, projectID := overlayScopeIdentity(baseNodes, graphPath, idx)
		staged = append(staged, stagedOverlayFile{
			graphPath:   graphPath,
			repoPrefix:  idx.RepoPrefix(),
			workspaceID: workspaceID,
			projectID:   projectID,
			baseNodes:   baseNodes,
			result:      cloneOverlayExtractionResult(result),
		})
	}

	if len(coveredPaths) == 0 {
		return nil, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	sort.Strings(coveredPaths)

	layer := graph.NewOverlayLayer()
	for index, file := range staged {
		if index&15 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		if file.deleted {
			for _, node := range file.baseNodes {
				layer.MarkRemoved(node.Name, node.ID)
			}
			layer.MarkFile(file.graphPath, true)
			continue
		}

		// The staged result is an owned, unstamped clone captured before the
		// next Extract call. Stamp it only after the full cohort preflight.
		result := file.result
		applyRepoIdentityToResult(result, file.repoPrefix, file.workspaceID, file.projectID)

		baseIDsByName := make(map[string]map[string]bool)
		for _, node := range file.baseNodes {
			set := baseIDsByName[node.Name]
			if set == nil {
				set = make(map[string]bool)
				baseIDsByName[node.Name] = set
			}
			set[node.ID] = true
		}
		overlayIDsByName := make(map[string]map[string]bool)
		layer.MarkFile(file.graphPath, false)
		if result != nil {
			for _, node := range result.Nodes {
				if node == nil {
					continue
				}
				layer.AddNode(file.graphPath, node)
				set := overlayIDsByName[node.Name]
				if set == nil {
					set = make(map[string]bool)
					overlayIDsByName[node.Name] = set
				}
				set[node.ID] = true
			}
			for _, edge := range result.Edges {
				layer.AddEdge(edge)
			}
		}
		for name, baseIDs := range baseIDsByName {
			overlayIDs := overlayIDsByName[name]
			for id := range baseIDs {
				if !overlayIDs[id] {
					layer.MarkRemoved(name, id)
				}
			}
		}
	}

	if err := resolveOverlayEdges(ctx, s.graph, layer); err != nil {
		return nil, nil, err
	}
	return layer, coveredPaths, nil
}

func readOverlayBaseNodes(
	ctx context.Context,
	reader graph.BoundedFileNodeReader,
	graphPath string,
	total *int,
) ([]*graph.Node, error) {
	if reader == nil {
		return nil, fmt.Errorf("overlay base identities for %s: %w", graphPath, graph.ErrBoundedLocalizationUnavailable)
	}
	if total == nil || *total < 0 || *total > overlayLayerBaseNodesMax {
		return nil, fmt.Errorf("overlay base identities for %s: invalid aggregate count", graphPath)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	remaining := overlayLayerBaseNodesMax - *total
	queryLimit := overlayLayerBaseNodesPerFileMax
	aggregateLimited := remaining < queryLimit
	if aggregateLimited {
		queryLimit = remaining
		if queryLimit == 0 {
			// A one-row emptiness sentinel lets an actually empty later file pass
			// without ever issuing an invalid limit-zero projection.
			queryLimit = 1
		}
	}
	projection, err := reader.FindFileNodesBounded(
		ctx,
		graphPath,
		graph.LocalizationNodeScope{},
		queryLimit,
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("overlay base identities for %s: %w", graphPath, err)
	}
	if projection.Truncated || projection.Total > queryLimit || len(projection.Nodes) > queryLimit {
		resource := "base nodes per file"
		limit := overlayLayerBaseNodesPerFileMax
		if aggregateLimited {
			resource = "base nodes"
			limit = overlayLayerBaseNodesMax
		}
		return nil, &overlayLayerLimitError{Resource: resource, Limit: limit}
	}
	if projection.Total < 0 || projection.Total != len(projection.Nodes) {
		return nil, fmt.Errorf("overlay base identities for %s: invalid bounded projection", graphPath)
	}
	if projection.Total > remaining {
		return nil, &overlayLayerLimitError{
			Resource: "base nodes",
			Limit:    overlayLayerBaseNodesMax,
		}
	}
	for _, node := range projection.Nodes {
		if node == nil {
			return nil, fmt.Errorf("overlay base identities for %s: invalid nil node", graphPath)
		}
	}
	*total += projection.Total
	return projection.Nodes, nil
}

func cloneOverlayExtractionResult(result *parser.ExtractionResult) *parser.ExtractionResult {
	if result == nil {
		return nil
	}
	cloned := &parser.ExtractionResult{
		Nodes: make([]*graph.Node, len(result.Nodes)),
		Edges: make([]*graph.Edge, len(result.Edges)),
	}
	for index, node := range result.Nodes {
		if node == nil {
			continue
		}
		copy := *node
		copy.Meta = cloneOverlayMetadata(node.Meta)
		cloned.Nodes[index] = &copy
	}
	for index, edge := range result.Edges {
		if edge == nil {
			continue
		}
		copy := *edge
		copy.Meta = cloneOverlayMetadata(edge.Meta)
		cloned.Edges[index] = &copy
	}
	return cloned
}

func cloneOverlayMetadata(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	cloned := make(map[string]any, len(meta))
	for key, value := range meta {
		cloned[key] = value
	}
	return cloned
}

func validateOverlayBuildEnvelope(files []daemon.OverlayFile) error {
	if len(files) > overlayRequestSnapshotMaxFiles {
		return overlayBuildEnvelopeError("files", overlayRequestSnapshotMaxFiles)
	}
	totalBytes := 0
	for _, file := range files {
		if err := addOverlayBuildFileBytes(&totalBytes, file); err != nil {
			return err
		}
	}
	return nil
}

// validateOverlayBuildMapEnvelope checks simulation state before it is copied
// into a retained, sorted snapshot. Keeping this map-shaped seam avoids
// materializing an already-oversized state merely to discover its size.
func validateOverlayBuildMapEnvelope(files map[string]daemon.OverlayFile) error {
	if len(files) > overlayRequestSnapshotMaxFiles {
		return overlayBuildEnvelopeError("files", overlayRequestSnapshotMaxFiles)
	}
	totalBytes := 0
	for _, file := range files {
		if err := addOverlayBuildFileBytes(&totalBytes, file); err != nil {
			return err
		}
	}
	return nil
}

func addOverlayBuildFileBytes(totalBytes *int, file daemon.OverlayFile) error {
	for _, size := range [...]int{len(file.Path), len(file.Content), len(file.BaseSHA)} {
		remaining := overlayRequestSnapshotMaxBytes - *totalBytes
		if size > remaining {
			return overlayBuildEnvelopeError("bytes", overlayRequestSnapshotMaxBytes)
		}
		*totalBytes += size
	}
	return nil
}

func overlayBuildEnvelopeError(resource string, limit int) error {
	return &daemon.ErrOverlaySnapshotTooLarge{Resource: resource, Limit: limit}
}

// requestContextError preserves request transport cancellation instead of
// converting it into an MCP tool-error payload. Only the parent request context
// is authoritative: component-local cancellation remains an ordinary tool error.
func requestContextError(ctx context.Context, _ error) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// applyRepoIdentityToResult stamps one repository's identity onto every
// node/edge in an extraction result: the repo prefix on IDs and file paths
// so they match base's shape in multi-repo mode, and the workspace /
// project slugs so the nodes carry the same scope identity as the base
// nodes of that repo. Mirrors `Indexer.applyRepoPrefix` but kept here to
// avoid having to expose that helper to non-indexer packages.
//
// Prefix and slugs are independent. A repo can declare a workspace without
// being prefixed (single-repo mode), and the base path stamps the slugs
// before its own prefix check for exactly that reason. Stamping only the
// prefix leaves the node with an empty WorkspaceID, which every reader's
// scope gate reads as "no workspace declared" and falls back to the repo
// prefix — so an overlay node in a repo whose workspace slug differs from
// its prefix is dropped by any scope-narrowed read.
//
// Like the base path, a slug is written only where the extractor left the
// field empty, so a node that already carries an identity keeps it.
func applyRepoIdentityToResult(result *parser.ExtractionResult, repoPrefix, workspaceID, projectID string) {
	if result == nil {
		return
	}
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		if workspaceID != "" && n.WorkspaceID == "" {
			n.WorkspaceID = workspaceID
		}
		if projectID != "" && n.ProjectID == "" {
			n.ProjectID = projectID
		}
	}
	if repoPrefix == "" {
		return
	}
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		n.ID = repoPrefix + "/" + n.ID
		if n.FilePath != "" {
			n.FilePath = repoPrefix + "/" + n.FilePath
		}
		n.RepoPrefix = repoPrefix
	}
	for _, e := range result.Edges {
		if e == nil {
			continue
		}
		e.From = repoPrefix + "/" + e.From
		if !strings.HasPrefix(e.To, unresolvedPrefix) {
			e.To = repoPrefix + "/" + e.To
		}
	}
}

// overlayScopeIdentity resolves the workspace / project slugs one overlay
// file's nodes must carry. It runs once per staged file, so every node the
// file contributes to the layer gets the same pair.
//
// Precedence, highest first:
//
//  1. The base file node for this path. It is what the repo's own indexer
//     stamped for this exact file, so copying it keeps the buffer and the
//     file it shadows on the same side of every scope boundary.
//  2. The base sibling with the smallest ID among those carrying a
//     workspace slug — for a file whose extractor emits no file node. The
//     smallest ID makes the pick independent of projection order.
//  3. The repo indexer's own binding, which is the value
//     `Indexer.applyRepoPrefix` stamps at indexing time. This is the only
//     source for a file that is new in the buffer and has no base nodes.
//
// Both slugs come from a single source. Mixing them could place a node in
// one stamping generation's workspace under another's project slug, and a
// project narrow would then drop it.
func overlayScopeIdentity(baseNodes []*graph.Node, graphPath string, idx *indexer.Indexer) (workspaceID, projectID string) {
	var donor *graph.Node
	for _, node := range baseNodes {
		if node == nil || node.WorkspaceID == "" {
			continue
		}
		if node.ID == graphPath {
			donor = node
			break
		}
		if donor == nil || node.ID < donor.ID {
			donor = node
		}
	}
	if donor != nil {
		return donor.WorkspaceID, donor.ProjectID
	}
	if idx == nil {
		return "", ""
	}
	return idx.WorkspaceID(), idx.ProjectID()
}

// unresolvedPrefix matches the resolver's prefix for placeholder
// edge targets. Kept as a package constant so resolveOverlayEdges
// can recognise placeholders without importing the resolver package
// (which would create a layering cycle through the indexer).
const unresolvedPrefix = "unresolved::"

// resolveOverlayEdges runs a conservative local resolver pass over
// the overlay layer's edges. For each placeholder `unresolved::*`
// edge:
//
//   - Strip the `unresolved::<kind>::` prefix to recover the target
//     name (and optional fully-qualified suffix).
//   - Look the name up in (layer.nodesByName ∪ base.FindNodesByName).
//     Prefer overlay matches over base; prefer a single unambiguous
//     match over multiple.
//   - On a unique match, rewrite the edge's To and re-index it in
//     the layer's outEdges / inEdges maps.
//
// The pass deliberately does NOT replicate the full resolver
// (interface dispatch, import-path attribution, dataflow, etc.) —
// overlay buffers are transient and the common case the editor
// cares about is "I added a call to Foo; does find_usages of Foo
// now include this site?" Direct name resolution covers that.
func resolveOverlayEdges(ctx context.Context, base graph.Reader, layer *graph.OverlayLayer) error {
	if layer == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	edgesByFrom := layer.OutEdgesByFromAll()
	names := make(map[string]struct{})
	inspected := 0
	for _, edges := range edgesByFrom {
		for _, edge := range edges {
			inspected++
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if edge == nil {
				continue
			}
			name := overlayUnresolvedTargetName(edge.To)
			if name == "" {
				continue
			}
			if _, exists := names[name]; exists {
				continue
			}
			if len(names) >= overlayLayerUnresolvedNamesMax {
				return &overlayLayerLimitError{
					Resource: "unresolved names",
					Limit:    overlayLayerUnresolvedNamesMax,
				}
			}
			names[name] = struct{}{}
		}
	}

	orderedNames := make([]string, 0, len(names))
	for name := range names {
		orderedNames = append(orderedNames, name)
	}
	sort.Strings(orderedNames)
	resolvedByName := make(map[string]string, len(orderedNames))
	view := graph.NewOverlaidView(base, layer)
	for _, name := range orderedNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		overlay := layer.NodesByName(name)
		if len(overlay) == 1 {
			if overlay[0] == nil {
				return fmt.Errorf("overlay target %q: invalid nil overlay node", name)
			}
			resolvedByName[name] = overlay[0].ID
			continue
		}
		if len(overlay) > 1 {
			continue
		}

		projection, err := view.FindNodesByNameBounded(
			ctx,
			name,
			graph.LocalizationNodeScope{},
			1,
		)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return fmt.Errorf("overlay target %q: %w", name, err)
		}
		if projection.Total < 0 || projection.Total < len(projection.Nodes) || len(projection.Nodes) > 1 {
			return fmt.Errorf("overlay target %q: invalid bounded projection", name)
		}
		if projection.Truncated || projection.Total > 1 {
			continue // ambiguous names remain unresolved
		}
		switch projection.Total {
		case 0:
			if len(projection.Nodes) != 0 {
				return fmt.Errorf("overlay target %q: invalid empty projection", name)
			}
		case 1:
			if len(projection.Nodes) != 1 || projection.Nodes[0] == nil {
				return fmt.Errorf("overlay target %q: invalid unique projection", name)
			}
			resolvedByName[name] = projection.Nodes[0].ID
		default:
			return fmt.Errorf("overlay target %q: invalid bounded projection", name)
		}
	}

	inspected = 0
	for _, edges := range edgesByFrom {
		for _, edge := range edges {
			inspected++
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if edge == nil {
				continue
			}
			if resolved := resolvedByName[overlayUnresolvedTargetName(edge.To)]; resolved != "" {
				edge.To = resolved
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	layer.RebuildInEdges()
	return ctx.Err()
}

func overlayUnresolvedTargetName(target string) string {
	if !strings.HasPrefix(target, unresolvedPrefix) {
		return ""
	}
	target = strings.TrimPrefix(target, unresolvedPrefix)
	if index := strings.Index(target, "::"); index > 0 {
		target = target[index+2:]
	}
	if index := strings.Index(target, "@"); index > 0 {
		target = target[:index]
	}
	return target
}

// hashOverlayFiles produces a stable content-hash of an overlay
// file set so the cache can detect "same set, reuse parse".
func hashOverlayFiles(files []daemon.OverlayFile) string {
	sorted := make([]daemon.OverlayFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%t\x00%s\x00", f.Path, f.Deleted, f.BaseSHA)
		_, _ = h.Write([]byte(f.Content))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// overlayContentFor returns the editor-buffer content for an
// absolute path in the calling session's overlay, if any. Used by
// source-reading handlers (get_symbol_source, get_editing_context,
// smart_context) to substitute the overlay's text for the on-disk
// file when the request is overlay-active. Returns (content, true)
// on a hit, ("", false) otherwise — including the deleted-overlay
// case, since a tombstone has no content to return and callers
// should treat the file as absent.
func (s *Server) overlayContentFor(ctx context.Context, absPath string) (string, bool) {
	if s == nil || ctx == nil {
		return "", false
	}
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || snapshot == nil || !snapshot.canonical {
		return "", false
	}
	cleanedAbs := filepath.Clean(absPath)
	for _, ov := range snapshot.files {
		if ov.Deleted {
			continue
		}
		ovAbs, _ := s.resolveOverlayAbsPathForRequest(ctx, ov.Path)
		if ovAbs == "" {
			continue
		}
		if filepath.Clean(ovAbs) == cleanedAbs {
			return ov.Content, true
		}
	}
	return "", false
}

// overlayShadowsGraphPath reports whether the active request replaces or
// tombstones the path represented by a base search hit. Search indexes are
// built from durable files, so their snippets must never authenticate stale
// content from a file whose editor overlay owns the request-time view.
func (s *Server) overlayShadowsGraphPath(ctx context.Context, graphPath, nodeID string) bool {
	view := OverlayViewFromContext(ctx)
	if view == nil || view.Layer() == nil {
		return false
	}
	layer := view.Layer()
	for _, candidate := range []string{graphPath, graph.IDFile(nodeID)} {
		candidate = filepath.ToSlash(filepath.Clean(strings.TrimSpace(candidate)))
		if candidate != "" && candidate != "." && layer.HasFile(candidate) {
			return true
		}
	}
	return false
}

func (s *Server) filterOverlayContentHits(ctx context.Context, hits []graph.ContentHit) []graph.ContentHit {
	if OverlayViewFromContext(ctx) == nil || len(hits) == 0 {
		return hits
	}
	filtered := hits[:0:0]
	for _, hit := range hits {
		if !s.overlayShadowsGraphPath(ctx, hit.FilePath, hit.NodeID) {
			filtered = append(filtered, hit)
		}
	}
	return filtered
}

// overlayCacheInvalidate drops the cached layer for a session. Called
// by overlay_push / overlay_delete / overlay_drop so the next tool
// call re-parses with the fresh buffer state.
func (s *Server) overlayCacheInvalidate(sessID string) {
	if s == nil || sessID == "" {
		return
	}
	s.overlayLayerCache.Delete(sessID)
}

// Compile-time sanity: a sync.Mutex usage placeholder so future
// linter-driven import pruning doesn't strip the package.
var _ sync.Mutex

package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"time"

	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathguard"
	"github.com/zzet/gortex/internal/pathkey"
)

const projectedReadBudget = 150 * time.Millisecond

// checkedProjectionReadObserver observes an already physically verified reuse
// before its live-owner check. It supplies no projection or publication authority.
// Only request-scoped lifecycle tests install this optional scheduling observer.
type checkedProjectionReadObserverKey struct{}
type checkedProjectionReadObserver func(context.Context, *projectionEntry)

var errProjectionStorm = errors.New("file projection deferred by queued source storm")

func (s *Server) projectionStormActive(abs string) bool {
	watcher, ok := s.currentWatcher().(interface{ StormActiveForFile(string) bool })
	return ok && watcher.StormActiveForFile(abs)
}

func auditedSourceProjectionMismatch(ctx context.Context, err error) bool {
	if ctx.Err() != nil || projectionBudgetUnavailable(ctx, err) || errors.Is(err, context.Canceled) {
		return false
	}
	allowance, audited := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	if !audited {
		return false
	}
	// A competing read can replace the same suspect ACK owner with a checked
	// same-byte COW entry before this request borrows it. All audited file
	// reads must restart the whole attempt instead of losing their file layer
	// and serving old base rows. No old facts or reader are rebound in place.
	if errors.Is(err, projectionSuperseded) {
		return true
	}
	return (allowance.tool == "" || allowance.tool == "get_symbol_source") && errors.Is(err, errProjectionExternalWrite)
}

// Each request retains only the immutable file layers it actually checked in
// its selected checkout. Editor buffers compose above these disk projections.
type requestProjectionKey struct{}

func requestProjectionLayers(ctx context.Context) []graph.OverlayLayerReader {
	layers, _ := ctx.Value(requestProjectionKey{}).([]graph.OverlayLayerReader)
	return layers
}

// ensureProjected reads current admitted bytes without publishing graph/store
// state. Failure is explicit: an old graph must never masquerade as a current
// answer after the checked file could not be projected.
func (s *Server) ensureProjected(ctx context.Context, paths []string) (context.Context, error) {
	if requestReadsCommittedTree(ctx) || !requestViewFromContext(ctx).acceptsBufferOverlay() {
		return ctx, nil
	}
	ctx = withRequestProjectionFacts(ctx)
	if err := validateRequestProjectionStage(ctx, projectionReadInherited); err != nil {
		return ctx, err
	}
	deadline := time.Now().Add(projectedReadBudget)
	if original, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok && original.Before(deadline) {
		deadline = original
	}
	buildCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	layers := append([]graph.OverlayLayerReader(nil), requestProjectionLayers(ctx)...)
	for _, graphPath := range paths {
		if err := buildCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx, ctx.Err()
			}
			markRequestProjectionDeferred(ctx, graphPath, "external_write")
			continue
		}
		if editor := OverlayViewFromContext(ctx); editor != nil && editor.LayerReader() != nil && editor.LayerReader().HasFile(graphPath) {
			// This pinned buffer completely owns the file. Disk contributes
			// neither nodes nor edges, so do not read or parse it underneath.
			continue
		}
		borrowed := false
		for _, layer := range layers {
			if layer.HasFile(graphPath) {
				borrowed = true
				break
			}
		}
		if borrowed {
			if !s.hasRequestProjectionFact(ctx, graphPath) {
				return ctx, annotateProjectionReadRefusal(projectionReadBorrowed, projectionOwnerFactUnavailable, projectionSuperseded)
			}
			// Candidate enumeration and hydration must keep one immutable
			// selection. Supersession was refused above; never append a newer
			// same-file layer beneath already selected older candidates.
			continue
		}
		idx, rel := s.indexerForRel(graphPath)
		if idx == nil {
			continue
		}
		abs, err := s.resolveGraphPath(ctx, graphPath)
		if err != nil {
			return ctx, err
		}
		entry, err := s.projectCurrentTrackedFile(buildCtx, ctx, abs, graphPath, rel)
		if err != nil {
			if errors.Is(err, errProjectionStorm) {
				markRequestProjectionStorm(ctx, graphPath)
				continue
			}
			if auditedSourceProjectionMismatch(ctx, err) {
				return ctx, errProjectedSourceMoved
			}
			if projectionBudgetUnavailable(ctx, err) || ctx.Err() == nil && (errors.Is(err, errProjectionExternalWrite) || errors.Is(err, projectionSuperseded)) {
				markRequestProjectionDeferred(ctx, graphPath, "external_write")
				continue
			}
			return ctx, fmt.Errorf("file graph pending for %s: %w", graphPath, err)
		}
		if entry != nil {
			if s.retireSelectedProjection(buildCtx, ctx, entry) {
				continue
			}
			if err := buildCtx.Err(); err != nil {
				if ctx.Err() != nil {
					return ctx, ctx.Err()
				}
				markRequestProjectionDeferred(ctx, graphPath, "external_write")
				continue
			}
			layer, err := projectedFileVisibility(buildCtx, s.requestSelectedBaseReader(ctx), entry)
			if err != nil {
				// Carry is optional relation evidence. Unknown checked capability
				// keeps the exact file projection, with its pending relation facts.
				// Required disk/parse failures above and actual cancellation still
				// refuse; no errorless or canonical carry fallback is introduced.
				layer = entry.layer
			}
			if err := buildCtx.Err(); err != nil {
				if ctx.Err() != nil {
					return ctx, ctx.Err()
				}
				markRequestProjectionDeferred(ctx, graphPath, "external_write")
				continue
			}
			ctx, err = retainRequestProjection(ctx, entry)
			if err != nil {
				if auditedSourceProjectionMismatch(ctx, err) {
					return ctx, errProjectedSourceMoved
				}
				// This entry was already selected for checked retirement/carry.
				// A new owner forbids serving its old facts or falling back.
				return ctx, err
			}
			layers = append(layers, layer)
		}
	}
	if err := validateRequestProjectionStage(ctx, projectionReadSelection); err != nil {
		return ctx, err
	}
	if len(layers) == 0 {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, requestProjectionKey{}, layers)
	if editor := OverlayViewFromContext(ctx); editor != nil {
		// Reuse the request's pinned buffer layer; do not parse it again or
		// change its session/snapshot authority.
		ctx = WithOverlayView(ctx, graph.NewOverlaidViewWithLayer(s.requestBaseReader(ctx), editor.LayerReader()))
	}
	return ctx, nil
}

// Only the private projection budget may degrade into a qualified graph
// answer. Caller cancellation, identity/parse failures and unsupported sources
// retain their refusals; no old graph gains exactness or completed-work credit.
func projectionBudgetUnavailable(caller context.Context, err error) bool {
	return caller.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, indexer.ErrProjectionDeadline) || errors.Is(err, indexer.ErrProjectionNoSlot))
}

// Both projection and source slicing use this same selected-root boundary.
func selectedProjectionReadPath(ctx context.Context, root, prefix, abs string) (string, string, string, error) {
	if selected := requestViewPathRoot(ctx); selected.serves(prefix) {
		root = selected.root
	}
	rel, ok := relativeWithinRoot(root, abs)
	if !ok {
		return "", "", "", errors.New("file is outside the selected checkout")
	}
	key := pathkey.MutationKey(abs)
	if !pathguard.WithinRoot(key, pathkey.IdentityKey(pathguard.ResolveRoot(root))) {
		return "", "", "", errors.New("file parent is outside the selected checkout")
	}
	return root, rel, key, nil
}

func (s *Server) projectCurrentTrackedFile(buildCtx, requestCtx context.Context, abs, graphPath, rel string) (*projectionEntry, error) {
	return s.projectCurrentTrackedFileVersion(buildCtx, requestCtx, abs, graphPath, rel, nil)
}

// sameVersion confines a late proof to the already selected immutable bytes.
// Ordinary selection retains the existing changed-source projection path.
func (s *Server) projectCurrentTrackedFileVersion(buildCtx, requestCtx context.Context, abs, graphPath, rel string, sameVersion *projectionEntry) (*projectionEntry, error) {
	idx, _ := s.indexerForRel(graphPath)
	tracked := idx.IsTracked(rel)
	root, readPath, key, err := selectedProjectionReadPath(requestCtx, idx.RootPath(), idx.RepoPrefix(), abs)
	if err != nil {
		return nil, err
	}
	observed := pendingFileProjections.captureReadExpectation(key)
	expected := observed.entry
	if sameVersion != nil && expected != sameVersion {
		return nil, projectionSuperseded
	}
	// A current cached payload is reusable during queued work. Without an
	// existing owner, a storm must not start disk projection work at all.
	if expected == nil && s.projectionStormActive(abs) {
		return nil, errProjectionStorm
	}
	before, err := statDiskStamp(abs)
	if err != nil {
		return nil, err
	}
	if sameVersion != nil && (before.info == nil || !sameVersion.disk.matches(before)) {
		return nil, projectionSuperseded
	}
	if before.info == nil {
		if expected != nil && expected.tombstone && liveProjectionForAcknowledgement(expected) {
			return expected, nil
		}
		if !tracked && expected == nil {
			return nil, nil
		}
		if s.projectionStormActive(abs) {
			return nil, errProjectionStorm
		}
		entry := newProjectionTombstone(abs, graphPath, idx.RepoPrefix(), 0)
		latest, err := statDiskStamp(abs)
		if err != nil || latest.info != nil || pathkey.MutationKey(abs) != key {
			return nil, errors.Join(err, errProjectionExternalWrite)
		}
		if err := pendingFileProjections.publishReadExpected(entry, observed); err != nil {
			return nil, err
		}
		s.admitReadBuiltProjection(requestCtx, root, idx, tracked, entry)
		return entry, nil
	}
	// Reuse the confined, bounded, nonblocking regular-file reader. A FIFO,
	// symlink or unsupported platform is a refusal, never an unsafe Open.
	files, err := source.NewFilesystemSource(root)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	raw, _, err := files.ReadRegularFile(buildCtx, filepath.ToSlash(readPath), 1<<20)
	if err != nil {
		return nil, err
	}
	// The existing buffer admission applies the owning indexer's exclusions,
	// selected checkout's ignore files, language and content gates to these
	// already-confined bytes. A new file need not have a watcher inventory row.
	if !tracked && !idx.AdmitSourceSearchBuffer(buildCtx, root, abs, raw, nil) {
		return nil, errors.New("file is not admitted source")
	}
	after, err := statDiskStamp(abs)
	if err != nil || !before.matches(after) {
		return nil, errors.Join(err, errProjectionExternalWrite)
	}
	if err := buildCtx.Err(); err != nil {
		return nil, err
	}
	// This path already read the bounded, confined current bytes. Never call
	// legacy lookup/onDisk here: its suspect rehash performs another raw read.
	var rawHash [32]byte
	if expected != nil {
		rawHash = blake3.Sum256(raw)
		if !expected.tombstone && expected.rawHash == rawHash && expected.disk.matches(after) &&
			pathkey.MutationKey(abs) == key {
			if err := buildCtx.Err(); err != nil {
				return nil, err
			}
			if observe, ok := buildCtx.Value(checkedProjectionReadObserverKey{}).(checkedProjectionReadObserver); ok {
				observe(buildCtx, expected)
				if err := buildCtx.Err(); err != nil {
					return nil, err
				}
			}
			// Ownership can move while bounded current bytes are read even
			// though their content did not change. Refuse this selection without
			// inventing source invalidation on a checked historical owner.
			if pendingFileProjections.snap.Load().entries[key] != expected {
				// No file layer or facts have been retained at this boundary.
				// A competing checked read may already own these exact immutable
				// bytes. Use search binding's same-version authority proof instead
				// of spending a whole-response attempt on pointer contention.
				// Physical executions, events and expired contexts still refuse.
				if current := checkedPendingSearchCOWSuccessor(buildCtx, expected); current != nil {
					return current, nil
				}
				return nil, projectionSuperseded
			}
			if err := buildCtx.Err(); err != nil {
				return nil, err
			}
			if !expected.suspect.Load() {
				return expected, nil
			}
			// Reuse the immutable parse only after bounded current-byte checks.
			// This unpublished copy has no physical sequence or old invalidation;
			// the old pointer stays suspect and the revision is checked at publish.
			entry := expected.committedCopy(0, after)
			entry.suspect.Store(false)
			if err := buildCtx.Err(); err != nil {
				return nil, err
			}
			if err := pendingFileProjections.publishReadExpected(entry, observed); err != nil {
				return nil, err
			}
			s.admitReadBuiltProjection(requestCtx, root, idx, tracked, entry)
			if err := buildCtx.Err(); err != nil {
				return nil, err
			}
			return entry, nil
		}
	}
	if sameVersion != nil {
		return nil, projectionSuperseded // no new bytes, SQL admission or parse may prove this old selection
	}
	// Exact immutable reuse above needs no canonical SQL admission. Existing
	// hash-based publication callbacks still retire its current COW owner.
	// Changed or uncached bytes retain the bounded canonical-match path.
	if !requestViewFromContext(requestCtx).routed() {
		matches, err := idx.IndexedContentMatchesContext(buildCtx, rel, raw)
		if err != nil {
			return nil, err
		}
		if matches {
			return nil, nil
		}
	} else if expected == nil {
		if allowance, ok := requestCtx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance); ok && allowance.tool == "get_symbol_source" {
			// Reuse the existing SAME-reader hash/full-node proof, including
			// gen0/layer revisions and pending-owner checks. Unknown proof
			// keeps the ordinary bounded physical projection path below.
			if _, matched := s.readFileCompressionSymbols(buildCtx, abs, graphPath, raw); matched {
				return nil, nil
			}
		}
	}
	if err := buildCtx.Err(); err != nil {
		return nil, err
	}
	if expected != nil {
		// Invalidate the old bytes on every exit, including cache refusal.
		// Do it after admission so this read's own old-version invalidation
		// cannot be inherited as an event against the newly verified bytes.
		// Actual concurrent watcher events advance its captured revision;
		// read publication refuses them rather than clearing their evidence.
		defer expected.suspect.Store(true)
	} else {
		rawHash = blake3.Sum256(raw)
	}
	// No fresh extraction during a queued storm. The matching-cache branch
	// above reused only bounded, identity-checked bytes and immutable nodes.
	if s.projectionStormActive(abs) {
		return nil, errProjectionStorm
	}
	// Use the registered parser identity while retaining the physical
	// checkout path as the registry key and disk identity.
	projection, err := idx.ProjectSource(buildCtx, s.overlayOwnerAbsPath(requestCtx, abs), raw)
	if err != nil {
		return nil, err
	}
	if !tracked && !projection.ParseChecked {
		return nil, errors.New("untracked source parse health is unavailable")
	}
	entry := newProjectionEntry(abs, idx.RepoPrefix(), projection, raw, 0, after)
	latest, err := statDiskStamp(abs)
	if err != nil || !after.matches(latest) {
		return nil, errors.Join(err, errProjectionExternalWrite)
	}
	// Stat identity alone misses equal-size in-place edits with restored mtime,
	// including a newer version that retired while this extraction was running.
	current, _, readErr := files.ReadRegularFile(buildCtx, filepath.ToSlash(readPath), 1<<20)
	finalStamp, statErr := statDiskStamp(abs)
	if readErr != nil || statErr != nil || !latest.matches(finalStamp) || blake3.Sum256(current) != rawHash || pathkey.MutationKey(abs) != key {
		return nil, errors.Join(readErr, statErr, errProjectionExternalWrite)
	}
	if err := pendingFileProjections.publishReadExpected(entry, observed); err != nil {
		return nil, err
	}
	s.admitReadBuiltProjection(requestCtx, root, idx, tracked, entry)
	return entry, nil
}

// Projection facts are request-owned observations consumed by the existing
// tier producer. They are shared with the outer response decorator, while the
// graph layers retained by each derived context remain immutable snapshots.
type requestProjectionFactsKey struct{}
type requestProjectionFacts struct {
	mu             sync.RWMutex
	entries        map[string]*projectionEntry
	deferredFiles  map[string]string   // projection deferral observations, not publication authority
	storeBusyFiles map[string]struct{} // selected store sections were unavailable
}

func withRequestProjectionFacts(ctx context.Context) context.Context {
	if _, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts); ok {
		return ctx
	}
	return context.WithValue(ctx, requestProjectionFactsKey{}, &requestProjectionFacts{entries: make(map[string]*projectionEntry)})
}

// A queued storm defers disk projection for this request. Keep its observed
// pending file in the same request-local facts consumed by the tier producer;
// detached background work has no storm witness and retains generic labels.
func markRequestProjectionStorm(ctx context.Context, path string) {
	markRequestProjectionDeferred(ctx, path, "storm")
}

func markRequestProjectionDeferred(ctx context.Context, path, reason string) {
	facts := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	facts.mu.Lock()
	defer facts.mu.Unlock()
	if facts.deferredFiles == nil {
		facts.deferredFiles = make(map[string]string)
	}
	facts.deferredFiles[path] = reason
}

// markRequestStoreBusy records a failed selected-store section in the same
// request facts consumed by freshnessFor. It does not authorize fallback to a
// canonical reader or certify any omitted rows. Callers pass a graph-keyed path.
func markRequestStoreBusy(ctx context.Context, path string) context.Context {
	ctx = withRequestProjectionFacts(ctx)
	facts := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	facts.mu.Lock()
	defer facts.mu.Unlock()
	if facts.storeBusyFiles == nil {
		facts.storeBusyFiles = make(map[string]struct{})
	}
	facts.storeBusyFiles[path] = struct{}{}
	return ctx
}

func requestDeferredProjectionFiles(ctx context.Context) map[string]string {
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return nil
	}
	facts.mu.RLock()
	defer facts.mu.RUnlock()
	return maps.Clone(facts.deferredFiles)
}

func requestStoreBusyFiles(ctx context.Context) []string {
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return nil
	}
	facts.mu.RLock()
	defer facts.mu.RUnlock()
	var paths []string
	for path := range facts.storeBusyFiles {
		paths = append(paths, path)
	}
	return paths
}

// retainRequestProjection shares the existing request-owned freshness facts
// with candidate and row selection. It adds no registry or publication owner.
func retainRequestProjection(ctx context.Context, entry *projectionEntry) (context.Context, error) {
	ctx = withRequestProjectionFacts(ctx)
	if guard := projectionLivenessFailure(entry); guard != projectionOwnerLive {
		return ctx, annotateProjectionReadRefusal(projectionReadRetain, guard, projectionSuperseded)
	}
	facts := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	facts.mu.Lock()
	defer facts.mu.Unlock()
	if prior := facts.entries[entry.key]; prior != nil && prior != entry {
		return ctx, annotateProjectionReadRefusal(projectionReadRetain, projectionOwnerFactsConflict, projectionSuperseded)
	}
	facts.entries[entry.key] = entry
	return ctx, nil
}

// Re-entry must reject the entire old selection, including unchanged bytes
// owned by a newer physical execution. Only borrowed entries are examined;
// there is no global rebuild, candidate scan, raw read or parser invocation.
func validateRequestProjections(ctx context.Context) error {
	return validateRequestProjectionStage(ctx, projectionReadValidation)
}

func validateRequestProjectionStage(ctx context.Context, stage projectionReadStage) error {
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return nil
	}
	facts.mu.RLock()
	defer facts.mu.RUnlock()
	if len(facts.entries) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, entry := range facts.entries {
		if guard := projectionLivenessFailure(entry); guard != projectionOwnerLive {
			return annotateProjectionReadRefusal(stage, guard, projectionSuperseded)
		}
	}
	return nil
}

func withProjectedReadToolFacts(ctx context.Context, name string) context.Context {
	switch name {
	case "get_symbol", "get_symbol_source", "get_file_summary", "get_editing_context", "get_cfg", "search_symbols", "read_file", "get_callers", "get_call_chain":
		return withRequestProjectionFacts(ctx)
	default:
		return ctx
	}
}

func (s *Server) hasRequestProjectionFact(ctx context.Context, path string) bool {
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return false
	}
	facts.mu.RLock()
	empty := len(facts.entries) == 0
	facts.mu.RUnlock()
	if empty {
		return false
	} // idle producer does no path resolution or stat
	abs, err := s.resolveGraphPath(ctx, path)
	if err != nil {
		return false
	}
	key := pathkey.MutationKey(abs)
	facts.mu.RLock()
	entry := facts.entries[key]
	facts.mu.RUnlock()
	return liveProjectionForAcknowledgement(entry)
}

// Carry is a request-local visibility fact, never a mutation of a published
// projection or credit for background relations. This base excludes both the
// request's disk projections and editor buffers.
func (s *Server) requestSelectedBaseReader(ctx context.Context) graph.Reader {
	if view := requestViewFromContext(ctx); view.routed() {
		return view.reader
	}
	return s.graph
}

// The existing matching algorithm reads only this bounded, checked snapshot of
// the same selected base. No contextless store lookup escapes the read budget.
type projectedCarryBase struct {
	graph.Reader
	nodes    []*graph.Node
	outgoing map[string][]*graph.Edge
	targets  map[string]*graph.Node
}

func (p *projectedCarryBase) GetFileNodes(string) []*graph.Node { return p.nodes }
func (p *projectedCarryBase) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	return p.outgoing
}
func (p *projectedCarryBase) GetNodesByIDs([]string) map[string]*graph.Node { return p.targets }

func projectedFileVisibility(ctx context.Context, base graph.Reader, entry *projectionEntry) (graph.OverlayLayerReader, error) {
	var unresolved []*graph.Edge
	for edge := range entry.layer.Edges() {
		if edge != nil && edge.Kind == graph.EdgeCalls && graph.IsUnresolvedTarget(edge.To) && !graph.IsFnValuePlaceholder(edge.To) && edge.FilePath == entry.graphPath && graph.IDFile(edge.From) == entry.graphPath {
			unresolved = append(unresolved, edge)
		}
	}
	if entry.tombstone || len(unresolved) == 0 {
		return entry.layer, nil
	}
	files, err := graph.ContractFileNodesContext(ctx, base, entry.repoPrefix, []string{entry.graphPath})
	if err != nil {
		return nil, err
	}
	nodes := files[entry.graphPath]
	if len(nodes) == 0 {
		return entry.layer, ctx.Err()
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	outgoing, truncated, err := graph.GetOutEdgesByNodeIDsWithMetadataContext(ctx, base, ids, graph.ContractProjectionRowLimit)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, graph.ErrContractProjectionLimit
	}
	var targetIDs []string
	for _, edges := range outgoing {
		for _, edge := range edges {
			if edge != nil && edge.Kind == graph.EdgeCalls && edge.FilePath == entry.graphPath && !graph.IsUnresolvedTarget(edge.To) {
				targetIDs = append(targetIDs, edge.To)
			}
		}
	}
	if len(targetIDs) == 0 {
		return entry.layer, ctx.Err()
	}
	targets, err := graph.ContractSourceNodesContext(ctx, base, targetIDs)
	if err != nil {
		return nil, err
	}
	carried := graph.CarryBaseEdges(&projectedCarryBase{Reader: base, nodes: nodes, outgoing: outgoing, targets: targets}, entry.graphPath, unresolved)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(carried) == 0 {
		return entry.layer, nil
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile(entry.graphPath, false)
	for _, node := range entry.layer.FileNodes(entry.graphPath) {
		layer.AddNode(entry.graphPath, node)
	}
	for edge := range entry.layer.Edges() {
		layer.AddEdge(edge)
	}
	for _, edge := range carried {
		layer.AddEdge(edge)
	}
	return layer, nil
}

package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// A sequence holds values only: no graph readers, leases, SQL connections or
// writer tokens survive a call. Bounds are per session and expired cursors
// refuse explicitly instead of restarting at row zero or hiding unread rows.
const (
	symbolPageCacheEntries  = defaultResponseBufferCap
	symbolPageSequenceBytes = defaultResponseBufferBytes / symbolPageCacheEntries
	symbolPageTTL           = comboWindow
)

type symbolPageCursor struct {
	Version  int    `json:"v"`
	Sequence string `json:"seq"`
	Page     int    `json:"page"`
}

func symbolCursor(id string, page int) string {
	raw, _ := json.Marshal(symbolPageCursor{2, id, page})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func parseSymbolCursor(raw string) (symbolPageCursor, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return symbolPageCursor{}, false
	}
	var cursor symbolPageCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.Version != 2 {
		return symbolPageCursor{}, false
	}
	return cursor, true
}

type symbolPageCandidate struct {
	id              string
	node, row, rank json.RawMessage
}
type symbolPageReplay struct {
	result *mcplib.CallToolResult
	nodes  []*graph.Node
}
type symbolPageSequence struct {
	token                      chan struct{}
	id, identity, firstSeed    string
	created                    time.Time
	query                      string
	owner                      *sessionState
	scope                      ResolvedScope
	template                   map[string]any
	storage                    *symbolPageStorage
	retired                    atomic.Bool
	nextPage, horizon, pending int
	more                       bool
}
type symbolPageCache struct {
	closed  bool
	calls   map[*symbolPageCall]struct{}
	callWG  sync.WaitGroup
	mu      sync.Mutex
	entries map[string]*symbolPageSequence
	order   []string
}
type symbolPageCacheKey struct{}
type symbolPageOwnerKey struct{}
type symbolPageCallKey struct{}
type symbolPageCall struct {
	cache  *symbolPageCache
	cancel context.CancelFunc
	done   <-chan struct{}
}
type symbolPageRefillKey struct{}
type symbolPageRefill struct {
	sequence *symbolPageSequence
	horizon  int
}
type symbolPageIdentityKey struct{}

func (s *Server) symbolPages(ctx context.Context) *symbolPageCache {
	if cache, ok := ctx.Value(symbolPageCacheKey{}).(*symbolPageCache); ok {
		return cache
	}
	session, _ := ctx.Value(symbolPageOwnerKey{}).(*sessionState)
	if session == nil {
		session = s.sessionFor(ctx)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.symbolPages == nil {
		session.symbolPages = &symbolPageCache{entries: make(map[string]*symbolPageSequence)}
	}
	return session.symbolPages
}

// Registration and terminal closure share the cache lock, so no Add can
// race a terminal Wait. Internal refills reuse the outer registration.
func (cache *symbolPageCache) beginCall(ctx context.Context) (context.Context, *symbolPageCall, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return ctx, nil, false
	}
	owned, cancel := context.WithCancel(ctx)
	call := &symbolPageCall{cache: cache, cancel: cancel, done: owned.Done()}
	if cache.calls == nil {
		cache.calls = make(map[*symbolPageCall]struct{})
	}
	cache.calls[call] = struct{}{}
	cache.callWG.Add(1)
	return context.WithValue(owned, symbolPageCallKey{}, call), call, true
}
func (call *symbolPageCall) finish() {
	call.cancel()
	call.cache.mu.Lock()
	delete(call.cache.calls, call)
	call.cache.mu.Unlock()
	call.cache.callWG.Done()
}
func (cache *symbolPageCache) lookup(id string) *symbolPageSequence {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return nil
	}
	entry := cache.entries[id]
	if entry != nil && time.Since(entry.created) >= symbolPageTTL {
		delete(cache.entries, id)
		cache.order = slices.DeleteFunc(cache.order, func(key string) bool { return key == id })
		entry.retire()
		return nil
	}
	return entry
}
func (cache *symbolPageCache) add(entry *symbolPageSequence) bool {
	_, added := cache.publishFirst(entry)
	return added
}

// First-page publication chooses one live sequence for an identical full
// retrieval seed. Random IDs remain lifetime-scoped: retirement or expiry
// never recreates the ownership of an earlier cursor.
func (cache *symbolPageCache) publishFirst(entry *symbolPageSequence) (*symbolPageSequence, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		entry.retire()
		return nil, false
	}
	if entry.firstSeed != "" {
		for _, id := range append([]string(nil), cache.order...) {
			existing := cache.entries[id]
			if existing == nil || existing.firstSeed != entry.firstSeed {
				continue
			}
			if existing.retired.Load() || time.Since(existing.created) >= symbolPageTTL {
				delete(cache.entries, id)
				cache.order = slices.DeleteFunc(cache.order, func(key string) bool { return key == id })
				existing.retire()
				continue
			}
			return existing, true
		}
	}
	for len(cache.order) >= symbolPageCacheEntries {
		cache.entries[cache.order[0]].retire()
		delete(cache.entries, cache.order[0])
		cache.order = cache.order[1:]
	}
	cache.entries[entry.id] = entry
	cache.order = append(cache.order, entry.id)
	return entry, true
}

func symbolFirstPageSeed(identity string, template []byte, candidates []symbolPageCandidate, horizon int, more bool) string {
	hash := sha256.New()
	write := func(raw []byte) {
		_, _ = hash.Write(raw)
		_, _ = hash.Write([]byte{0})
	}
	write([]byte(identity))
	write(template)
	write([]byte(fmt.Sprintf("%d:%t", horizon, more)))
	for _, candidate := range candidates {
		write([]byte(candidate.id))
		write(candidate.node)
		write(candidate.row)
		write(candidate.rank)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func symbolPageError(reason string) *mcplib.CallToolResult {
	return mcplib.NewToolResultError("search cursor " + reason + "; restart the query without cursor")
}

// Include the resolved scope, not only its spelling, and the routed view and
// editor snapshot identities. Coarse node revision changes refuse continuation
// rather than silently merging a different mutable view into this sequence.
func (s *Server) symbolPageIdentity(ctx context.Context, req mcplib.CallToolRequest, scope ResolvedScope) (string, error) {
	args := make(map[string]any, len(req.GetArguments()))
	for key, value := range req.GetArguments() {
		if key != "cursor" {
			args[key] = value
		}
	}
	identity := map[string]any{"args": args, "scope": scope, "cwd": SessionCWDFromContext(ctx), "view": requestOverlayViewCacheKey(ctx), "gcx": s.isGCX(ctx, req), "toon": s.isTOON(ctx, req), "compact": isCompact(req)}
	if snapshot, ok := overlayRequestSnapshotFromContext(ctx); ok {
		identity["overlay"] = hashOverlayFiles(snapshot.files)
	} else if OverlayViewFromContext(ctx) != nil {
		identity["overlay_reader"] = fmt.Sprintf("%p", OverlayViewFromContext(ctx))
	}
	known := false
	if revision, ok := s.readerFor(ctx).(interface{ SearchMutationRevision() (uint64, bool) }); ok {
		v, hasRevision := revision.SearchMutationRevision()
		if hasRevision {
			identity["revision"] = v
			known = true
		}
	} else if revision, ok := s.readerFor(ctx).(interface{ MutationRevision() uint64 }); ok {
		identity["revision"] = revision.MutationRevision()
		known = true
	}
	if !known {
		identity["mutable_revision_unknown"] = true
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func symbolPageRevisionKnown(reader graph.Reader) bool {
	if revision, ok := reader.(interface{ SearchMutationRevision() (uint64, bool) }); ok {
		_, known := revision.SearchMutationRevision()
		return known
	}
	_, known := reader.(interface{ MutationRevision() uint64 })
	return known
}

func cloneSymbolResult(result *mcplib.CallToolResult) *mcplib.CallToolResult {
	if result == nil {
		return nil
	}
	copy := *result
	copy.Content = append([]mcplib.Content(nil), result.Content...)
	if result.Meta != nil {
		raw, _ := json.Marshal(result.Meta)
		copy.Meta = nil
		_ = json.Unmarshal(raw, &copy.Meta)
	}
	// JSON is the only cursor format. Copy its structured map independently;
	// TextContent holds immutable strings, and metadata is immutable after fit.
	if result.StructuredContent != nil {
		raw, _ := json.Marshal(result.StructuredContent)
		var value any
		_ = json.Unmarshal(raw, &value)
		copy.StructuredContent = value
	}
	return &copy
}

func (s *Server) continueSymbolPage(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error, bool) {
	if ctx.Value(symbolPageRefillKey{}) != nil {
		return nil, nil, false
	}
	cursor, newCursor := parseSymbolCursor(req.GetString("cursor", ""))
	if !newCursor {
		return nil, nil, false
	}
	if cursor.Sequence == "" || cursor.Page < 1 {
		return symbolPageError("is malformed"), nil, true
	}
	if source := sourceRequestView(ctx); source != nil && source.sourceScope == "declarations" {
		return symbolPageError("belongs to a different view"), nil, true
	}
	cache := s.symbolPages(ctx)
	owned, call, live := cache.beginCall(ctx)
	if !live {
		return symbolPageError("session ended"), nil, true
	}
	defer call.finish()
	ctx = context.WithValue(owned, symbolPageCacheKey{}, cache)
	entry := cache.lookup(cursor.Sequence)
	if entry == nil {
		return symbolPageError("expired or was evicted"), nil, true
	}
	admitted, err := entry.acquire(ctx)
	if err != nil {
		return nil, err, true
	}
	if !admitted {
		return symbolPageError("expired or was evicted"), nil, true
	}

	defer func() {
		if entry.retired.Load() {
			entry.storage.close()
		}
		entry.token <- struct{}{}
	}()
	q := req.GetString("query", "")
	entry.owner.recordSearch(q)
	ctx = context.WithValue(ctx, symbolPageOwnerKey{}, entry.owner)
	ctx = context.WithValue(ctx, symbolPageCacheKey{}, s.symbolPages(ctx))
	scope, refusal := s.resolveScope(ctx, requestWithInlineScopeClauses(req, parseFieldQuery(q)), IntentLocate)
	if refusal != nil {
		return refusal, nil, true
	}
	identity, err := s.symbolPageIdentity(ctx, req, scope)
	if err != nil {
		return nil, err, true
	}
	if identity != entry.identity {
		return symbolPageError("query, scope, or view changed"), nil, true
	}
	if !symbolPageRevisionKnown(s.readerFor(ctx)) {
		return symbolPageError("view has no authoritative mutation revision"), nil, true
	}
	replay, ok, replayErr := entry.storage.replay(ctx, cursor.Page)
	if replayErr != nil {
		return nil, replayErr, true
	}
	if ok {
		if err := ctx.Err(); err != nil {
			return nil, err, true
		}
		recordLastSearchFromNodes(entry.owner, entry.query, replay.nodes)
		captureLocalizationSearchSymbols(ctx, replay.nodes)
		return cloneSymbolResult(replay.result), nil, true
	}
	if cursor.Page != entry.nextPage {
		return symbolPageError("page is unavailable"), nil, true
	}
	if entry.pending == 0 && entry.more {
		if entry.horizon > int(^uint(0)>>1)/2 {
			return symbolPageError("retrieval horizon overflowed"), nil, true
		}
		horizon := max(entry.horizon+1, entry.horizon*2)
		arguments := make(map[string]any, len(req.GetArguments()))
		for key, value := range req.GetArguments() {
			arguments[key] = value
		}
		delete(arguments, "cursor")
		refillReq := req
		refillReq.Params.Arguments = arguments
		result, err := s.handleSearchSymbols(context.WithValue(ctx, symbolPageRefillKey{}, &symbolPageRefill{entry, horizon}), refillReq)
		return result, err, true
	}
	stage, err := entry.storage.begin(ctx, nil)
	if err != nil {
		return nil, err, true
	}
	defer stage.rollback()
	result, err := s.renderSymbolSequence(ctx, req, entry, stage, entry.template, entry.more)
	return result, err, true
}

func (s *Server) publishSymbolPage(ctx context.Context, req mcplib.CallToolRequest, template map[string]any, nodes []*graph.Node, ranks []*rerank.Candidate, horizon int, more bool, scope ResolvedScope, queryText string) (*mcplib.CallToolResult, error) {
	identity, err := s.symbolPageIdentity(ctx, req, scope)
	if err != nil {
		return nil, err
	}

	refill, _ := ctx.Value(symbolPageRefillKey{}).(*symbolPageRefill)
	var entry *symbolPageSequence
	if refill != nil {
		entry = refill.sequence
		if entry.identity != identity {
			return symbolPageError("view changed during search"), nil
		}
	} else {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		owner, _ := ctx.Value(symbolPageOwnerKey{}).(*sessionState)
		if owner == nil {
			owner = s.sessionFor(ctx)
		}
		if before, _ := ctx.Value(symbolPageIdentityKey{}).(string); before != "" {
			identity = before
		}
		entry = &symbolPageSequence{owner: owner, token: make(chan struct{}, 1), id: hex.EncodeToString(random[:]), identity: identity, created: time.Now(), query: queryText, scope: scope, storage: newSymbolPageStorage()}
	}
	published := false
	defer func() {
		if refill == nil {
			if !published || entry.retired.Load() {
				entry.storage.close()
			}
			entry.token <- struct{}{}
		}
	}()
	var encodedRanks []map[string]any
	if req.GetBool("debug", false) {
		encodedRanks = encodeRerankBreakdown(ranks, s.engineFor(ctx).Rerank())
	}
	candidates := make([]symbolPageCandidate, 0, len(nodes))
	for index, node := range s.withAbsPaths(ctx, nodes) {
		if node == nil || node.ID == "" {
			continue
		}
		// Retain exactly the attribution and typed localization evidence, not code/docs.
		projection := &graph.Node{ID: node.ID, FilePath: node.FilePath, AbsoluteFilePath: node.AbsoluteFilePath, RepoPrefix: node.RepoPrefix, Name: node.Name, QualName: node.QualName, Kind: node.Kind, StartLine: node.StartLine}
		if signature, ok := node.Meta["signature"].(string); ok {
			projection.Meta = map[string]any{"signature": signature}
		}
		rawNode, err := json.Marshal(projection)
		if err != nil {
			return nil, err
		}
		row, err := json.Marshal(node.Brief())
		if err != nil {
			return nil, err
		}
		var rank json.RawMessage
		if req.GetBool("debug", false) && index < len(encodedRanks) {
			rank, err = json.Marshal(encodedRanks[index])
			if err != nil {
				return nil, err
			}
		}
		candidates = append(candidates, symbolPageCandidate{node.ID, rawNode, row, rank})
	}
	copiedTemplate := make(map[string]any, len(template))
	for key, value := range template {
		if key != "results" && key != "rerank" && key != "next_cursor" {
			copiedTemplate[key] = value
		}
	}
	rawTemplate, err := json.Marshal(copiedTemplate)
	if err != nil {
		return nil, err
	}
	copiedTemplate = map[string]any{}
	if err = json.Unmarshal(rawTemplate, &copiedTemplate); err != nil {
		return nil, err
	}
	if refill == nil {
		entry.firstSeed = symbolFirstPageSeed(entry.identity, rawTemplate, candidates, horizon, more)
	}
	stage, err := entry.storage.begin(ctx, candidates)
	if err != nil {
		return nil, err
	}
	defer stage.rollback()
	result, err := s.renderSymbolSequence(ctx, req, entry, stage, copiedTemplate, more)
	if err != nil || result == nil || result.IsError {
		return result, err
	}
	entry.horizon = horizon
	entry.more = more
	entry.template = copiedTemplate
	if refill == nil && entry.pending == 0 && !entry.more {
		// A terminal first page has no continuation ownership to retain.
		return result, nil
	}
	if refill == nil {
		winner, live := s.symbolPages(ctx).publishFirst(entry)
		if !live {
			return symbolPageError("session ended"), nil
		}
		if winner != entry {
			admitted, err := winner.acquire(ctx)
			if err != nil {
				return nil, err
			}
			if !admitted {
				return symbolPageError("expired or was evicted"), nil
			}
			defer func() {
				if winner.retired.Load() {
					winner.storage.close()
				}
				winner.token <- struct{}{}
			}()
			replay, ok, err := winner.storage.replay(ctx, 0)
			if err != nil {
				return nil, err
			}
			if !ok {
				return symbolPageError("first page is unavailable"), nil
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return cloneSymbolResult(replay.result), nil
		}
	}
	published = true
	return result, nil
}

func (s *Server) renderSymbolSequence(ctx context.Context, req mcplib.CallToolRequest, entry *symbolPageSequence, stage *symbolPageTransaction, template map[string]any, more bool) (*mcplib.CallToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := req.GetInt("limit", 20)
	if limit <= 0 {
		return symbolPageError("limit must be positive"), nil
	}
	selected, err := stage.page(ctx, limit)
	if err != nil {
		return nil, err
	}
	count := len(selected)
	rows := make([]any, 0, count)
	rankRows := make([]any, 0, count)
	for _, candidate := range selected {
		var row any
		if err := json.Unmarshal(candidate.row, &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
		if len(candidate.rank) > 0 {
			var rank any
			_ = json.Unmarshal(candidate.rank, &rank)
			rankRows = append(rankRows, rank)
		}
	}
	payload := make(map[string]any, len(template)+3)
	for key, value := range template {
		payload[key] = value
	}
	payload["results"] = rows
	payload["total"] = stage.total
	payload["truncated"] = stage.pending > count || more
	// Reserve a continuation during shaping because a nominal final page can
	// still lose rows to the existing budget policy. No identity marker is added.
	payload["next_cursor"] = symbolCursor(entry.id, entry.nextPage+1)
	if req.GetBool("debug", false) && len(rankRows) > 0 {
		payload["rerank"] = rankRows
	}
	observer := &symbolBudgetObservation{positions: make([]int, count)}
	for index := range observer.positions {
		observer.positions[index] = index
	}
	result, err := s.respondScopedJSONOrTOON(context.WithValue(ctx, symbolBudgetObservationKey{}, observer), req, payload, entry.scope)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(observer.positions) == 0 && count > 0 {
		return symbolPageError("budget cannot fit a row; increase max_bytes or max_tokens"), nil
	}
	returned := make([]symbolPageCandidate, 0, len(observer.positions))
	published := make([]*graph.Node, 0, len(observer.positions))
	for _, position := range observer.positions {
		returned = append(returned, selected[position])
		var node graph.Node
		if err := json.Unmarshal(selected[position].node, &node); err != nil {
			return nil, err
		}
		published = append(published, &node)
	}
	remaining := stage.pending - len(returned)
	if entry.nextPage > 0 && (remaining > 0 || more) && !symbolPageRevisionKnown(s.readerFor(ctx)) {
		return symbolPageError("view has no authoritative mutation revision"), nil
	}
	if remaining == 0 && !more {
		// Removal reduces size only; it cannot cause another budget drop.
		object, _ := result.StructuredContent.(map[string]any)
		if object != nil {
			delete(object, "next_cursor")
			object["truncated"] = false
			updated, encodeErr := mcplib.NewToolResultJSON(object)
			if encodeErr != nil {
				return nil, encodeErr
			}
			updated.Meta = result.Meta
			result = updated
		}
	}
	identity, err := s.symbolPageIdentity(ctx, req, entry.scope)
	if err != nil {
		return nil, err
	}
	if entry.nextPage > 0 && identity != entry.identity {
		return symbolPageError("view changed during page preparation"), nil
	}
	// Cache publication is the handler's commit boundary, not client delivery:
	// a lost transport reply can replay this exact committed page via its cursor.
	replay := symbolPageReplay{cloneSymbolResult(result), published}
	if entry.retired.Load() {
		return symbolPageError("expired or was evicted"), nil
	}
	if err = stage.commit(ctx, entry.nextPage, replay, returned); err != nil {
		return nil, err
	}
	if entry.retired.Load() {
		return symbolPageError("expired or was evicted"), nil
	}
	entry.pending = remaining
	entry.nextPage++
	session := entry.owner
	if previousQuery, skipped := session.drainSkippedNegatives(); previousQuery != "" && len(skipped) > 0 {
		s.combo.RecordNegative(previousQuery, skipped)
	}
	recordLastSearchFromNodes(session, entry.query, published)
	captureLocalizationSearchSymbols(ctx, published)
	return result, nil
}

// acquire validates retirement after token admission, not just after lookup.
func (entry *symbolPageSequence) acquire(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-entry.token:
	}
	if entry.retired.Load() {
		entry.storage.close()
		entry.token <- struct{}{}
		return false, nil
	}
	return true, nil
}

func (entry *symbolPageSequence) retire() {
	entry.retired.Store(true)
	select {
	case <-entry.token:
		entry.storage.close()
		entry.token <- struct{}{}
	default:
	}
}

// beginClose publishes retirement before returning its cancellation/cleanup
// action. A session owner can detach under its map lock, then invoke the
// action outside every map/session/cache lock without losing terminal ownership.
func (cache *symbolPageCache) beginClose() func() {
	cache.mu.Lock()
	if cache.closed {
		cache.mu.Unlock()
		return func() {}
	}
	cache.closed = true
	entries := cache.entries
	cache.entries = map[string]*symbolPageSequence{}
	cache.order = nil
	calls := make([]*symbolPageCall, 0, len(cache.calls))
	for call := range cache.calls {
		calls = append(calls, call)
	}
	cache.mu.Unlock()
	return func() {
		for _, call := range calls {
			call.cancel()
		}
		for _, entry := range entries {
			entry.retire()
		}
	}
}
func (cache *symbolPageCache) close() { cache.beginClose()() }
func (cache *symbolPageCache) hasCalls() bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.calls) > 0
}

type symbolBudgetObservationKey struct{}
type symbolBudgetObservation struct{ positions []int }

func symbolBudgetRetainer(ctx context.Context, req mcplib.CallToolRequest) func(string, []int) {
	if req.Params.Name != "search_symbols" {
		return nil
	}
	observer, _ := ctx.Value(symbolBudgetObservationKey{}).(*symbolBudgetObservation)
	if observer == nil {
		return nil
	}
	return func(key string, positions []int) {
		if key != "results" {
			return
		}
		kept := make([]int, 0, len(positions))
		for _, position := range positions {
			if position >= 0 && position < len(observer.positions) {
				kept = append(kept, observer.positions[position])
			}
		}
		observer.positions = kept
	}
}
func applySymbolObservedBudget(payload any, maxBytes int, retain func(string, []int)) (any, bool) {
	result, trimmed := applyBudget(payload, maxBytes)
	if retain != nil && trimmed {
		raw, _ := json.Marshal(result)
		var object map[string]any
		_ = json.Unmarshal(raw, &object)
		if rows, ok := object["results"].([]any); ok {
			positions := make([]int, len(rows))
			for index := range positions {
				positions[index] = index
			}
			retain("results", positions)
		}
	}
	return result, trimmed
}

// retireSymbolSessionPages refuses future publication into this owner even if
// shutdown races a first page before its cache has been created.
func symbolSessionPages(owner *sessionState) *symbolPageCache {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.symbolPages == nil {
		owner.symbolPages = &symbolPageCache{entries: make(map[string]*symbolPageSequence)}
	}
	return owner.symbolPages
}
func retireSymbolSessionPages(owner *sessionState) *symbolPageCache {
	cache := symbolSessionPages(owner)
	if cache != nil {
		cache.close()
	}
	return cache
}

// retireSymbolPages is normal terminal cleanup, not crash recovery. Cancellation
// and joining happen outside every session/map/cache lock. Like existing prune
// cleanup, cancellation cannot interrupt an OS call already in progress.
func (s *Server) retireSymbolPages() {
	caches := []*symbolPageCache{retireSymbolSessionPages(s.session)}
	if s.sessions != nil {
		s.sessions.mu.Lock()
		s.sessions.symbolPagesClosed = true
		caches = append(caches, s.sessions.retiredSymbolPages...)
		s.sessions.retiredSymbolPages = nil
		owners := make([]*sessionState, 0, len(s.sessions.sessions))
		for _, local := range s.sessions.sessions {
			owners = append(owners, local.session)
		}
		s.sessions.mu.Unlock()
		for _, owner := range owners {
			caches = append(caches, retireSymbolSessionPages(owner))
		}
	}
	for _, cache := range caches {
		if cache != nil {
			cache.close()
			cache.callWG.Wait()
		}
	}
}

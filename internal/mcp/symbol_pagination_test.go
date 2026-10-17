package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// Fixed graph, fixed backend order, and no ranking pipeline: increasing the
// candidate horizon must not move an already returned file-diversified tail
// into a later page or leave newly discovered diverse-head nodes behind it.
func TestSearchSymbolsPaginationKeepsDiversifiedSequence(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("path_scoped_%v", scoped), func(t *testing.T) {
			server := func() (*Server, *orderedBackend) {
				g := graph.New()
				backend := newOrderedBackend()
				for i := 0; i < 20; i++ {
					file := "repo/scope/shared.go"
					if i >= 10 {
						file = fmt.Sprintf("repo/scope/unique%02d.go", i)
					}
					id := fmt.Sprintf("%s::Opaque%02d", file, i)
					g.AddNode(&graph.Node{ID: id, Name: fmt.Sprintf("Opaque%02d", i), Kind: graph.KindFunction,
						FilePath: file, RepoPrefix: "repo", Language: "go", StartLine: i + 1})
					// The token is in indexed content, not the declaration name:
					// exact-name and substring splices cannot add deeper nodes.
					backend.put("needle", id)
				}
				eng := query.NewEngine(g)
				eng.SetSearch(backend)
				eng.SetRerank(nil)
				return NewServer(eng, g, nil, nil, zap.NewNop(), nil), backend
			}
			args := func(limit int, cursor string) map[string]any {
				a := map[string]any{"query": "Needle", "limit": limit, "max_per_file": 3, "max_bytes": 0}
				if scoped {
					a["path"] = "scope/"
				}
				if cursor != "" {
					a["cursor"] = cursor
				}
				return a
			}
			reference, _ := server()
			want := respIDs(searchResp(t, reference, args(2000, "")))
			require.Len(t, want, 20, "one-page immutable reference must contain every concrete backend hit")
			paged, backend := server()
			seen := map[string]bool{}
			duplicates := []string{}
			cursor := ""
			pages := 0
			for ; pages < 8; pages++ {
				response := searchResp(t, paged, args(4, cursor))
				for id := range respIDs(response) {
					if seen[id] {
						duplicates = append(duplicates, id)
					}
					seen[id] = true
				}
				next, _ := response["next_cursor"].(string)
				if next == "" {
					break
				}
				require.NotEqual(t, cursor, next, "cursor must advance")
				cursor = next
			}
			t.Logf("fixed immutable backend limits=%v pages=%d unique=%d duplicates=%v", backend.searchLimits(), pages+1, len(seen), duplicates)
			require.Less(t, pages, 8, "bounded concrete reference must exhaust")
			require.Empty(t, duplicates, "following next_cursor must never repeat a returned ID")
			require.Equal(t, want, seen, "the cursor walk must retain every one-page reference ID")
		})
	}
}

func TestSymbolPageObserverKeepsIdentityThroughFieldsAndKindDrop(t *testing.T) {
	observer := &symbolBudgetObservation{positions: []int{0, 1, 2, 3}}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"fields": "kind,name", "max_bytes": 650}
	payload := map[string]any{"results": []any{
		map[string]any{"id": "p0", "kind": "param", "name": strings.Repeat("p", 1000)},
		map[string]any{"id": "f1", "kind": "function", "name": "same"},
		map[string]any{"id": "p2", "kind": "param", "name": strings.Repeat("p", 1000)},
		map[string]any{"id": "f3", "kind": "function", "name": "same"},
	}}
	filtered := applyFieldsFilter(payload, parseFields("kind,name"))
	result, trimmed := applyDegradationObserved(filtered, degradeShapes["search_symbols"], 650, symbolBudgetRetainer(context.WithValue(t.Context(), symbolBudgetObservationKey{}, observer), req))
	require.True(t, trimmed)
	require.Equal(t, []int{1, 3}, observer.positions, "same projected values must retain physical candidate positions, not guessed IDs")
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"id"`)
	// Generic budget tail trimming follows the already transformed sidecar.
	small, _ := applySymbolObservedBudget(result, 1, symbolBudgetRetainer(context.WithValue(t.Context(), symbolBudgetObservationKey{}, observer), req))
	require.Empty(t, observer.positions)
	require.NotNil(t, small)
}

func TestSymbolPageContinuationReplaysAndRejectsOtherQueryOrView(t *testing.T) {
	g := graph.New()
	backend := newOrderedBackend()
	for i := range 24 {
		id := fmt.Sprintf("repo/f%d.go::Opaque%d", i, i)
		g.AddNode(&graph.Node{ID: id, Name: fmt.Sprintf("Opaque%d", i), Kind: graph.KindFunction, FilePath: fmt.Sprintf("repo/f%d.go", i), RepoPrefix: "repo"})
		backend.put("needle", id)
	}
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	call := func(ctx context.Context, queryText, cursor string) (*mcplib.CallToolResult, error) {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_symbols"
		req.Params.Arguments = map[string]any{"query": queryText, "limit": 4, "cursor": cursor, "max_bytes": 0}
		return srv.handleSearchSymbols(ctx, req)
	}
	first, err := call(t.Context(), "Needle", "")
	require.NoError(t, err)
	require.False(t, first.IsError)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(first.Content[0].(mcplib.TextContent).Text), &body))
	cursor := body["next_cursor"].(string)
	initialCalls := len(backend.searchLimits())
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	answers := make(chan answer, 2)
	for range 2 {
		go func() { result, err := call(t.Context(), "Needle", cursor); answers <- answer{result, err} }()
	}
	a, b := <-answers, <-answers
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	araw, err := json.Marshal(a.result)
	require.NoError(t, err)
	braw, err := json.Marshal(b.result)
	require.NoError(t, err)
	require.JSONEq(t, string(araw), string(braw), "identical cursor reads replay the same published wire page")
	require.Equal(t, initialCalls, len(backend.searchLimits()), "pending continuation must not refetch/rerank")
	wrong, err := call(t.Context(), "Another", cursor)
	require.NoError(t, err)
	require.True(t, wrong.IsError)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = call(cancelled, "Needle", cursor)
	require.ErrorIs(t, err, context.Canceled)
	foreign, err := call(WithSessionID(t.Context(), "other-session"), "Needle", cursor)
	require.NoError(t, err)
	require.True(t, foreign.IsError)
	g.AddNode(&graph.Node{ID: "repo/changed.go::Changed", Name: "Changed", Kind: graph.KindFunction, FilePath: "repo/changed.go"})
	changed, err := call(t.Context(), "Needle", cursor)
	require.NoError(t, err)
	require.True(t, changed.IsError)
}

func TestSymbolPageCacheBoundsAndExpiryAreExplicit(t *testing.T) {
	cache := &symbolPageCache{entries: map[string]*symbolPageSequence{}}
	for i := range symbolPageCacheEntries + 1 {
		cache.add(&symbolPageSequence{id: fmt.Sprint(i), created: time.Now()})
	}
	require.Nil(t, cache.lookup("0"))
	require.Len(t, cache.entries, symbolPageCacheEntries)
	old := &symbolPageSequence{id: "old", created: time.Now().Add(-symbolPageTTL)}
	cache.add(old)
	require.Nil(t, cache.lookup("old"))
	parsed, ok := parseSymbolCursor(encodeCursor(10))
	require.False(t, ok)
	require.Empty(t, parsed.Sequence, "legacy offsets retain the existing independent path")
}

func TestSymbolPageSpillRetainsCompleteWalkAndReplayBeyondInitialWindow(t *testing.T) {
	storage := newSymbolPageStorage()
	t.Cleanup(storage.close)
	var candidates []symbolPageCandidate
	for i := range 6000 {
		id := fmt.Sprintf("repo/file%05d.go::Opaque%05d", i, i)
		node, err := json.Marshal(&graph.Node{ID: id, FilePath: fmt.Sprintf("repo/file%05d.go", i)})
		require.NoError(t, err)
		row, err := json.Marshal(map[string]any{"id": id, "name": fmt.Sprintf("Opaque%05d", i), "kind": "function"})
		require.NoError(t, err)
		candidates = append(candidates, symbolPageCandidate{id: id, node: node, row: row})
	}
	stage, err := storage.begin(t.Context(), candidates)
	require.NoError(t, err)
	require.NotEmpty(t, storage.path, "compact state beyond the memory threshold must spill, not cap recall")
	artifact := storage.path
	seen := map[string]bool{}
	for page := 0; page < 60; page++ {
		selected, err := stage.page(t.Context(), 100)
		require.NoError(t, err)
		require.Len(t, selected, 100)
		var ids []string
		for _, candidate := range selected {
			require.False(t, seen[candidate.id])
			seen[candidate.id] = true
			ids = append(ids, candidate.id)
		}
		response, err := mcplib.NewToolResultJSON(map[string]any{"results": ids})
		require.NoError(t, err)
		require.NoError(t, stage.commit(t.Context(), page, symbolPageReplay{result: response}, selected))
		stage.rollback()
		replay, ok, err := storage.replay(t.Context(), page)
		require.NoError(t, err)
		require.True(t, ok)
		require.JSONEq(t, response.Content[0].(mcplib.TextContent).Text, replay.result.Content[0].(mcplib.TextContent).Text)
		stage, err = storage.begin(t.Context(), nil)
		require.NoError(t, err)
	}
	require.Equal(t, 6000, len(seen))
	require.Zero(t, stage.pending)
	stage.rollback()
	// Earlier cursor replay remains intact after the full walk completes.
	_, ok, err := storage.replay(t.Context(), 0)
	require.NoError(t, err)
	require.True(t, ok)
	storage.close()
	_, err = os.Stat(artifact)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// Done is first consulted at the sequence-token select, after cache lookup.
// This barrier makes lookup -> retirement -> token acquisition deterministic.
type symbolAdmissionBarrier struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (ctx *symbolAdmissionBarrier) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.entered) })
	return ctx.Context.Done()
}

type symbolBlockingBackend struct {
	*orderedBackend
	entered, proceed chan struct{}
	once             sync.Once
}

func (backend *symbolBlockingBackend) Search(query string, limit int) []search.SearchResult {
	backend.once.Do(func() { close(backend.entered); <-backend.proceed })
	return backend.orderedBackend.Search(query, limit)
}

func symbolLifecycleServer(t *testing.T) (*Server, *orderedBackend, mcplib.CallToolRequest) {
	t.Helper()
	g := graph.New()
	backend := newOrderedBackend()
	for i := range 24 {
		id := fmt.Sprintf("repo/f%d.go::Opaque%d", i, i)
		g.AddNode(&graph.Node{ID: id, Name: fmt.Sprintf("Opaque%d", i), QualName: fmt.Sprintf("pkg.Opaque%d", i), Kind: graph.KindFunction, FilePath: fmt.Sprintf("repo/f%d.go", i), StartLine: i + 1, RepoPrefix: "repo", Meta: map[string]any{"signature": "func Opaque()", "doc": "not retained"}})
		backend.put("needle", id)
	}
	engine := query.NewEngine(g)
	engine.SetSearch(backend)
	engine.SetRerank(nil)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": "Needle", "limit": 4, "max_bytes": 0}
	return NewServer(engine, g, nil, nil, zap.NewNop(), nil), backend, req
}

func TestSymbolPageRetiredWaiterDoesNotReadClosedSpill(t *testing.T) {
	for _, mode := range []string{"session_release", "eviction"} {
		t.Run(mode, func(t *testing.T) {
			srv, _, req := symbolLifecycleServer(t)
			ctx, cancel := context.WithTimeout(WithSessionID(t.Context(), "pagination-lifecycle"), 5*time.Second)
			defer cancel()
			first, err := srv.handleSearchSymbols(ctx, req)
			require.NoError(t, err)
			body := first.StructuredContent.(map[string]any)
			cursor, ok := parseSymbolCursor(body["next_cursor"].(string))
			require.True(t, ok)
			cache := srv.symbolPages(ctx)
			entry := cache.lookup(cursor.Sequence)
			require.NotNil(t, entry)
			<-entry.token
			require.NoError(t, entry.storage.spill(ctx))
			artifact := entry.storage.path
			req.Params.Arguments = map[string]any{"query": "Needle", "limit": 4, "max_bytes": 0, "cursor": body["next_cursor"]}
			barrier := &symbolAdmissionBarrier{Context: ctx, entered: make(chan struct{})}
			type answer struct {
				result *mcplib.CallToolResult
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				admitted, err := entry.acquire(barrier)
				var result *mcplib.CallToolResult
				if !admitted && err == nil {
					result = symbolPageError("expired or was evicted")
				}
				done <- answer{result, err}
			}()
			select {
			case <-barrier.entered:
			case <-ctx.Done():
				entry.token <- struct{}{}
				t.Fatal(ctx.Err())
			}
			if mode == "session_release" {
				srv.ReleaseSession("pagination-lifecycle")
			} else {
				for i := range symbolPageCacheEntries {
					cache.add(&symbolPageSequence{id: fmt.Sprint(i), created: time.Now()})
				}
			}
			require.True(t, entry.retired.Load())
			entry.token <- struct{}{}
			select {
			case answer := <-done:
				require.NoError(t, answer.err)
				require.True(t, answer.result.IsError)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// The waiting caller owns cleanup once the active token is returned.
			_, err = os.Stat(artifact)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestSymbolPageReleaseRefusesInFlightFirstPagePublication(t *testing.T) {
	srv, backend, req := symbolLifecycleServer(t)
	blocking := &symbolBlockingBackend{orderedBackend: backend, entered: make(chan struct{}), proceed: make(chan struct{})}
	srv.engine.SetSearch(blocking)
	ctx, cancel := context.WithTimeout(WithSessionID(t.Context(), "released-first-page"), 5*time.Second)
	defer cancel()
	var release sync.Once
	defer release.Do(func() { close(blocking.proceed) })
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	done := make(chan answer, 1)
	go func() { result, err := srv.handleSearchSymbols(ctx, req); done <- answer{result, err} }()
	select {
	case <-blocking.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	oldCache := srv.symbolPages(ctx)
	srv.ReleaseSession("released-first-page")
	release.Do(func() { close(blocking.proceed) })
	select {
	case answer := <-done:
		require.ErrorIs(t, answer.err, context.Canceled)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	oldCache.mu.Lock()
	require.True(t, oldCache.closed)
	require.Empty(t, oldCache.entries)
	oldCache.mu.Unlock()
	srv.sessions.mu.Lock()
	_, exists := srv.sessions.sessions["released-first-page"]
	srv.sessions.mu.Unlock()
	require.False(t, exists, "active call must not recreate the released session while publishing")
	srv.ReleaseSession("unknown-session")
	srv.sessions.mu.Lock()
	_, exists = srv.sessions.sessions["unknown-session"]
	srv.sessions.mu.Unlock()
	require.False(t, exists, "release must not create an unknown session")
}

func TestSymbolPagePreservesTypedLocalizationOnFirstPageAndReplay(t *testing.T) {
	srv, _, req := symbolLifecycleServer(t)
	firstContext := withLocalizationPermittedEvidenceCapture(t.Context(), 1)
	first, err := srv.handleSearchSymbols(firstContext, req)
	require.NoError(t, err)
	require.False(t, first.IsError)
	rows, ok := localizationCapturedEvidence(firstContext, 1)
	require.True(t, ok)
	require.Len(t, rows, 4)
	for _, row := range rows {
		require.NotEmpty(t, row.Name)
		require.NotEmpty(t, row.QualName)
		require.Equal(t, "function", row.Kind)
		require.Positive(t, row.Line)
		require.Equal(t, "func Opaque()", row.Signature)
	}
	req.Params.Arguments = map[string]any{"query": "Needle", "limit": 4, "max_bytes": 0, "cursor": first.StructuredContent.(map[string]any)["next_cursor"]}
	nextContext := withLocalizationPermittedEvidenceCapture(t.Context(), 2)
	_, err = srv.handleSearchSymbols(nextContext, req)
	require.NoError(t, err)
	nextRows, ok := localizationCapturedEvidence(nextContext, 2)
	require.True(t, ok)
	replayContext := withLocalizationPermittedEvidenceCapture(t.Context(), 3)
	_, err = srv.handleSearchSymbols(replayContext, req)
	require.NoError(t, err)
	replayRows, ok := localizationCapturedEvidence(replayContext, 3)
	require.True(t, ok)
	require.Equal(t, nextRows, replayRows, "replay must retain exact typed localization evidence")
}

func TestSymbolPageDefaultHundredWalkKeepsGrowingDiversifiedHorizon(t *testing.T) {
	g := graph.New()
	backend := newOrderedBackend()
	for i := range 240 {
		file := "repo/shared.go"
		if i >= 140 {
			file = fmt.Sprintf("repo/f%d.go", i)
		}
		id := fmt.Sprintf("%s::Opaque%d", file, i)
		g.AddNode(&graph.Node{ID: id, Name: fmt.Sprintf("Opaque%d", i), Kind: graph.KindFunction, FilePath: file})
		backend.put("needle", id)
	}
	engine := query.NewEngine(g)
	engine.SetSearch(backend)
	engine.SetRerank(nil)
	srv := NewServer(engine, g, nil, nil, zap.NewNop(), nil)
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 8; page++ {
		response := searchResp(t, srv, map[string]any{"query": "Needle", "limit": 100, "max_per_file": 3, "max_bytes": 0, "cursor": cursor})
		for id := range respIDs(response) {
			require.False(t, seen[id], "cursor repeated %s", id)
			seen[id] = true
		}
		cursor, _ = response["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	require.Empty(t, cursor, "fixed wider result must exhaust")
	require.Len(t, seen, 240, "default-sized pages must retain hits beyond the initial retrieval horizon")
}

func TestSymbolPageNormalShutdownRemovesDefaultAndSessionSpills(t *testing.T) {
	srv, _, req := symbolLifecycleServer(t)
	var artifacts []string
	var caches []*symbolPageCache
	for _, ctx := range []context.Context{t.Context(), WithSessionID(t.Context(), "owned-session")} {
		result, err := srv.handleSearchSymbols(ctx, req)
		require.NoError(t, err)
		require.False(t, result.IsError)
		cursor, ok := parseSymbolCursor(result.StructuredContent.(map[string]any)["next_cursor"].(string))
		require.True(t, ok)
		cache := srv.symbolPages(ctx)
		caches = append(caches, cache)
		entry := cache.lookup(cursor.Sequence)
		require.NotNil(t, entry)
		<-entry.token
		require.NoError(t, entry.storage.spill(t.Context()))
		artifacts = append(artifacts, entry.storage.path)
		entry.token <- struct{}{}
	}
	// SharedServer.Close and the CLI signal-return path already invoke this
	// existing terminal owner. ServeStdio also defers the same cursor cleanup.
	srv.DrainBackground()
	srv.DrainBackground() // normal cleanup is idempotent
	for _, artifact := range artifacts {
		_, err := os.Stat(artifact)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	for _, cache := range caches {
		cache.mu.Lock()
		require.True(t, cache.closed)
		require.Empty(t, cache.entries)
		cache.mu.Unlock()
	}
	late := srv.symbolPages(WithSessionID(t.Context(), "after-shutdown"))
	require.False(t, late.add(&symbolPageSequence{id: "late", created: time.Now()}), "shutdown must refuse later continuation publication")
}

func TestSymbolPageFirstResponseSurvivesUnrelatedMutation(t *testing.T) {
	for _, limit := range []int{4, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			srv, backend, req := symbolLifecycleServer(t)
			req.Params.Arguments = map[string]any{"query": "Needle", "limit": limit, "max_bytes": 0}
			blocking := &symbolBlockingBackend{orderedBackend: backend, entered: make(chan struct{}), proceed: make(chan struct{})}
			srv.engine.SetSearch(blocking)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var unpark sync.Once
			defer unpark.Do(func() { close(blocking.proceed) })
			type answer struct {
				result *mcplib.CallToolResult
				err    error
			}
			done := make(chan answer, 1)
			go func() { result, err := srv.handleSearchSymbols(ctx, req); done <- answer{result, err} }()
			select {
			case <-blocking.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			srv.graph.AddNode(&graph.Node{ID: "other/unrelated.go::Changed", Name: "Changed", Kind: graph.KindFunction, FilePath: "other/unrelated.go"})
			unpark.Do(func() { close(blocking.proceed) })
			var result *mcplib.CallToolResult
			select {
			case answer := <-done:
				require.NoError(t, answer.err)
				result = answer.result
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.False(t, result.IsError, "existing valid first response must survive unrelated writes")
			body := result.StructuredContent.(map[string]any)
			require.Len(t, body["results"], min(limit, 24))
			if limit == 4 {
				req.Params.Arguments = map[string]any{"query": "Needle", "limit": limit, "max_bytes": 0, "cursor": body["next_cursor"]}
				stale, err := srv.handleSearchSymbols(ctx, req)
				require.NoError(t, err)
				require.True(t, stale.IsError)
			} else {
				require.NotContains(t, body, "next_cursor")
				cache := srv.symbolPages(ctx)
				cache.mu.Lock()
				require.Empty(t, cache.entries)
				cache.mu.Unlock()
			}
		})
	}
}

func TestSymbolPageTerminalShutdownCancelsAndJoinsActiveCall(t *testing.T) {
	srv, backend, req := symbolLifecycleServer(t)
	blocking := &symbolBlockingBackend{orderedBackend: backend, entered: make(chan struct{}), proceed: make(chan struct{})}
	srv.engine.SetSearch(blocking)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var unpark sync.Once
	defer unpark.Do(func() { close(blocking.proceed) })
	callDone := make(chan error, 1)
	go func() { _, err := srv.handleSearchSymbols(ctx, req); callDone <- err }()
	select {
	case <-blocking.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cache := srv.symbolPages(ctx)
	cache.mu.Lock()
	var owned *symbolPageCall
	for call := range cache.calls {
		owned = call
	}
	cache.mu.Unlock()
	require.NotNil(t, owned)
	shutdownDone := make(chan struct{})
	go func() { srv.DrainBackground(); close(shutdownDone) }()
	// Cancellation must reach the owned call, but teardown must not abandon it
	// while a noninterruptible backend call is deliberately still parked.
	ownedContextCancelled := make(chan struct{})
	// cache closure is synchronous before the terminal join.
	go func() { cache.close(); close(ownedContextCancelled) }()
	select {
	case <-ownedContextCancelled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before active cursor cleanup")
	default:
	}
	unpark.Do(func() { close(blocking.proceed) })
	select {
	case err := <-callDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-shutdownDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSymbolPageReleasedActiveOwnerStillJoinsTerminalDrain(t *testing.T) {
	srv, backend, req := symbolLifecycleServer(t)
	blocking := &symbolBlockingBackend{orderedBackend: backend, entered: make(chan struct{}), proceed: make(chan struct{})}
	srv.engine.SetSearch(blocking)
	ctx, cancel := context.WithTimeout(WithSessionID(t.Context(), "removed-active-owner"), 5*time.Second)
	defer cancel()
	var unpark sync.Once
	defer unpark.Do(func() { close(blocking.proceed) })
	callDone := make(chan error, 1)
	go func() { _, err := srv.handleSearchSymbols(ctx, req); callDone <- err }()
	select {
	case <-blocking.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cache := srv.symbolPages(ctx)
	cache.mu.Lock()
	var call *symbolPageCall
	for current := range cache.calls {
		call = current
	}
	cache.mu.Unlock()
	require.NotNil(t, call)
	srv.ReleaseSession("removed-active-owner")
	select {
	case <-call.done:
	case <-ctx.Done():
		t.Fatal("session release did not cancel owned call")
	}
	srv.sessions.mu.Lock()
	require.Empty(t, srv.sessions.sessions, "removed session must stay absent from the live inventory")
	require.Equal(t, []*symbolPageCache{cache}, srv.sessions.retiredSymbolPages)
	srv.sessions.mu.Unlock()
	drainDone := make(chan struct{})
	go func() { srv.DrainBackground(); close(drainDone) }()
	// Observe terminal ownership transfer, not a guessed delay or new shutdown
	// timeout. Its collected cache still has the deliberately parked active call.
	for {
		srv.sessions.mu.Lock()
		collected := len(srv.sessions.retiredSymbolPages) == 0
		srv.sessions.mu.Unlock()
		if collected {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	require.True(t, cache.hasCalls())
	select {
	case <-drainDone:
		t.Fatal("terminal drain abandoned the removed active owner")
	default:
	}
	unpark.Do(func() { close(blocking.proceed) })
	select {
	case err := <-callDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-drainDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.False(t, cache.hasCalls())
	srv.sessions.mu.Lock()
	require.Empty(t, srv.sessions.retiredSymbolPages)
	require.Empty(t, srv.sessions.sessions)
	srv.sessions.mu.Unlock()
}

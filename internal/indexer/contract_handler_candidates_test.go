package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	gosit "github.com/zzet/gortex/internal/parser/tsitter/golang"
	"go.uber.org/zap"
)

type handlerDeclarationCountingReader struct {
	*capturedRepoNameCounter
	declarations int
}

func (r *handlerDeclarationCountingReader) VisitNodeDeclarationsByNamesInRepoContext(ctx context.Context, names []string, repo string, yield func(*graph.Node) bool) error {
	r.declarations++
	return r.store.VisitNodeDeclarationsByNamesInRepoContext(ctx, names, repo, yield)
}

func TestContractHandlerDeclarationsKeepFullLookupIndependent(t *testing.T) {
	e, counts := completedNameSelectedFixture(t)
	e.ledger = newContractUnitLedger()
	var projectedReaders []*handlerDeclarationCountingReader
	for repo, reader := range e.core.(*contractCapturedCore).readers {
		wrapped := &handlerDeclarationCountingReader{capturedRepoNameCounter: reader.(*capturedRepoNameCounter)}
		projectedReaders = append(projectedReaders, wrapped)
		e.core.(*contractCapturedCore).readers[repo] = wrapped
	}
	names := []string{"Match", "Missing", "bad\xff"}
	got, ok := e.contractHandlerDeclarations(names)
	require.True(t, ok)
	require.NoError(t, e.err)
	require.Len(t, got["Match"], 2)
	require.Empty(t, got["Missing"])
	require.Empty(t, e.scratch.FindNodesByNames(names), "declarations must not be imported as complete nodes")
	require.Empty(t, e.completedCoreNames)
	require.Len(t, e.ledger.reads, 1)
	require.Equal(t, contractReadHandlerDeclarations, e.ledger.reads[0].kind)
	require.ElementsMatch(t, []string{"a/current.go", "b/base.go"}, e.ledger.reads[0].owners, "foreign candidates remain proof owners")
	for _, n := range got["Match"] {
		require.Empty(t, n.Meta)
		require.Zero(t, n.StartLine)
	}
	for _, r := range projectedReaders {
		require.Equal(t, 1, r.declarations)
		require.Zero(t, r.scoped, "handler discovery must use the actual SQLite declaration projection")
	}
	full := e.FindNodesByNames(names)
	require.NoError(t, e.err)
	for _, n := range full["Match"] {
		require.NotEmpty(t, n.Meta)
	}
	require.Equal(t, []string{"Match", "Missing", "bad\xff"}, names)
	for _, r := range counts {
		require.Equal(t, 1, r.scoped, "the later full lookup must still hydrate complete rows")
	}
}

func TestContractHandlerDeclarationsPreserveScratchPrecedence(t *testing.T) {
	for _, mode := range []string{"identical", "renamed", "kind", "file", "repo", "scratch_only", "contract", "duplicate_names"} {
		t.Run(mode, func(t *testing.T) {
			core := graph.New()
			n := &graph.Node{ID: "a/f.go::Handle", Name: "Handle", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", StartLine: 7, Meta: map[string]any{"core": true}}
			core.AddNode(n)
			e := completedNameEvidence(t, core)
			scratch := *n
			scratch.Meta = map[string]any{"scratch": true}
			names := []string{"Handle", "Absent"}
			switch mode {
			case "renamed":
				scratch.Name = "Other"
			case "kind":
				scratch.Kind = graph.KindType
			case "file":
				scratch.FilePath = "a/other.go"
			case "repo":
				scratch.RepoPrefix = "b"
			case "scratch_only":
				scratch.ID = "a/derived.go::Handle"
			case "contract":
				scratch.Kind = graph.KindContract
			case "duplicate_names":
				names = append(names, "Handle")
			}
			e.scratch.AddNode(&scratch)
			got, handled := e.contractHandlerDeclarations(names)
			require.NoError(t, e.err)
			if mode == "identical" {
				require.True(t, handled)
				require.True(t, contractSameHandlerDeclaration(n, got["Handle"][0]))
				require.Empty(t, e.completedCoreNames)
			} else {
				require.False(t, handled, "uncertain scratch precedence must use unchanged full lookup")
				require.Nil(t, got)
			}
			full := e.FindNodesByNames(names)
			require.NoError(t, e.err)
			found := e.scratch.GetNode(scratch.ID)
			require.Equal(t, scratch.Meta, found.Meta, "full lookup never overwrites an existing scratch ID")
			if mode == "renamed" {
				require.Empty(t, full["Handle"])
			}
		})
	}
}

func TestContractHandlerDeclarationReplayTracksNewAndForeignMatches(t *testing.T) {
	core := graph.New()
	core.AddNode(&graph.Node{ID: "b/f.go::Handle", Name: "Handle", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/f.go", Meta: map[string]any{"full": true}})
	e := completedNameEvidence(t, core)
	e.allowedRepos["b"] = true
	e.ledger = newContractUnitLedger()
	_, ok := e.contractHandlerDeclarations([]string{"Handle", "Missing"})
	require.True(t, ok)
	require.NoError(t, e.err)
	read := e.ledger.reads[0]
	c := &contractConvergence{ctx: t.Context(), snap: ContractFollowupSnapshot{Core: core}}
	answer, err := c.ask(e, read)
	require.NoError(t, err)
	digest, valid := contractAnswerDigest(answer)
	require.True(t, valid)
	require.Equal(t, read.digest, digest)
	// A new match, including a non-handler kind, must invalidate a negative.
	core.AddNode(&graph.Node{ID: "a/new.go::Missing", Name: "Missing", Kind: graph.KindType, RepoPrefix: "a", FilePath: "a/new.go"})
	answer, err = c.ask(e, read)
	require.NoError(t, err)
	digest, valid = contractAnswerDigest(answer)
	require.True(t, valid)
	require.NotEqual(t, read.digest, digest)
	m := contractMoves{declaredKnown: true, declared: map[string]bool{"name:Missing": true}, byPath: map[string][]string{"a/new.go": {"name:Missing"}}}
	require.True(t, m.affected(read))
	require.True(t, m.depends("a/new.go", read))
	require.True(t, (contractMoves{moved: map[string]bool{"b/f.go": true}, declaredKnown: true}).affected(read), "nonchosen foreign owners are dependencies")
	// A previously representable proof must not turn an invalid raw spelling
	// into the same JSON replacement-character digest during revalidation.
	changed := *core.GetNode("b/f.go::Handle")
	changed.FilePath = "b/bad\xff.go"
	core.AddNode(&changed)
	_, err = c.ask(e, read)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
}

func TestContractHandlerDeclarationsRejectPartialProofs(t *testing.T) {
	for _, mode := range []string{"error", "canceled", "limit", "namespace", "scratch_closed", "bytes", "raw_name"} {
		t.Run(mode, func(t *testing.T) {
			r := &completedNameRows{Reader: graph.New(), rows: []*graph.Node{{ID: "a/f::Handle", Name: "Handle", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f"}}}
			e := completedNameEvidence(t, r)
			e.ledger = newContractUnitLedger()
			cause := errors.New("checked reader failed")
			switch mode {
			case "error":
				r.finish = cause
			case "canceled":
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				e.ctx = ctx
				r.cancel = cancel
			case "limit":
				r.counts = map[string]int{"Handle": graph.ContractProjectionRowLimit}
			case "namespace":
				r.rows[0].RepoPrefix = "outside"
			case "scratch_closed":
				require.NoError(t, e.scratch.Close())
			case "bytes":
				r.rows[0].FilePath = strings.Repeat("x", contractFollowupCompactLimit+1)
			case "raw_name":
				r.rows[0].FilePath = "a/bad\xff.go"
			}
			got, handled := e.contractHandlerDeclarations([]string{"Handle"})
			require.Nil(t, got)
			require.Empty(t, e.ledger.reads)
			require.Empty(t, e.completedCoreNames)
			if mode == "bytes" || mode == "raw_name" {
				require.False(t, handled)
				require.NoError(t, e.err)
			} else {
				require.True(t, handled)
				require.Error(t, e.err)
			}
			if mode == "error" {
				require.ErrorIs(t, e.err, cause)
			}
			if mode == "canceled" {
				require.ErrorIs(t, e.err, context.Canceled)
			}
			if mode == "limit" {
				require.ErrorIs(t, e.err, graph.ErrContractProjectionLimit)
			}
		})
	}
}

type handlerDeclarationTestStore struct {
	graph.Store
	declarations map[string][]*graph.Node
}

func (s *handlerDeclarationTestStore) contractHandlerDeclarations([]string) (map[string][]*graph.Node, bool) {
	return s.declarations, true
}

func TestContractProjectedHandlersEqualFullRoutingAndRefuseMissingOwner(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "handlers.go"), []byte("package handlers\nfunc Handle() {}\n"), 0600))
	base := graph.New()
	handler := &graph.Node{ID: "a/handlers.go::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: "a/handlers.go", RepoPrefix: "a", StartLine: 2, EndLine: 2, Language: "go"}
	base.AddNode(handler)
	for _, mode := range []string{"full", "projected", "missing_owner", "empty_projection"} {
		t.Run(mode, func(t *testing.T) {
			counting := &residualBatchCountingStore{Store: base}
			var store graph.Store = counting
			if mode != "full" {
				declaration := contractHandlerDeclaration(handler)
				if mode == "missing_owner" {
					declaration.ID = "a/handlers.go::Absent"
				}
				rows := map[string][]*graph.Node{"Handle": {declaration}}
				if mode == "empty_projection" {
					rows = nil
				}
				store = &handlerDeclarationTestStore{Store: counting, declarations: rows}
			}
			idx := &Indexer{graph: store, rootPath: root, repoPrefix: "a", logger: zap.NewNop()}
			if declared, ok := store.(*handlerDeclarationTestStore); ok {
				idx.contractHandlerCandidates = declared.contractHandlerDeclarations
			}
			reg := contracts.NewRegistry()
			reg.Add(contracts.Contract{ID: "http::one", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "a/router.go", Line: 9, RepoPrefix: "a", Meta: map[string]any{"handler_ident": "Handle"}})
			idx.resolveProviderHandlers(reg)
			got := reg.ByID("http::one")[0]
			if mode != "full" {
				require.Zero(t, counting.findNodesByNameCalls)
				require.Zero(t, counting.findNodesByNamesCalls)
			}
			switch mode {
			case "empty_projection":
				require.NoError(t, idx.contractInputError())
				require.Empty(t, got.SymbolID)
			case "missing_owner":
				require.ErrorIs(t, idx.contractInputError(), graph.ErrContractProjectionStale)
				require.Empty(t, got.SymbolID)
			default:
				require.NoError(t, idx.contractInputError())
				require.Equal(t, handler.ID, got.SymbolID)
				require.Equal(t, "a/router.go", got.FilePath)
				require.Equal(t, 9, got.Line)
			}
		})
	}
}

func TestContractProjectedHandlerHydrationKeepsSameNameBodyFacts(t *testing.T) {
	src := []byte("package handlers\ntype A struct{}\ntype B struct{}\nfunc (A) Handle() { WriteJSON(w,200,First{}) }\nfunc (B) Handle() { WriteJSON(w,201,Second{}) }\n")
	tree, err := parser.ParseFile(src, gosit.GetLanguage())
	require.NoError(t, err)
	pt := parser.NewParseTree(tree, src, "go")
	defer pt.Release()
	first := &graph.Node{ID: "a/f.go::A.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: "a/f.go", RepoPrefix: "a", StartLine: 4, EndLine: 4}
	chosen := &graph.Node{ID: "a/f.go::B.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: "a/f.go", RepoPrefix: "a", StartLine: 5, EndLine: 5, Meta: map[string]any{"precedence": "scratch"}}
	thin := contractHandlerDeclaration(chosen)
	require.Equal(t, "First{}", contracts.MakeBodyFacts(pt, thin).ResponseCalls()[0].ValueExpr, "thin name-only fallback is the adversarial control")
	full := contractHydrateHandler(thin, map[string]*graph.Node{first.ID: first, chosen.ID: chosen}[thin.ID])
	require.Same(t, chosen, full)
	require.Equal(t, contracts.MakeBodyFacts(pt, chosen).ResponseCalls(), contracts.MakeBodyFacts(pt, full).ResponseCalls())
	require.Equal(t, "Second{}", contracts.MakeBodyFacts(pt, full).ResponseCalls()[0].ValueExpr)
	changed := *chosen
	changed.Kind = graph.KindFunction
	require.Nil(t, contractHydrateHandler(thin, &changed), "ID alone cannot certify full handler hydration")
	// Existing full name discovery captures scratch precedence before loading
	// an unread owner file. Exercise the same ordering against real SQLite.
	core := graph.New()
	coreVersion := *chosen
	coreVersion.StartLine = 4
	coreVersion.Meta = map[string]any{"precedence": "core"}
	core.AddNode(&coreVersion)
	core.AddNode(&graph.Node{ID: "a/f.go", Name: "f.go", Kind: graph.KindFile, RepoPrefix: "a", FilePath: "a/f.go"})
	evidence := completedNameEvidence(t, core)
	evidence.files = map[string]ContractFollowupFile{"a/f.go": {RepoPrefix: "a", Path: "a/f.go"}}
	evidence.scratch.AddNode(chosen)
	legacy := evidence.FindNodesByNames([]string{"Handle"})["Handle"][0]
	selected := contractHydrateHandler(thin, evidence.GetNodesByIDs([]string{thin.ID})[thin.ID])
	require.NotNil(t, selected)
	evidence.GetFileNodesByPaths([]string{"a/f.go"})
	require.NoError(t, evidence.err)
	require.Equal(t, 4, evidence.scratch.GetNode(chosen.ID).StartLine, "the owner-file load really refreshed the row")
	require.Equal(t, legacy.StartLine, selected.StartLine)
	require.Equal(t, legacy.Meta, selected.Meta)
	require.NotEqual(t, evidence.scratch.GetNode(chosen.ID).Meta, selected.Meta)
	require.Equal(t, "Second{}", contracts.MakeBodyFacts(pt, selected).ResponseCalls()[0].ValueExpr)
}

func TestContractHandlerDeclarationsKeepCandidateSelection(t *testing.T) {
	core := graph.New()
	core.AddBatch([]*graph.Node{
		{ID: "a/pkg/f.go::A.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: "a/pkg/f.go", RepoPrefix: "a"},
		{ID: "a/other/f.go::B.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: "a/other/f.go", RepoPrefix: "a"},
		{ID: "b/f.go::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: "b/f.go", RepoPrefix: "b"},
		{ID: "b/f.go::Only", Name: "Only", Kind: graph.KindFunction, FilePath: "b/f.go", RepoPrefix: "b"},
		{ID: "a/f.go::Type", Name: "Handle", Kind: graph.KindType, FilePath: "a/f.go", RepoPrefix: "a"},
	}, nil)
	e := completedNameEvidence(t, core)
	e.allowedRepos["b"] = true
	e.handlerSeededFiles = map[string]bool{"a/pkg/f.go": true, "a/other/f.go": true, "a/f.go": true}
	e.files = map[string]ContractFollowupFile{}
	for file := range e.handlerSeededFiles {
		e.files[file] = ContractFollowupFile{RepoPrefix: "a", Path: file}
	}
	thin, ok := e.contractHandlerDeclarations([]string{"Handle", "Only", "Missing"})
	require.True(t, ok)
	require.NoError(t, e.err)
	full := e.FindNodesByNames([]string{"Handle", "Only", "Missing"})
	require.NoError(t, e.err)
	for _, tc := range []struct{ repo, dir, name, want string }{
		{"a", "a/pkg", "Handle", "a/pkg/f.go::A.Handle"},
		{"a", "", "Handle", ""},
		{"b", "", "Handle", "b/f.go::Handle"},
		{"missing", "", "Only", "b/f.go::Only"},
		{"missing", "", "Handle", ""},
		{"a", "", "Missing", ""},
	} {
		projected := lookupHandlerFromCandidates(tc.name, tc.repo, tc.dir, thin)
		reference := lookupHandlerFromCandidates(tc.name, tc.repo, tc.dir, full)
		if tc.want == "" {
			require.Nil(t, projected)
			require.Nil(t, reference)
		} else {
			require.NotNil(t, projected)
			require.NotNil(t, reference)
			require.Equal(t, tc.want, projected.ID)
			require.Equal(t, reference.ID, projected.ID)
		}
	}
}

func TestContractHandlerDeclarationsKeepInertNameCollisions(t *testing.T) {
	core := graph.New()
	handler := &graph.Node{ID: "a/handler.go::Handle", Name: "Handle", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/handler.go", StartLine: 7}
	core.AddNode(handler)
	for _, kind := range []graph.NodeKind{graph.KindBuiltin, graph.KindConstant, graph.KindLocal, graph.KindParam, graph.KindVariable} {
		core.AddNode(&graph.Node{ID: "b/collisions.go::" + string(kind), Name: "Handle", Kind: kind, RepoPrefix: "b", FilePath: "b/collisions.go"})
	}
	e := completedNameEvidence(t, core)
	e.allowedRepos["b"] = true
	e.ledger = newContractUnitLedger()
	names := []string{"Handle", "Absent"}
	got, handled := e.contractHandlerDeclarations(names)
	require.True(t, handled, "inert same-name rows must not force a full name import")
	require.NoError(t, e.err)
	require.Len(t, got["Handle"], 6, "all kinds still participate in the proof")
	require.Empty(t, e.completedCoreNames)
	require.Empty(t, e.scratch.FindNodesByNames(names), "no thin rows may be imported")
	read := e.ledger.reads[0]
	require.ElementsMatch(t, []string{"a/handler.go", "b/collisions.go"}, read.owners)
	require.Equal(t, handler.ID, lookupHandlerFromCandidates("Handle", "a", "a", got).ID)
	full := e.FindNodesByNames(names)
	require.NoError(t, e.err)
	require.Equal(t, lookupHandlerFromCandidates("Handle", "a", "a", full).ID, lookupHandlerFromCandidates("Handle", "a", "a", got).ID)
	require.Empty(t, e.outputIDs)
	require.Empty(t, e.springIDs)
	require.Empty(t, e.javaMethodIDs)
	c := &contractConvergence{ctx: t.Context(), snap: ContractFollowupSnapshot{Core: core}}
	answer, err := c.ask(e, read)
	require.NoError(t, err)
	digest, valid := contractAnswerDigest(answer)
	require.True(t, valid)
	require.Equal(t, read.digest, digest)
	core.AddNode(&graph.Node{ID: "b/new.go::Absent#param", Name: "Absent", Kind: graph.KindParam, RepoPrefix: "b", FilePath: "b/new.go"})
	answer, err = c.ask(e, read)
	require.NoError(t, err)
	digest, valid = contractAnswerDigest(answer)
	require.True(t, valid)
	require.NotEqual(t, read.digest, digest, "a new inert match must invalidate a previously negative witness")
	require.True(t, (contractMoves{moved: map[string]bool{"b/collisions.go": true}, declaredKnown: true}).affected(read))
	core.AddNode(&graph.Node{ID: "a/unknown.go::Handle", Name: "Handle", Kind: graph.NodeKind("unknown_handler_collision"), RepoPrefix: "a", FilePath: "a/unknown.go"})
	_, handled = e.contractHandlerDeclarations(names)
	require.False(t, handled, "unknown kinds retain the full lookup fallback")
	require.NoError(t, e.err)
}

type handlerOutcomeEvidence struct {
	*contractFollowupEvidence
	handled  int
	hydrated map[string]*graph.Node
}

func (e *handlerOutcomeEvidence) contractHandlerDeclarations(names []string) (map[string][]*graph.Node, bool) {
	rows, handled := e.contractFollowupEvidence.contractHandlerDeclarations(names)
	if handled {
		e.handled++
	}
	return rows, handled
}

func (e *handlerOutcomeEvidence) GetNodesByIDs(ids []string) map[string]*graph.Node {
	rows := e.contractFollowupEvidence.GetNodesByIDs(ids)
	e.hydrated = rows
	return rows
}

func TestContractHandlerInertCollisionsPreserveFullBodyAndScratchPrecedence(t *testing.T) {
	root := t.TempDir()
	source := "package handlers\ntype A struct{}\ntype B struct{}\nfunc (A) Handle() { WriteJSON(w,200,First{}) }\nfunc (B) Handle() { WriteJSON(w,201,Second{}) }\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "handlers.go"), []byte(source), 0600))
	tree, err := parser.ParseFile([]byte(source), gosit.GetLanguage())
	require.NoError(t, err)
	parsed := parser.NewParseTree(tree, []byte(source), "go")
	defer parsed.Release()
	core := graph.New()
	chosen := &graph.Node{ID: "a/handlers.go::B.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: "a/handlers.go", RepoPrefix: "a", StartLine: 5, EndLine: 5, Language: "go", Meta: map[string]any{"precedence": "scratch"}}
	old := *chosen
	old.StartLine = 4
	old.Meta = map[string]any{"precedence": "core"}
	core.AddBatch([]*graph.Node{
		&old,
		{ID: "a/handlers.go", Name: "handlers.go", Kind: graph.KindFile, FilePath: "a/handlers.go", RepoPrefix: "a"},
		{ID: "a/collisions.go::other#param", Name: "Handle", Kind: graph.KindParam, FilePath: "a/collisions.go", RepoPrefix: "a"},
		{ID: "a/collisions.go::other#local", Name: "Handle", Kind: graph.KindLocal, FilePath: "a/collisions.go", RepoPrefix: "a"},
	}, nil)
	var want []contracts.Contract
	for _, projected := range []bool{false, true} {
		e := completedNameEvidence(t, core)
		e.files = map[string]ContractFollowupFile{chosen.FilePath: {RepoPrefix: "a", Path: chosen.FilePath}}
		e.handlerSeededFiles = map[string]bool{chosen.FilePath: true}
		e.scratch.AddNode(chosen)
		var store graph.Store = &residualBatchCountingStore{Store: e}
		observed := &handlerOutcomeEvidence{contractFollowupEvidence: e}
		if projected {
			store = observed
		}
		idx := &Indexer{graph: store, rootPath: root, repoPrefix: "a", logger: zap.NewNop()}
		if projected {
			idx.contractHandlerCandidates = observed.contractHandlerDeclarations
		}
		reg := contracts.NewRegistry()
		reg.Add(contracts.Contract{ID: "http::one", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "a/router.go", Line: 9, RepoPrefix: "a", Meta: map[string]any{"handler_ident": "Handle"}})
		idx.resolveProviderHandlers(reg)
		require.NoError(t, e.err)
		require.NoError(t, idx.contractInputError())
		got := reg.ByID("http::one")[0]
		require.Equal(t, chosen.ID, got.SymbolID)
		require.Equal(t, "a/router.go", got.FilePath)
		require.Equal(t, 9, got.Line)
		if projected {
			require.Equal(t, 1, observed.handled, "the actual optional helper must accept the collision set")
			require.NotNil(t, observed.hydrated[chosen.ID])
			require.Equal(t, chosen.StartLine, observed.hydrated[chosen.ID].StartLine)
			require.Equal(t, chosen.Meta, observed.hydrated[chosen.ID].Meta)
			require.Equal(t, "Second{}", contracts.MakeBodyFacts(parsed, observed.hydrated[chosen.ID]).ResponseCalls()[0].ValueExpr)
			require.Equal(t, want, reg.All(), "full hydrated body facts and registration attribution must equal the original lookup")
		} else {
			want = reg.All()
		}
	}
}

func TestContractHandlerDeclarationsPreserveImportSideEffects(t *testing.T) {
	for _, kind := range []string{"config", "spring", "java"} {
		for _, seeded := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "_unseeded", true: "_seeded"}[seeded], func(t *testing.T) {
				core := graph.New()
				handler := &graph.Node{ID: "a/f.go::Handle", Name: "Handle", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go"}
				side := &graph.Node{ID: "a/side.go::Handle", Name: "Handle", RepoPrefix: "a", FilePath: "a/side.go"}
				switch kind {
				case "config":
					side.Kind = graph.KindConfigKey
				case "spring":
					side.Kind = graph.KindField
					side.Meta = map[string]any{"spring_config_keys": []string{"server.port"}}
				case "java":
					side.Kind = graph.KindMethod
					side.Language = "java"
				}
				core.AddBatch([]*graph.Node{handler, side}, nil)
				optimized := completedNameEvidence(t, core)
				reference := completedNameEvidence(t, core)
				if seeded {
					optimized.AddBatch([]*graph.Node{side}, nil)
					reference.AddBatch([]*graph.Node{side}, nil)
					optimized.handlerSeededFiles = map[string]bool{side.FilePath: true}
					optimized.files = map[string]ContractFollowupFile{side.FilePath: {RepoPrefix: "a", Path: side.FilePath}}
				}
				_, handled := optimized.contractHandlerDeclarations([]string{"Handle"})
				require.NoError(t, optimized.err)
				if kind == "config" || !seeded {
					require.False(t, handled, "full imports with observable side effects require the original path")
					optimized.FindNodesByNames([]string{"Handle"})
				} else {
					require.True(t, handled)
				}
				reference.FindNodesByNames([]string{"Handle"})
				require.NoError(t, optimized.err)
				require.NoError(t, reference.err)
				require.Equal(t, reference.outputIDs, optimized.outputIDs)
				require.Equal(t, reference.springIDs, optimized.springIDs)
				require.Equal(t, reference.javaMethodIDs, optimized.javaMethodIDs)
				if seeded && kind != "config" {
					// A path-only seed mark cannot stand in for an admitted owner proof.
					optimized.files[side.FilePath] = ContractFollowupFile{RepoPrefix: "wrong", Path: side.FilePath}
					_, ok := optimized.contractHandlerDeclarations([]string{"Handle"})
					require.False(t, ok)
				}
				switch kind {
				case "config":
					require.True(t, optimized.outputIDs[side.ID])
				case "spring":
					require.True(t, optimized.springIDs[side.ID])
				case "java":
					require.True(t, optimized.javaMethodIDs[side.ID])
				}
			})
		}
	}
}

type handlerObservedCapturedCore struct {
	*contractCapturedCore
	handlerDeclarations *int
}

func (r *handlerObservedCapturedCore) VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	for _, name := range names {
		if name == "handler" {
			(*r.handlerDeclarations)++
		}
	}
	return r.contractCapturedCore.VisitNodeDeclarationsByNamesContext(ctx, names, yield)
}

func TestContractHandlerDeclarationJobUsesPrivateDoorAndEqualsCold(t *testing.T) {
	routes := `package fixture
import "net/http"
func routes(mux *http.ServeMux) { mux.HandleFunc("GET /route",handler) }
`
	handler := `package fixture
import "net/http"
type Response struct { Value string }
func handler(w http.ResponseWriter,r *http.Request) {
 value:=Response{}
 WriteJSON(w,200,value)
}
func WriteJSON(w http.ResponseWriter,status int,value any){}
func shadowParameter(handler string) {}
func shadowLocal() { handler:=1; _=handler }
`
	files := map[string]string{"routes.go": routes, "handler.go": handler}
	f := newContractJobFixture(t, files)
	declarations := 0
	f.adjust = func(snap *ContractFollowupSnapshot) {
		selected, ok := snap.Core.(*contractCapturedCore)
		require.True(t, ok, "actual captured repository scope must remain intact")
		snap.Core = &handlerObservedCapturedCore{contractCapturedCore: selected, handlerDeclarations: &declarations}
	}
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.Positive(t, declarations, "real cross-file enrichment must use declaration discovery/replay, not just the generic full door")
	require.NotEmpty(t, cache.units["fixture/handler.go"].providerSchemas, "selected full body-facts kernel must still run")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	handler = strings.Replace(handler, "Value string", "Value string; Other int", 1)
	files["handler.go"] = handler
	f.save("handler.go", handler)
	routes = strings.Replace(routes, "GET /route", "GET /changed", 1)
	files["routes.go"] = routes
	f.save("routes.go", routes)
	before := declarations
	runCachedContractFixture(t, f, &cache)
	require.Greater(t, declarations, before)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

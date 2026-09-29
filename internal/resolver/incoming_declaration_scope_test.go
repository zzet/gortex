package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// declarationFixture is a changed file that declares Target, a caller in
// another file whose reference to Target is parked on the name stub, and a
// store the caller's reference can bind in (the same package, so it resolves
// whenever it is attempted).
//
// The parked reference is attempted-and-still-pending by construction only in
// the tests' story: the fixture deliberately leaves it resolvable, so a pass
// that re-attempts it binds it and a pass that skips it leaves it parked. That
// is how the tests observe which references the incoming leg admitted.
func declarationFixture(t *testing.T, targetMeta map[string]any) (*graph.Graph, *graph.Edge) {
	t.Helper()
	g := graph.New()
	for _, node := range []*graph.Node{
		{ID: "pkg/changed.go", Kind: graph.KindFile, Name: "changed.go", FilePath: "pkg/changed.go", Language: "go"},
		{ID: "pkg/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "pkg/caller.go", Language: "go"},
		{ID: "pkg/changed.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "pkg/changed.go",
			Language: "go", StartLine: 10, EndLine: 12, Meta: targetMeta},
		{ID: "pkg/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/caller.go", Language: "go"},
	} {
		g.AddNode(node)
	}
	parked := &graph.Edge{From: "pkg/caller.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
		FilePath: "pkg/caller.go", Line: 3}
	g.AddEdge(parked)
	return g, parked
}

// scopedResolver is a resolver configured for evidence scoping, isolated from
// an ambient EvidenceScopeEnv.
func scopedResolver(t *testing.T, g graph.Store) *Resolver {
	t.Helper()
	t.Setenv(EvidenceScopeEnv, "")
	r := New(g)
	r.SetEvidenceScoping(true)
	return r
}

func priorOf(nodes ...*graph.Node) map[string]DeclarationSurface {
	return map[string]DeclarationSurface{"pkg/changed.go": DeclarationSurfaceOf(nodes)}
}

func TestDeclarationSurfaceIgnoresPositionsAndBodyMetrics(t *testing.T) {
	before := &graph.Node{ID: "pkg/a.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "pkg/a.go",
		Language: "go", StartLine: 10, EndLine: 20, Meta: map[string]any{"signature": "func F()", "complexity": 1}}
	body := *before
	body.StartLine, body.EndLine = 11, 25
	body.Meta = map[string]any{"signature": "func F()", "complexity": 4}
	assert.Equal(t, DeclarationSurfaceOf([]*graph.Node{before}), DeclarationSurfaceOf([]*graph.Node{&body}),
		"a body edit moves positions and body metrics, never the declaration surface")

	signature := *before
	signature.Meta = map[string]any{"signature": "func F(x int)", "complexity": 1}
	assert.NotEqual(t, DeclarationSurfaceOf([]*graph.Node{before}), DeclarationSurfaceOf([]*graph.Node{&signature}))

	local := &graph.Node{ID: "pkg/a.go::F::x", Kind: graph.KindLocal, Name: "x", FilePath: "pkg/a.go"}
	assert.Equal(t, DeclarationSurfaceOf([]*graph.Node{before}), DeclarationSurfaceOf([]*graph.Node{before, local}),
		"locals are not referenceable declarations")

	added := &graph.Node{ID: "pkg/a.go::G", Kind: graph.KindFunction, Name: "G", FilePath: "pkg/a.go", Language: "go"}
	grown := DeclarationSurfaceOf([]*graph.Node{before, added})
	assert.Contains(t, grown, "unresolved::G")
	assert.NotContains(t, DeclarationSurfaceOf([]*graph.Node{before}), "unresolved::G")
}

func TestIncomingLegSkipsParkedReferencesOnUnchangedDeclarations(t *testing.T) {
	for _, entry := range []struct {
		name string
		run  func(*Resolver) *ResolveStats
	}{
		{"batch", func(r *Resolver) *ResolveStats { return r.ResolveFilesAndIncoming([]string{"pkg/changed.go"}) }},
		{"single", func(r *Resolver) *ResolveStats { return r.ResolveFileAndIncoming("pkg/changed.go") }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			g, parked := declarationFixture(t, map[string]any{"signature": "func Target()"})
			prior := priorOf(g.GetFileNodes("pkg/changed.go")...)
			core, logs := observer.New(zap.InfoLevel)
			r := scopedResolver(t, g)
			r.SetLogger(zap.New(core))
			r.SetPriorDeclarations(prior)
			stats := entry.run(r)
			r.SetPriorDeclarations(nil)

			assert.Equal(t, "unresolved::Target", parked.To,
				"a parked reference on an unchanged declaration is not re-attempted")
			assert.Zero(t, stats.Resolved)
			assert.Empty(t, logs.FilterMessage("resolver: prepare page indexes").All(),
				"no admitted work means no page indexes")
			if entry.name == "batch" {
				summaries := logs.FilterMessage("resolver: incremental files phases").All()
				require.Len(t, summaries, 1)
				fields := summaries[0].ContextMap()
				assert.Equal(t, "no_pending", fields["outcome"])
				assert.EqualValues(t, 1, fields["carried_skipped"])
				assert.EqualValues(t, 4, fields["carried_keys"])
			}
		})
	}
}

func TestIncomingLegAdmitsParkedReferencesOnChangedDeclarations(t *testing.T) {
	g, parked := declarationFixture(t, map[string]any{"signature": "func Target(x int)"})
	before := &graph.Node{ID: "pkg/changed.go::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: "pkg/changed.go", Language: "go", Meta: map[string]any{"signature": "func Target()"}}
	r := scopedResolver(t, g)
	r.SetPriorDeclarations(priorOf(before))
	stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
	r.SetPriorDeclarations(nil)
	assert.Equal(t, "pkg/changed.go::Target", parked.To, "a changed declaration re-attempts its parked references")
	assert.Equal(t, 1, stats.Resolved)
}

func TestIncomingLegAdmitsParkedReferencesOnAddedDeclarations(t *testing.T) {
	g, parked := declarationFixture(t, nil)
	r := scopedResolver(t, g)
	r.SetPriorDeclarations(priorOf()) // the file declared nothing before
	stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
	r.SetPriorDeclarations(nil)
	assert.Equal(t, "pkg/changed.go::Target", parked.To)
	assert.Equal(t, 1, stats.Resolved)
}

func TestIncomingLegAdmitsRestubbedReferencesOnUnchangedDeclarations(t *testing.T) {
	g, parked := declarationFixture(t, nil)
	prior := priorOf(g.GetFileNodes("pkg/changed.go")...)
	// The re-parse restubbed a reference that was bound before the edit.
	parked.To = "pkg/changed.go::Target"
	graph.StashRestubProvenance(parked)
	parked.To = "unresolved::Target"
	g.AddEdge(&graph.Edge{From: "pkg/caller.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
		FilePath: "pkg/caller.go", Line: 9})

	r := scopedResolver(t, g)
	r.SetPriorDeclarations(prior)
	stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
	r.SetPriorDeclarations(nil)
	assert.Equal(t, "pkg/changed.go::Target", parked.To, "a restubbed reference is always rebound")
	assert.Equal(t, 1, stats.Resolved, "only the restubbed reference is re-attempted")
}

func TestIncomingLegWithoutEvidenceOrWithTheSwitchOffStaysExhaustive(t *testing.T) {
	t.Run("no evidence", func(t *testing.T) {
		g, parked := declarationFixture(t, nil)
		stats := New(g).ResolveFilesAndIncoming([]string{"pkg/changed.go"})
		assert.Equal(t, "pkg/changed.go::Target", parked.To)
		assert.Equal(t, 1, stats.Resolved)
	})
	t.Run("evidence for another file only", func(t *testing.T) {
		g, parked := declarationFixture(t, nil)
		r := New(g)
		r.SetPriorDeclarations(map[string]DeclarationSurface{"pkg/other.go": {}})
		stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
		assert.Equal(t, "pkg/changed.go::Target", parked.To)
		assert.Equal(t, 1, stats.Resolved)
	})
	t.Run("configured off", func(t *testing.T) {
		t.Setenv(EvidenceScopeEnv, "")
		g, parked := declarationFixture(t, nil)
		r := New(g)
		r.SetEvidenceScoping(false)
		r.SetPriorDeclarations(priorOf(g.GetFileNodes("pkg/changed.go")...))
		stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
		assert.Equal(t, "pkg/changed.go::Target", parked.To)
		assert.Equal(t, 1, stats.Resolved)
	})
	t.Run("environment off overrides the configuration", func(t *testing.T) {
		g, parked := declarationFixture(t, nil)
		r := scopedResolver(t, g)
		t.Setenv(EvidenceScopeEnv, "off")
		r.SetPriorDeclarations(priorOf(g.GetFileNodes("pkg/changed.go")...))
		stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
		assert.Equal(t, "pkg/changed.go::Target", parked.To)
		assert.Equal(t, 1, stats.Resolved)
	})
}

// Evidence scoping is on unless the configuration turns it off, and the
// environment overrides the configuration in both directions.
func TestEvidenceScopingIsOnByDefault(t *testing.T) {
	for _, tc := range []struct {
		env        string
		configured bool
		want       bool
	}{
		{"", false, false},
		{"", true, true},
		{"on", false, true},
		{" ON ", false, true},
		{"off", true, false},
		{"garbage", false, false},
		{"garbage", true, true},
	} {
		t.Setenv(EvidenceScopeEnv, tc.env)
		assert.Equal(t, tc.want, EvidenceScopingEnabled(tc.configured), "env=%q configured=%v", tc.env, tc.configured)
		r := New(graph.New())
		r.SetEvidenceScoping(tc.configured)
		assert.Equal(t, tc.want, r.EvidenceScoping(), "resolver env=%q configured=%v", tc.env, tc.configured)
	}

	// A resolver nobody configured scopes.
	t.Setenv(EvidenceScopeEnv, "")
	assert.True(t, New(graph.New()).EvidenceScoping(), "an unconfigured resolver must scope")

	// The master resolver inherits the repository resolver's choice with its
	// evidence.
	from := New(graph.New())
	from.SetEvidenceScoping(false)
	master := New(graph.New())
	master.InheritIncrementalEvidence(from)
	assert.False(t, master.EvidenceScoping())

	// An unconfigured resolver engages the carry.
	g, parked := declarationFixture(t, nil)
	r := New(g)
	r.SetPriorDeclarations(priorOf(g.GetFileNodes("pkg/changed.go")...))
	stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
	assert.Equal(t, "unresolved::Target", parked.To, "the default carries the unchanged declaration's key")
	assert.Equal(t, 0, stats.Resolved)
}

// A batch key is carried only when every frontier file owning it has evidence
// and an unchanged declaration: one changed owner admits the whole bucket.
func TestIncomingCarryRequiresEveryOwnerUnchanged(t *testing.T) {
	a := &graph.Node{ID: "pkg/a.go::Shared", Kind: graph.KindFunction, Name: "Shared", FilePath: "pkg/a.go"}
	b := &graph.Node{ID: "pkg/b.go::Shared", Kind: graph.KindFunction, Name: "Shared", FilePath: "pkg/b.go"}
	bChanged := &graph.Node{ID: "pkg/b.go::Shared", Kind: graph.KindFunction, Name: "Shared", FilePath: "pkg/b.go",
		Meta: map[string]any{"signature": "func Shared(int)"}}
	r := scopedResolver(t, graph.New())
	r.SetPriorDeclarations(map[string]DeclarationSurface{
		"pkg/a.go": DeclarationSurfaceOf([]*graph.Node{a}),
		"pkg/b.go": DeclarationSurfaceOf([]*graph.Node{b}),
	})
	carry := r.incomingCarryFor([]string{"pkg/a.go", "pkg/b.go"}, map[string][]*graph.Node{
		"pkg/a.go": {a}, "pkg/b.go": {bChanged},
	})
	assert.Nil(t, carry, "a changed owner refuses the shared key")

	carry = r.incomingCarryFor([]string{"pkg/a.go", "pkg/b.go"}, map[string][]*graph.Node{
		"pkg/a.go": {a}, "pkg/b.go": {b},
	})
	require.NotNil(t, carry)
	assert.True(t, carry.carried("unresolved::Shared"))
}

// repoPrefixCountingStore counts the whole-generation repository listing.
type repoPrefixCountingStore struct {
	*graph.Graph
	prefixes []string
	listings int
}

func (s *repoPrefixCountingStore) RepoPrefixes() []string {
	s.listings++
	return s.prefixes
}

// The repository listing validates ID-derived guesses only; a page whose
// sources all hydrate with their repository never reads it, and a page with an
// unhydrated source still rejects a guess that names no repository.
func TestPendingRepoPrefixesListsRepositoriesOnlyForUnhydratedSources(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a/pkg/x.go::X", Kind: graph.KindFunction, Name: "X", FilePath: "a/pkg/x.go", RepoPrefix: "a"})
	store := &repoPrefixCountingStore{Graph: g, prefixes: []string{"a"}}
	r := New(store)

	hydrated := []*graph.Edge{{From: "a/pkg/x.go::X", To: "unresolved::Y", Kind: graph.EdgeCalls}}
	prefixes, _ := pendingRepoPrefixes(r, hydrated)
	assert.Equal(t, []string{"a"}, prefixes)
	assert.Zero(t, store.listings, "hydrated sources must not pay the repository listing")

	unhydrated := []*graph.Edge{
		{From: "a/pkg/x.go::X", To: "unresolved::Y", Kind: graph.EdgeCalls},
		{From: "internal/gone.go::Z", To: "unresolved::Y", Kind: graph.EdgeCalls},
		{From: "a/pkg/gone.go::W", To: "unresolved::Y", Kind: graph.EdgeCalls},
	}
	prefixes, _ = pendingRepoPrefixes(r, unhydrated)
	assert.Equal(t, []string{"a"}, prefixes, "a guess naming no repository is dropped, a known one kept")
	assert.Equal(t, 1, store.listings, "the listing is read once per page")

	// A guess another hydrated source of the page already confirms (a
	// dataflow placeholder source under a known repository) needs no listing.
	confirmedByPage := []*graph.Edge{
		{From: "a/pkg/x.go::X", To: "unresolved::Y", Kind: graph.EdgeCalls},
		{From: "a/unresolved::*.Command", To: "unresolved::Y", Kind: graph.EdgeArgOf},
	}
	prefixes, _ = pendingRepoPrefixes(r, confirmedByPage)
	assert.Equal(t, []string{"a"}, prefixes)
	assert.Equal(t, 1, store.listings, "a guess a hydrated source confirms must not pay the listing")
}

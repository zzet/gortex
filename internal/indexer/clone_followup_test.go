package indexer

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

const cloneFollowupOtherBody = `	product := 1
	for i, x := range xs {
		if x < 0 {
			product -= x * (i + 7)
		} else {
			product += x + i + 11
		}
		if product > 10000 {
			product = product / 3
		}
	}
	return product
`

func cloneFollowupTree(body string) map[string]string {
	tree := kindParityTree()
	old := "func CloneB(xs []int) int {\n" + kindParityCloneBody + "}\n\n"
	if !strings.Contains(tree["prod/producers.go"], old) {
		panic("kind-parity CloneB fixture changed")
	}
	tree["prod/producers.go"] = strings.Replace(tree["prod/producers.go"], old, "", 1)
	tree["prod/clone_b.go"] = cloneFollowupBSource(body)
	tree["extra/stable.go"] = "package extra\n\nfunc StableA(xs []int) int {\n" + cloneFollowupOtherBody +
		"}\n\nfunc StableB(xs []int) int {\n" + cloneFollowupOtherBody + "}\n"
	return tree
}

func cloneFollowupBSource(body string) string {
	return "package prod\n\nfunc CloneB(xs []int) int {\n" + body + "}\n"
}

func cloneFollowupPairEdges(reader graph.Reader, a, b string) []string {
	var got []string
	for edge := range reader.EdgesByKind(graph.EdgeSimilarTo) {
		if edge == nil || (edge.From != a && edge.From != b) || (edge.To != a && edge.To != b) {
			continue
		}
		got = append(got, fmt.Sprintf("%s>%s@%s:%d", edge.From, edge.To, edge.FilePath, edge.Line))
	}
	sort.Strings(got)
	return got
}

func cloneFollowupAllRelations(reader graph.Reader) []string {
	var out []string
	for _, kind := range []graph.EdgeKind{graph.EdgeSimilarTo, graph.EdgeSemanticallyRelated} {
		for edge := range reader.EdgesByKind(kind) {
			if cloneFollowupDerivedEdge(edge) {
				out = append(out, fmt.Sprintf("%s|%s>%s@%s:%d|%g", kind, edge.From, edge.To,
					edge.FilePath, edge.Line, edge.Confidence))
			}
		}
	}
	sort.Strings(out)
	return out
}

func cloneFollowupCorpusRow(t *testing.T, store *store_sqlite.Store, id string) graph.CloneCorpusRow {
	t.Helper()
	after := ""
	for {
		page, err := store.CloneCorpusPage(builderRepoPrefix, after, cloneCorpusFinalizeBatch)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page {
			if row.NodeID == id {
				return row
			}
		}
		if len(page) < cloneCorpusFinalizeBatch {
			break
		}
		after = page[len(page)-1].NodeID
	}
	t.Fatalf("clone corpus row %s absent", id)
	return graph.CloneCorpusRow{}
}

// The follow-up must compare a changed body with a clone partner that lives
// outside its file mask. Creation needs both directed edges; removal must
// hide the old partner-to-changed reverse edge as well.
func TestEnrichmentFollowupCloneCrossFilePairs(t *testing.T) {
	for _, tc := range []struct {
		name, before, after    string
		beforeEdges, wantEdges int
	}{
		{name: "create", before: cloneFollowupOtherBody, after: kindParityCloneBody, beforeEdges: 0, wantEdges: 2},
		{name: "remove", before: kindParityCloneBody, after: cloneFollowupOtherBody, beforeEdges: 2, wantEdges: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
			f, c, mgr := semanticChainFixtureProduct(t, cloneFollowupTree(tc.before))
			c.followup.quiet = -1
			coordinatorReconcile(t, c)
			ctx := context.Background()
			a := builderGraphPath(builderRepoPrefix, "prod/producers.go") + "::CloneA"
			b := builderGraphPath(builderRepoPrefix, "prod/clone_b.go") + "::CloneB"
			stableA := builderGraphPath(builderRepoPrefix, "extra/stable.go") + "::StableA"
			stableB := builderGraphPath(builderRepoPrefix, "extra/stable.go") + "::StableB"
			initial := chainMaterialize(t, f)
			if got := cloneFollowupPairEdges(initial.Reader, a, b); len(got) != tc.beforeEdges {
				initial.Close()
				t.Fatalf("initial directed clone edges = %v, want %d", got, tc.beforeEdges)
			}
			if got := cloneFollowupPairEdges(initial.Reader, stableA, stableB); len(got) != 2 {
				initial.Close()
				t.Fatalf("stable clone fixture has %d directed edges, want 2: %v", len(got), got)
			}
			initial.Close()
			builderWriteFile(t, f.worktree, "prod/clone_b.go", cloneFollowupBSource(tc.after))
			coordinatorReconcile(t, c)
			debt, _, err := c.PendingDerived(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !containsString(debt[graphview.CapSimilarity].Paths, builderGraphPath(builderRepoPrefix, "prod/clone_b.go")) {
				t.Fatalf("changed clone file has no similarity debt: %+v", debt)
			}
			if outcome := c.runEnrichmentFollowup(ctx); outcome != followupLanded {
				t.Fatalf("follow-up: %s", outcome)
			}

			view := chainMaterialize(t, f)
			defer view.Close()
			clean := builderOpenStore(t, "clone-followup-cross-file-"+tc.name)
			builderIndexSemantic(mgr)(t, clean, f.worktree)
			want := cloneFollowupPairEdges(clean, a, b)
			got := cloneFollowupPairEdges(view.Reader, a, b)
			if len(want) != tc.wantEdges || len(got) != len(want) {
				t.Fatalf("directed clone edges: clean=%v composed=%v; want %d", want, got, tc.wantEdges)
			}
			for _, edge := range want {
				if !containsString(got, edge) {
					t.Errorf("composed clone edges %v omit clean edge %s", got, edge)
				}
			}
			for _, edge := range got {
				if !containsString(want, edge) {
					t.Errorf("composed clone edges %v retain stale edge %s", got, edge)
				}
			}
			if actual, expected := cloneFollowupAllRelations(view.Reader), cloneFollowupAllRelations(clean); !reflect.DeepEqual(actual, expected) {
				t.Errorf("clone-derived relations: composed=%v clean=%v", actual, expected)
			}
			for _, id := range []string{a, b} {
				actual, expected := view.Reader.GetNode(id), clean.GetNode(id)
				if actual == nil || expected == nil {
					t.Fatalf("clone endpoint %s missing: composed=%v clean=%v", id, actual, expected)
				}
				if actual.Meta[cloneSigMetaKey] != expected.Meta[cloneSigMetaKey] {
					t.Errorf("%s clone_sig: composed=%v clean=%v", id, actual.Meta[cloneSigMetaKey], expected.Meta[cloneSigMetaKey])
				}
			}
			followup := f.store.AtGeneration(f.route().DirtyGenerationID)
			repoNodes := followup.RepoMemoryEstimate(builderRepoPrefix).NodeCount
			if state, found, err := followup.GetRepoIndexState(builderRepoPrefix); err != nil {
				t.Fatal(err)
			} else if !found || state.NodeCount != repoNodes {
				t.Errorf("follow-up repo node count: index state=%d found=%t repo=%d generation=%d",
					state.NodeCount, found, repoNodes, followup.NodeCount())
			}
			fromFollowup := cloneFollowupCorpusRow(t, followup, b)
			fromClean := cloneFollowupCorpusRow(t, clean, b)
			if !fromFollowup.Finalized || fromFollowup.Signature != fromClean.Signature || fromFollowup.TokenCount != fromClean.TokenCount {
				t.Errorf("CloneB corpus row: follow-up=%+v clean=%+v", fromFollowup, fromClean)
			}
			masks, err := followup.FileMasksContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, mask := range masks {
				if mask.FilePath == builderGraphPath(builderRepoPrefix, "prod/producers.go") ||
					mask.FilePath == builderGraphPath(builderRepoPrefix, "prod/sib_e.go") ||
					mask.FilePath == builderGraphPath(builderRepoPrefix, "extra/stable.go") {
					t.Errorf("follow-up claimed untouched file %s", mask.FilePath)
				}
			}
			states, err := followup.ProducerStates()
			if err != nil {
				t.Fatal(err)
			}
			complete := false
			for _, row := range states {
				if row.Producer == string(graphview.CapSimilarity) && row.State == store_sqlite.ProducerStateComplete {
					complete = true
				}
			}
			if !complete {
				t.Errorf("follow-up did not declare graph.similarity complete: %+v", states)
			}
			if after, _, err := c.PendingDerived(ctx); err != nil || len(after[graphview.CapSimilarity].Paths) > 0 {
				t.Errorf("similarity debt after successful follow-up = %+v, %v", after, err)
			}
		})
	}
}

// A comment-only edit re-derives an unchanged partner into the first dirty
// generation. Its compact sidecar must retain the exact token count so a
// later follow-up can use it without guessing from unique shingles.
func TestEnrichmentFollowupCloneCarriedPartnerCorpus(t *testing.T) {
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	tree := cloneFollowupTree(cloneFollowupOtherBody)
	f, c, mgr := semanticChainFixtureProduct(t, tree)
	c.followup.quiet = -1
	coordinatorReconcile(t, c)
	ctx := context.Background()
	a := builderGraphPath(builderRepoPrefix, "prod/producers.go") + "::CloneA"
	b := builderGraphPath(builderRepoPrefix, "prod/clone_b.go") + "::CloneB"
	builderWriteFile(t, f.worktree, "prod/producers.go", "// unchanged clone body\n"+tree["prod/producers.go"])
	coordinatorReconcile(t, c)
	first := f.store.AtGeneration(f.route().DirtyGenerationID)
	if row := cloneFollowupCorpusRow(t, first, a); row.TokenCount <= 0 {
		t.Fatalf("carried clone partner lost normalized token count: %+v", row)
	}
	if outcome := c.runEnrichmentFollowup(ctx); outcome != followupLanded {
		t.Fatalf("settle comment edit's semantic debt: %s", outcome)
	}
	// The first edit also owes non-clone semantic work. Settle that debt so
	// the second follow-up has no legitimate reason to mask producers.go.
	if debt, _, err := c.PendingDerived(ctx); err != nil {
		t.Fatal(err)
	} else {
		for capability, pending := range debt {
			if containsString(pending.Paths, builderGraphPath(builderRepoPrefix, "prod/producers.go")) {
				t.Fatalf("first follow-up left %s debt on carried partner: %+v", capability, debt)
			}
		}
	}
	prior := f.store.AtGeneration(f.route().DirtyGenerationID)
	if row := cloneFollowupCorpusRow(t, prior, a); row.TokenCount <= 0 {
		t.Fatalf("first follow-up lost carried partner token count: %+v", row)
	}
	builderWriteFile(t, f.worktree, "prod/clone_b.go", cloneFollowupBSource(kindParityCloneBody))
	coordinatorReconcile(t, c)
	if outcome := c.runEnrichmentFollowup(ctx); outcome != followupLanded {
		t.Fatalf("follow-up over carried partner: %s", outcome)
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	clean := builderOpenStore(t, "clone-followup-carried-partner")
	builderIndexSemantic(mgr)(t, clean, f.worktree)
	if got, want := cloneFollowupAllRelations(view.Reader), cloneFollowupAllRelations(clean); !reflect.DeepEqual(got, want) {
		t.Errorf("clone relations after carried partner: composed=%v clean=%v", got, want)
	}
	if got, want := cloneFollowupPairEdges(view.Reader, a, b), cloneFollowupPairEdges(clean, a, b); len(want) != 2 || !reflect.DeepEqual(got, want) {
		t.Errorf("cross-layer directed pair: composed=%v clean=%v", got, want)
	}
	for _, id := range []string{a, b} {
		got, want := view.Reader.GetNode(id), clean.GetNode(id)
		if got == nil || want == nil || got.Meta[cloneSigMetaKey] != want.Meta[cloneSigMetaKey] {
			t.Errorf("cross-layer clone signature %s: composed=%v clean=%v", id, got, want)
		}
	}
	followup := f.store.AtGeneration(f.route().DirtyGenerationID)
	masks, err := followup.FileMasksContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range masks {
		if mask.FilePath == builderGraphPath(builderRepoPrefix, "prod/producers.go") {
			t.Errorf("follow-up claimed carried partner file %s", mask.FilePath)
		}
	}
}

// A shingled, visible partner with no authoritative corpus row cannot yield
// a complete similarity declaration. The failed follow-up leaves its debt and
// the routed generation unchanged.
func TestEnrichmentFollowupMissingCloneCorpusKeepsDebt(t *testing.T) {
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	f, c, _ := semanticChainFixtureProduct(t, cloneFollowupTree(cloneFollowupOtherBody))
	c.followup.quiet = -1
	coordinatorReconcile(t, c)
	ctx := context.Background()
	builderWriteFile(t, f.worktree, "prod/clone_b.go", cloneFollowupBSource(kindParityCloneBody))
	coordinatorReconcile(t, c)
	before := f.route().DirtyGenerationID
	a := builderGraphPath(builderRepoPrefix, "prod/producers.go") + "::CloneA"
	_ = cloneFollowupCorpusRow(t, f.store, a)
	if err := f.store.DeleteCloneShingles([]string{a}); err != nil {
		t.Fatal(err)
	}
	if outcome := c.runEnrichmentFollowup(ctx); outcome != followupFailed {
		t.Fatalf("missing partner corpus row: follow-up = %s, want failed", outcome)
	}
	if after := f.route().DirtyGenerationID; after != before {
		t.Fatalf("failed follow-up moved route: %d -> %d", before, after)
	}
	debt, _, err := c.PendingDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(debt[graphview.CapSimilarity].Paths, builderGraphPath(builderRepoPrefix, "prod/clone_b.go")) {
		t.Fatalf("failed follow-up cleared similarity debt: %+v", debt)
	}
}

func TestEnrichmentFollowupBodylessCloneCorpusKeepsDebt(t *testing.T) {
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "on")
	f, c, _ := semanticChainFixtureProduct(t, cloneFollowupTree(cloneFollowupOtherBody))
	c.followup.quiet = -1
	coordinatorReconcile(t, c)
	ctx := context.Background()
	a := builderGraphPath(builderRepoPrefix, "prod/producers.go") + "::CloneA"
	n := f.store.GetNode(a)
	if n == nil {
		t.Fatal("missing CloneA fixture node")
	}
	_ = cloneFollowupCorpusRow(t, f.store, a)
	corrupt := *n
	corrupt.Meta = make(map[string]any, len(n.Meta))
	for key, value := range n.Meta {
		if key != cloneBodyMetaKey {
			corrupt.Meta[key] = value
		}
	}
	if err := f.store.AddBatchChecked([]*graph.Node{&corrupt}, nil); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "prod/clone_b.go", cloneFollowupBSource(kindParityCloneBody))
	coordinatorReconcile(t, c)
	before := f.route().DirtyGenerationID
	if outcome := c.runEnrichmentFollowup(ctx); outcome != followupFailed {
		t.Fatalf("bodyless partner with corpus: follow-up = %s, want failed", outcome)
	}
	if after := f.route().DirtyGenerationID; after != before {
		t.Fatalf("failed follow-up moved route: %d -> %d", before, after)
	}
	debt, _, err := c.PendingDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(debt[graphview.CapSimilarity].Paths, builderGraphPath(builderRepoPrefix, "prod/clone_b.go")) {
		t.Fatalf("failed follow-up cleared similarity debt: %+v", debt)
	}
}

// An untouched partner whose corpus-wide signature changed is projected as
// one identity override, not a replacement of its entire file or adjacency.
func TestCloneFollowupProjectionDetachedPartner(t *testing.T) {
	ctx := context.Background()
	store := builderOpenStore(t, "clone-followup-detached-partner")
	id := builderGraphPath(builderRepoPrefix, "prod/partner.go") + "::Partner"
	path := builderGraphPath(builderRepoPrefix, "prod/partner.go")
	parent := &graph.Node{
		ID: id, Kind: graph.KindFunction, RepoPrefix: builderRepoPrefix,
		FilePath: path, Name: "Partner", StartLine: 4,
		Meta: map[string]any{cloneSigMetaKey: "old", "unrelated": "keep"},
	}
	if err := store.AddBatchChecked([]*graph.Node{parent}, nil); err != nil {
		t.Fatal(err)
	}
	handle := builderContextDerivedHandle(t, store)
	projection := &cloneFollowupProjection{
		repo: builderRepoPrefix,
		rows: []cloneFollowupSignature{{
			filePath: path,
			row: graph.CloneCorpusRow{
				NodeID: id, RepoPrefix: builderRepoPrefix, Shingles: []uint64{1, 2, 3},
				Signature: "new", TokenCount: 61, Finalized: true,
			},
		}},
	}
	if detached, err := projection.apply(ctx, handle, store); err != nil || detached != 1 {
		t.Fatalf("detached partner projection = %d, %v; want 1", detached, err)
	}
	if handle.NodeCount() != 1 {
		t.Fatalf("projection wrote %d generation nodes, want 1", handle.NodeCount())
	}
	got := handle.GetNode(id)
	if got == nil || got.Meta[cloneSigMetaKey] != "new" || got.Meta["unrelated"] != "keep" {
		t.Fatalf("detached partner node = %+v", got)
	}
	if original := store.GetNode(id); original == nil || original.Meta[cloneSigMetaKey] != "old" || original.Meta["unrelated"] != "keep" {
		t.Fatalf("projection mutated parent node = %+v", original)
	}
	if row := cloneFollowupCorpusRow(t, handle, id); !row.Finalized || row.Signature != "new" || row.TokenCount != 61 {
		t.Errorf("detached partner corpus row = %+v", row)
	}
	if masks, err := handle.FileMasksContext(ctx); err != nil || len(masks) != 0 {
		t.Errorf("identity-only projection acquired file masks: %v, %v", masks, err)
	}
	if claims, err := handle.NodeIdentityMasksContext(ctx); err != nil || len(claims) != 1 ||
		claims[0].NodeID != id || claims[0].Kind != store_sqlite.NodeIdentityMaskReplace {
		t.Errorf("identity-only projection claims = %+v, %v", claims, err)
	}
}

func TestCloneFollowupRequiresNamedRepo(t *testing.T) {
	if _, err := prepareCloneFollowup(context.Background(), nil, nil, nil, "", 0.82); err == nil ||
		!strings.Contains(err.Error(), "non-empty repository prefix") {
		t.Fatalf("empty-prefix clone follow-up = %v, want a fail-closed error", err)
	}
}

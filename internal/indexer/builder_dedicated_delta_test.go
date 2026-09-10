package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
)

type dedicatedDeltaFixture struct {
	builder      *SparseGenerationBuilder
	request      dedicatedBuilderFixtureRequest
	baseClaim    store_sqlite.DedicatedBaseBuildClaim
	base         LayerBase
	materializer *graphview.Materializer
	git          func(...string) string
}

func newDedicatedDeltaFixture(t testing.TB) dedicatedDeltaFixture {
	t.Helper()
	ctx := context.Background()
	builder, request, git := privateDedicatedBuilderFixture(t)
	for name, content := range map[string]string{
		"dependent.go": "package dedicated\nfunc Dependent() string { return Committed() }\n",
		"stable.go":    "package dedicated\nfunc Unrelated() int { return 7 }\n",
		"delete.go":    "package dedicated\nfunc Removed() bool { return true }\n",
		"rename.go":    "package dedicated\nfunc Renamed() bool { return true }\n",
	} {
		if err := os.WriteFile(filepath.Join(request.RootPath, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "user.name=Delta Test", "-c", "user.email=delta@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "full lower fixture")
	request.Identity.TreeOID, request.Identity.ProvenanceCommitOID = git("rev-parse", "HEAD^{tree}"), git("rev-parse", "HEAD")
	catalog := builder.Store.Catalog()
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: request.Identity.GraphID, Owner: store_sqlite.DedicatedBaseOwner{CheckoutID: request.Identity.CheckoutID, Incarnation: "private-incarnation"}, Token: "delta-fixture-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: authority,
		Identity: store_sqlite.DedicatedBaseIdentity{TreeOID: request.Identity.TreeOID, ConfigHash: request.Identity.ConfigHash,
			ExtractorVersions: request.Identity.ExtractorVersions, ResolverVersion: request.Identity.ResolverVersion}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, AttemptToken: "delta-fixture-full", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatal(err)
	}
	materializer := &graphview.Materializer{Store: builder.Store, Catalog: catalog, Leases: graphview.NewLeaseManager(), Logger: builder.Logger}
	view, err := materializer.MaterializeRefView(ctx, claim.Desire.Authority.GraphID, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(view.Close) // Registered after Store.Close, so the real read lease drains first.
	return dedicatedDeltaFixture{builder: builder, request: request, baseClaim: claim,
		base: commitLayerBase{Reader: view.Reader, corpus: builder.Store.AtGeneration(id)}, materializer: materializer, git: git}
}

func (f dedicatedDeltaFixture) reserve(t testing.TB, tree string) ClaimedDedicatedDeltaRequest {
	t.Helper()
	ctx := context.Background()
	catalog := f.builder.Store.Catalog()
	p, _, err := catalog.DedicatedBasePublication(ctx, f.baseClaim.Desire.Authority.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	identity := f.baseClaim.Desire.Identity
	identity.TreeOID = tree
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: f.baseClaim.Desire.Authority, ExpectedDesiredEpoch: p.Desire.Epoch, Identity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, ExpectedActiveGenerationID: f.baseClaim.GenerationID, AttemptToken: fmt.Sprintf("delta-attempt-%d", desire.Epoch),
		BaseGenerationID: f.baseClaim.GenerationID, LowerViewFingerprint: fmt.Sprintf("test-full-lower-%d", f.baseClaim.GenerationID), CreatedAt: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ClaimedDedicatedDeltaRequest{Claim: claim, Base: f.base, BaseTreeOID: f.request.Identity.TreeOID,
		RepoDir: f.request.RootPath, RootPath: f.request.RootPath, WorkspaceID: f.request.WorkspaceID, ProjectID: f.request.ProjectID}
}

func (f dedicatedDeltaFixture) commit(t testing.TB) string {
	t.Helper()
	f.git("add", "-A")
	f.git("-c", "user.name=Delta Test", "-c", "user.email=delta@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "delta target")
	return f.git("rev-parse", "HEAD^{tree}")
}

// This bounded oracle compares structural node identities and edge endpoint/kind
// MULTISETS, not timestamps or all metadata fields. Duplicate edges are retained.
func dedicatedDeltaStructure(reader graph.Reader) ([]string, []string) {
	var nodes, edges []string
	for _, node := range reader.AllNodes() {
		nodes = append(nodes, node.ID)
	}
	for _, edge := range reader.AllEdges() {
		edges = append(edges, fmt.Sprintf("%s\x00%s\x00%v", edge.From, edge.To, edge.Kind))
	}
	sort.Strings(nodes)
	sort.Strings(edges)
	return nodes, edges
}

// The fixture is deliberately small: compare every persisted node/edge field
// without excluding timestamps, metadata, source, or location information. Edge
// order is not semantic, but duplicate edges and all their values are retained.
func dedicatedDeltaAssertFullPayload(t *testing.T, got, want graph.Reader) {
	t.Helper()
	gotNodes, wantNodes := got.AllNodes(), want.AllNodes()
	if len(gotNodes) != len(wantNodes) {
		t.Fatalf("full node count got=%d want=%d", len(gotNodes), len(wantNodes))
	}
	byID := make(map[string]*graph.Node, len(gotNodes))
	for _, node := range gotNodes {
		if _, duplicate := byID[node.ID]; duplicate {
			t.Fatalf("duplicate composed node ID %q", node.ID)
		}
		byID[node.ID] = node
	}
	for _, node := range wantNodes {
		actual, found := byID[node.ID]
		if !found || !reflect.DeepEqual(actual, node) {
			t.Fatalf("full node mismatch for %q:\ngot=%#v\nwant=%#v", node.ID, actual, node)
		}
		delete(byID, node.ID)
	}
	gotEdges, wantEdges := got.AllEdges(), want.AllEdges()
	if len(gotEdges) != len(wantEdges) {
		t.Fatalf("full edge count got=%d want=%d", len(gotEdges), len(wantEdges))
	}
	matched := make([]bool, len(gotEdges))
	for _, edge := range wantEdges {
		found := false
		for i, actual := range gotEdges {
			if !matched[i] && reflect.DeepEqual(actual, edge) {
				matched[i], found = true, true
				break
			}
		}
		if !found {
			t.Fatalf("full edge missing from composed multiset:\nwant=%#v\ngot=%#v", edge, gotEdges)
		}
	}
}

func dedicatedDeltaNamedNode(t *testing.T, reader graph.Reader, name string) *graph.Node {
	t.Helper()
	var found *graph.Node
	for _, node := range reader.AllNodes() {
		if node.Name != name {
			continue
		}
		if found != nil {
			t.Fatalf("fixture has multiple nodes named %q", name)
		}
		found = node
	}
	if found == nil {
		t.Fatalf("fixture lacks node named %q", name)
	}
	return found
}

// JSON supplies an independent deep copy, but it is accepted only when all
// values survive a reflect.DeepEqual round trip. An unsupported/lossy field is
// a fixture failure, never a reason to drop that field from the payload oracle.
func dedicatedDeltaSnapshotNode(t *testing.T, reader graph.Reader, name string) *graph.Node {
	t.Helper()
	node := dedicatedDeltaNamedNode(t, reader, name)
	encoded, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	copy := new(graph.Node)
	if err := json.Unmarshal(encoded, copy); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(node, copy) {
		t.Fatalf("lossy node snapshot for %q:\noriginal=%#v\ncopy=%#v", name, node, copy)
	}
	return copy
}

func (f dedicatedDeltaFixture) assertColdStructure(t *testing.T, ctx context.Context, request ClaimedDedicatedDeltaRequest, id int64, verify func(graph.Reader, graph.Reader)) {
	t.Helper()
	// Deliberately index the same target in mutable gen0 only in this isolated
	// fixture. Dedicated materialization must neither inherit it nor duplicate it.
	target, err := source.NewGitTreeSource(ctx, f.request.RootPath, request.Claim.Desire.Identity.TreeOID)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	cold := f.builder.Store.AtGeneration(0)
	idx := New(cold, f.builder.Registry, f.builder.Config, f.builder.Logger)
	idx.SetRepoPrefix(f.request.RepoPrefix)
	idx.SetWorkspaceID(f.request.WorkspaceID)
	idx.SetProjectID(f.request.ProjectID)
	idx.SetContentSource(target)
	_, err = idx.IndexCtx(ctx, f.request.RootPath)
	idx.Close()
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.materializer.MaterializeRefView(ctx, request.Claim.Desire.Authority.GraphID, id)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	gotNodes, gotEdges := dedicatedDeltaStructure(view.Reader)
	wantNodes, wantEdges := dedicatedDeltaStructure(cold)
	if len(wantNodes) == 0 || len(wantEdges) == 0 {
		t.Fatal("cold positive control has no nodes/edges")
	}
	if !reflect.DeepEqual(gotNodes, wantNodes) || !reflect.DeepEqual(gotEdges, wantEdges) {
		t.Fatalf("delta/cold structural mismatch:\nnodes got=%v want=%v\nedges got=%v want=%v", gotNodes, wantNodes, gotEdges, wantEdges)
	}
	if verify != nil {
		verify(view.Reader, cold)
	}
}

// Full payload parity uses an independently allocated, fully built positive
// dedicated root with the SAME source root and complete output context. A fresh
// database prevents catalog ready reuse. The separate gen0 structural control
// remains above; its observed builtin scope difference is not normalized away.
func (f dedicatedDeltaFixture) coldPositive(t *testing.T, ctx context.Context, request ClaimedDedicatedDeltaRequest) graph.Reader {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cold-positive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog := store.Catalog()
	owner := request.Claim.Desire.Authority
	family := store_sqlite.RepositoryFamily{FamilyID: owner.FamilyID, CommonDirIdentity: filepath.Join(f.request.RootPath, ".git"), State: "active"}
	if err := catalog.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	checkout := store_sqlite.Checkout{CheckoutID: owner.Owner.CheckoutID, Incarnation: owner.Owner.Incarnation,
		FamilyID: family.FamilyID, RootPath: f.request.RootPath, GitDir: family.CommonDirIdentity, AdminName: "main",
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadTree: request.Claim.Desire.Identity.TreeOID, HeadCommit: f.git("rev-parse", "HEAD")}
	if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{GraphID: owner.GraphID, OwnerCheckoutID: checkout.CheckoutID,
		RepoPrefix: owner.RepoPrefix, FamilyID: family.FamilyID, IsPrimaryBase: true, State: store_sqlite.DedicatedGraphReady}); err != nil {
		t.Fatal(err)
	}
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: owner.GraphID, Owner: owner.Owner, Token: "cold-positive-authority"})
	if err != nil {
		t.Fatal(err)
	}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: authority, Identity: request.Claim.Desire.Identity})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "cold-positive-full", ProvenanceCommitOID: checkout.HeadCommit, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if claim.GenerationID <= 0 || claim.BaseGenerationID != 0 || claim.Status != "allocated" {
		t.Fatalf("cold control did not allocate a new full root: %+v", claim)
	}
	builder := *f.builder
	builder.Store = store
	id, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: f.request.RootPath, WorkspaceID: f.request.WorkspaceID, ProjectID: f.request.ProjectID})
	if err != nil || id != claim.GenerationID || report.Coalesced {
		t.Fatalf("cold full id=%d report=%+v err=%v", id, report, err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatal(err)
	}
	materializer := graphview.Materializer{Store: store, Catalog: catalog, Leases: graphview.NewLeaseManager(), Logger: builder.Logger}
	view, err := materializer.MaterializeRefView(ctx, owner.GraphID, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(view.Close)
	return view.Reader
}

func TestClaimedDedicatedDeltaRealGitStructuralParity(t *testing.T) {
	for _, change := range []string{"rename", "delete", "comment", "body-only", "dependency"} {
		t.Run(change, func(t *testing.T) {
			f := newDedicatedDeltaFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var oldCommitted, oldUnrelated *graph.Node
			if change == "comment" {
				oldCommitted = dedicatedDeltaSnapshotNode(t, f.base, "Committed")
				oldUnrelated = dedicatedDeltaSnapshotNode(t, f.base, "Unrelated")
			}
			if change == "body-only" || change == "dependency" {
				committed := dedicatedDeltaNamedNode(t, f.base, "Committed")
				dependent := dedicatedDeltaNamedNode(t, f.base, "Dependent")
				found := false
				for _, edge := range f.base.GetInEdges(committed.ID) {
					if edge.From == dependent.ID && edge.Kind == "calls" {
						found = true
					}
				}
				if !found {
					t.Fatal("healthy lower lacks the genuine Dependent to Committed incoming call")
				}
			}
			switch change {
			case "rename":
				if err := os.Rename(filepath.Join(f.request.RootPath, "rename.go"), filepath.Join(f.request.RootPath, "moved.go")); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(filepath.Join(f.request.RootPath, "delete.go")); err != nil {
					t.Fatal(err)
				}
			case "comment":
				original, err := os.ReadFile(filepath.Join(f.request.RootPath, "base.go"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(original), "func Committed()") != 1 {
					t.Fatal("comment fixture lacks one unique Committed declaration")
				}
				// Insert only a documentation comment and newline into the actual
				// fixture text; all pre-existing source bytes remain unchanged.
				body := strings.Replace(string(original), "func Committed()", "// Changed committed documentation.\nfunc Committed()", 1)
				if err := os.WriteFile(filepath.Join(f.request.RootPath, "base.go"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			case "body-only", "dependency":
				body := "package dedicated\nfunc Replacement() string { return \"replacement\" }\nfunc Committed() string { return Replacement() }\nfunc Caller() string { return Committed() }\n"
				if change == "dependency" {
					body = strings.Replace(body, "func Committed()", "func Committed(suffix ...string)", 1)
				}
				if err := os.WriteFile(filepath.Join(f.request.RootPath, "base.go"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			request := f.reserve(t, f.commit(t))
			if request.Claim.BaseGenerationID != f.baseClaim.GenerationID || request.Claim.GenerationID == f.baseClaim.GenerationID {
				t.Fatalf("not a positive child reservation: %+v", request.Claim)
			}
			// Dirty worktree divergence cannot become the committed delta's source.
			if err := os.WriteFile(filepath.Join(f.request.RootPath, "dirty-only.go"), []byte("package dedicated\nfunc DirtyOnly() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			id, report, err := f.builder.BuildClaimedDedicatedDelta(ctx, request)
			if err != nil || id != request.Claim.GenerationID {
				t.Fatalf("delta id=%d report=%+v err=%v", id, report, err)
			}
			active, _, err := f.builder.Store.Catalog().GetDedicatedGraph(ctx, request.Claim.Desire.Authority.GraphID)
			if err != nil || active.ActiveGenerationID != f.baseClaim.GenerationID {
				t.Fatalf("builder adopted pointer: %+v %v", active, err)
			}
			for _, path := range report.IndexedPaths {
				if path == "stable.go" || path == "dirty-only.go" {
					t.Fatalf("delta indexed unrelated/dirty source: %+v", report)
				}
			}
			if change == "delete" && report.DeletedFiles != 1 {
				t.Fatalf("delete masks absent: %+v", report)
			}
			if change == "rename" && (report.AddedFiles != 1 || report.DeletedFiles != 1) {
				t.Fatalf("rename did not split add/delete: %+v", report)
			}
			if change == "dependency" {
				found := false
				for _, path := range report.IndexedPaths {
					if path == "dependent.go" {
						found = true
					}
				}
				if !found {
					t.Fatalf("unchanged dependent missing from affected closure: %+v", report)
				}
			}
			if change == "body-only" {
				for _, path := range report.IndexedPaths {
					if path == "dependent.go" {
						t.Fatalf("body-only edit unnecessarily reparsed unchanged caller: %+v", report)
					}
				}
			}
			f.assertColdStructure(t, ctx, request, id, func(composed, _ graph.Reader) {
				if change != "comment" && change != "body-only" && change != "dependency" {
					return
				}
				cold := f.coldPositive(t, ctx, request)
				dedicatedDeltaAssertFullPayload(t, composed, cold)
				if change != "comment" {
					return
				}
				coldCommitted := dedicatedDeltaNamedNode(t, cold, "Committed")
				if reflect.DeepEqual(oldCommitted, coldCommitted) {
					t.Fatalf("comment/newline changed no persisted Committed value: %#v", coldCommitted)
				}
				if !reflect.DeepEqual(dedicatedDeltaNamedNode(t, composed, "Committed"), coldCommitted) {
					t.Fatal("composed Committed did not retain cold comment/location metadata")
				}
				if !reflect.DeepEqual(oldUnrelated, dedicatedDeltaNamedNode(t, cold, "Unrelated")) || !reflect.DeepEqual(oldUnrelated, dedicatedDeltaNamedNode(t, composed, "Unrelated")) {
					t.Fatal("unchanged Unrelated node changed; timestamp-only differences cannot establish the comment oracle")
				}
			})
		})
	}
}

func TestClaimedDedicatedDeltaConsecutiveAdoptedLayersFullPayload(t *testing.T) {
	f := newDedicatedDeltaFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	unrelated := dedicatedDeltaSnapshotNode(t, f.base, "Unrelated")
	if err := os.Rename(filepath.Join(f.request.RootPath, "rename.go"), filepath.Join(f.request.RootPath, "moved.go")); err != nil {
		t.Fatal(err)
	}
	firstBody := "package dedicated\n// First delta documentation.\nfunc Committed() string { return \"committed\" }\nfunc Caller() string { return Committed() }\n"
	if err := os.WriteFile(filepath.Join(f.request.RootPath, "base.go"), []byte(firstBody), 0o600); err != nil {
		t.Fatal(err)
	}
	first := f.reserve(t, f.commit(t))
	firstID, firstReport, err := f.builder.BuildClaimedDedicatedDelta(ctx, first)
	if err != nil || firstID != first.Claim.GenerationID {
		t.Fatalf("first delta id=%d report=%+v err=%v", firstID, firstReport, err)
	}
	for _, path := range firstReport.IndexedPaths {
		if path == "stable.go" {
			t.Fatalf("first delta indexed unrelated file: %+v", firstReport)
		}
	}
	catalog := f.builder.Store.Catalog()
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: first.Claim}); err != nil {
		t.Fatal(err)
	}
	active, found, err := catalog.GetDedicatedGraph(ctx, first.Claim.Desire.Authority.GraphID)
	if err != nil || !found || active.ActiveGenerationID != firstID {
		t.Fatalf("first guarded adoption did not install its generation: %+v %v", active, err)
	}

	// Acquire a NEW actual full-ancestry lease after adoption. The second builder
	// receives its composed Reader, never the first delta's sparse handle alone.
	lower, err := f.materializer.MaterializeRefView(ctx, first.Claim.Desire.Authority.GraphID, firstID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lower.Close)
	if !reflect.DeepEqual(lower.Reader.GetNode(unrelated.ID), unrelated) {
		t.Fatal("newly leased lower lost an unchanged full-root node")
	}
	secondFixture := f
	secondFixture.baseClaim = first.Claim
	secondFixture.request.Identity.TreeOID = first.Claim.Desire.Identity.TreeOID
	secondFixture.request.Identity.ProvenanceCommitOID = f.git("rev-parse", "HEAD")
	secondFixture.base = commitLayerBase{Reader: lower.Reader, corpus: f.builder.Store.AtGeneration(firstID)}
	secondBody := "package dedicated\n// Second delta updates the dependency.\nfunc Replacement() string { return \"replacement\" }\nfunc Committed() string { return Replacement() }\nfunc Caller() string { return Committed() }\n"
	if err := os.WriteFile(filepath.Join(f.request.RootPath, "base.go"), []byte(secondBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.request.RootPath, "delete.go")); err != nil {
		t.Fatal(err)
	}
	second := secondFixture.reserve(t, secondFixture.commit(t))
	if second.Claim.BaseGenerationID != firstID || second.BaseTreeOID != first.Claim.Desire.Identity.TreeOID {
		t.Fatalf("second reservation did not use the adopted first delta: %+v", second.Claim)
	}
	secondID, secondReport, err := f.builder.BuildClaimedDedicatedDelta(ctx, second)
	if err != nil || secondID != second.Claim.GenerationID || secondID == firstID {
		t.Fatalf("second delta id=%d report=%+v err=%v", secondID, secondReport, err)
	}
	for _, path := range secondReport.IndexedPaths {
		if path == "stable.go" || path == "moved.go" {
			t.Fatalf("second delta reindexed unchanged ancestor-owned source: %+v", secondReport)
		}
	}
	if secondReport.DeletedFiles != 1 {
		t.Fatalf("second delta did not retain its deletion mask: %+v", secondReport)
	}
	row, found, err := catalog.GetViewGeneration(ctx, secondID)
	if err != nil || !found || row.BaseGenerationID != firstID {
		t.Fatalf("second payload ancestry lost first delta: %+v %v", row, err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: second.Claim}); err != nil {
		t.Fatal(err)
	}
	active, found, err = catalog.GetDedicatedGraph(ctx, second.Claim.Desire.Authority.GraphID)
	if err != nil || !found || active.ActiveGenerationID != secondID {
		t.Fatalf("second guarded adoption did not install its generation: %+v %v", active, err)
	}
	// No cold corpus was indexed for the intermediate target. The gen0 oracle
	// starts empty and indexes only the final target, then compares the complete
	// two-delta composition (including first-delta rename and second deletion).
	secondFixture.assertColdStructure(t, ctx, second, secondID, func(composed, _ graph.Reader) {
		cold := secondFixture.coldPositive(t, ctx, second)
		dedicatedDeltaAssertFullPayload(t, composed, cold)
		if !reflect.DeepEqual(dedicatedDeltaNamedNode(t, composed, "Unrelated"), unrelated) {
			t.Fatal("two-delta chain changed unrelated full-root metadata")
		}
	})
}

func TestClaimedDedicatedDeltaStaleAndPolicyGuards(t *testing.T) {
	f := newDedicatedDeltaFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.request.RootPath, "new.go"), []byte("package dedicated\nfunc NewSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := f.reserve(t, f.commit(t))
	check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"base-tree", "config", "extractor", "resolver", "attempt", "lower"} {
		t.Run(field, func(t *testing.T) {
			bad := request
			switch field {
			case "base-tree":
				bad.BaseTreeOID = strings.Repeat("a", 40)
			case "config":
				bad.Claim.Desire.Identity.ConfigHash = "different-policy"
			case "extractor":
				bad.Claim.Desire.Identity.ExtractorVersions = "different-extractor"
			case "resolver":
				bad.Claim.Desire.Identity.ResolverVersion = "different-resolver"
			case "attempt":
				bad.Claim.AttemptToken = "replaced-attempt"
			case "lower":
				bad.Claim.BaseGenerationID++
			}
			_, _, err := f.builder.BuildClaimedDedicatedDelta(ctx, bad)
			if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) && !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
				t.Fatalf("guard error=%v", err)
			}
		})
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	// Independent healthy control proves the refused calls did not poison the
	// actual positive-parent reservation or make its payload unwritable.
	if _, _, err := f.builder.BuildClaimedDedicatedDelta(ctx, request); err != nil {
		t.Fatal(err)
	}
}

func TestClaimedDedicatedDeltaOnePhysicalAndReadyReplay(t *testing.T) {
	f := newDedicatedDeltaFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := os.WriteFile(filepath.Join(f.request.RootPath, "new.go"), []byte("package dedicated\nfunc DeltaOnly() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := f.reserve(t, f.commit(t))
	var callbacks atomic.Int32
	request.PrePublish = func(context.Context, int64) error { callbacks.Add(1); return nil }
	start := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, _, err := f.builder.BuildClaimedDedicatedDelta(ctx, request)
			if err == nil && id != request.Claim.GenerationID {
				err = fmt.Errorf("wrong generation %d", id)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if callbacks.Load() != 1 {
		t.Fatalf("physical publication callbacks=%d", callbacks.Load())
	}
	check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	request.RepoDir, request.RootPath, request.Base = "/nonexistent/delta-ready", "", nil
	for range 3 {
		id, report, err := f.builder.BuildClaimedDedicatedDelta(ctx, request)
		if err != nil || id != request.Claim.GenerationID || !report.Coalesced {
			t.Fatalf("ready id=%d report=%+v err=%v", id, report, err)
		}
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkClaimedDedicatedDeltaReadyReplay(b *testing.B) {
	f := newDedicatedDeltaFixture(b)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.request.RootPath, "bench.go"), []byte("package dedicated\nfunc BenchDelta() {}\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	request := f.reserve(b, f.commit(b))
	if _, _, err := f.builder.BuildClaimedDedicatedDelta(ctx, request); err != nil {
		b.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
	if err != nil {
		b.Fatal(err)
	}
	request.RepoDir, request.Base = "/nonexistent/ready-delta-benchmark", nil
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		id, report, err := f.builder.BuildClaimedDedicatedDelta(ctx, request)
		if err != nil || id != request.Claim.GenerationID || !report.Coalesced {
			b.Fatalf("ready delta id=%d err=%v", id, err)
		}
	}
	b.StopTimer()
	if err := check(); err != nil {
		b.Fatal(err)
	}
}

func TestClaimedDedicatedDeltaTreeEquivalentCommitReusesBase(t *testing.T) {
	f := newDedicatedDeltaFixture(t)
	ctx := context.Background()
	f.git("-c", "user.name=Delta Test", "-c", "user.email=delta@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "tree equivalent commit")
	tree := f.git("rev-parse", "HEAD^{tree}")
	if tree != f.request.Identity.TreeOID || f.git("rev-parse", "HEAD") == f.request.Identity.ProvenanceCommitOID {
		t.Fatal("fixture did not create a distinct tree-equivalent commit")
	}
	check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	request := f.reserve(t, tree)
	if request.Claim.GenerationID != f.baseClaim.GenerationID || !request.Claim.AlreadyAdopted || request.Claim.BaseGenerationID != 0 {
		t.Fatalf("tree-equivalent reuse allocated a delta: %+v", request.Claim)
	}
	// The dispatcher must reuse this existing full base, not force it through
	// the positive-parent delta builder and manufacture a no-op generation.
	if _, _, err := f.builder.BuildClaimedDedicatedDelta(ctx, request); !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
		t.Fatalf("full reservation admitted as delta: %v", err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimedDedicatedDeltaPolicyChangeRequiresReseed(t *testing.T) {
	f := newDedicatedDeltaFixture(t)
	ctx := context.Background()
	identity := f.baseClaim.Desire.Identity
	identity.ConfigHash = "new-output-affecting-policy"
	desire, err := f.builder.Store.Catalog().RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: f.baseClaim.Desire.Authority, ExpectedDesiredEpoch: f.baseClaim.Desire.Epoch, Identity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, ExpectedActiveGenerationID: f.baseClaim.GenerationID, AttemptToken: "wrong-policy-parent", BaseGenerationID: f.baseClaim.GenerationID,
	})
	if !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
		t.Fatalf("policy-incompatible parent reservation accepted: %v", err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

// Each measured candidate is independently rooted at the SAME adopted full
// base; no candidate is adopted in this benchmark. This measures one-file
// sparse build cost including Git metadata diff, not a runtime advancement
// chain, full runtime lifecycle cost, filesystem bytes, or physical disk writes.
func BenchmarkClaimedDedicatedDeltaOneFileFixedFullBaseUnadopted(b *testing.B) {
	f := newDedicatedDeltaFixture(b)
	ctx := context.Background()
	var indexed int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		body := fmt.Sprintf("package dedicated\n// Target iteration %d.\nfunc Committed() string { return \"committed\" }\nfunc Caller() string { return Committed() }\n", i)
		if err := os.WriteFile(filepath.Join(f.request.RootPath, "base.go"), []byte(body), 0o600); err != nil {
			b.Fatal(err)
		}
		request := f.reserve(b, f.commit(b))
		b.StartTimer()
		id, report, err := f.builder.BuildClaimedDedicatedDelta(ctx, request)
		b.StopTimer()
		if err != nil || id != request.Claim.GenerationID || report.Coalesced {
			b.Fatalf("physical delta id=%d report=%+v err=%v", id, report, err)
		}
		indexed += len(report.IndexedPaths)
	}
	if b.N > 0 {
		b.ReportMetric(float64(indexed)/float64(b.N), "indexed_paths/op")
	}
}

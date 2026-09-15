package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// Enrichment writes name one output generation, and no enrichment
// input reads a working copy the request did not select.
//
// Previously every enrichment producer wrote `s.graph` — generation zero,
// the shared corpus — whatever view the request had read, and blame/coverage
// read the TRACKED repository root rather than the selected checkout's. A
// request routed to a worktree therefore read one snapshot and enriched
// another, from a third one's bytes, with nothing naming the output.
//
// What these tests pin:
//
//  1. a corpus enrichment DECLARES generation zero through the authority;
//  2. a request that reads a checkout of its own is REFUSED at the primitive
//     and at the tool surface, so neither its write nor its `git blame` of the
//     live tree happens;
//  3. the refusal is forced by the store, not by taste — a published
//     generation's handle rejects writes;
//  4. enrichment supersession is producer-scoped and never takes the authority
//     away from a live index mutation;
//  5. concurrent admission leaves exactly one live authority per owner.

// routedEnrichmentCtx materializes the fixture's worktree route and returns a
// context carrying it, the shape the middleware installs for a request whose
// cwd is the worktree.
func routedEnrichmentCtx(t *testing.T, stack *viewStack) context.Context {
	t.Helper()
	ctx, _ := routedEnrichmentView(t, stack)
	return ctx
}

// routedEnrichmentView is routedEnrichmentCtx plus the view itself, for the
// tests that inspect what the request recorded against it.
func routedEnrichmentView(t *testing.T, stack *viewStack) (context.Context, *requestView) {
	t.Helper()
	materialized, err := stack.srv.materializer.MaterializeCheckout(context.Background(), viewTestWorktree)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	t.Cleanup(materialized.Close)
	view := &requestView{
		kind:         requestViewKindWorktree,
		reader:       materialized.Reader,
		materialized: materialized,
		viewRoot:     stack.worktreeRoot,
		rider:        &graphview.ViewRider{CheckoutID: viewTestWorktree, Exact: true},
	}
	return withRequestView(context.Background(), view), view
}

// writeCoverProfile drops a syntactically valid Go cover profile into root and
// returns its absolute path. The fixture repositories carry no go.mod, so the
// module path resolves empty and nothing is stamped — which is exactly right
// for the tests here: they are about which ROOT the profile was resolved
// against and what the answer declares, not about the projection itself.
func writeCoverProfile(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "cover.out")
	body := "mode: set\nexample.com/m/keep.go:3.16,3.18 1 1\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write cover profile: %v", err)
	}
	return path
}

// enrichmentProbeNode is a node no layer of the fixture carries, so finding it
// in a store proves which store a write went to.
func enrichmentProbeNode(id string) *graph.Node {
	return &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: "Probe", QualName: "repo.Probe",
		FilePath: "repo/probe.go", RepoPrefix: "repo", Language: "go",
		StartLine: 1, EndLine: 2,
	}
}

// TestRoutedGenerationsAreSealedAgainstEnrichment is the evidence behind the
// refusal, stated as a test rather than as a claim: the generation a routed
// request reads cannot be written at all, so "route the enrichment write to the
// generation the request read" is not implementable at this layer.
//
// If this ever stops holding — a generation that admits lifecycle-qualified
// writes while leased — the refusal below becomes a policy choice again and
// should be revisited.
func TestRoutedGenerationsAreSealedAgainstEnrichment(t *testing.T) {
	stack := newViewStack(t)
	handle := stack.store.AtGeneration(stack.dirty)
	// SetFileMasks is a generation write that reports rather than panics; the
	// node writers take the same seal and abort the process on it, which is
	// itself why an enrichment must never be pointed at a leased generation.
	err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/probe.go", Mode: store_sqlite.OwnershipReplace},
	})
	if !errors.Is(err, store_sqlite.ErrPayloadGenerationSealed) {
		t.Fatalf("a published generation accepted a write (%v); the enrichment routing premise changed", err)
	}
}

// TestUnroutedEnrichmentNamesGenerationZeroExplicitly is the base half: the
// write is the same write, but it is now a NAMED output with an owner the
// authority can order, instead of an unattributed mutation of whatever
// `s.graph` happened to be.
//
// Revert-red: with the producers back on `s.graph` there is no receipt, no
// owner and nothing to assert — Generation, Owner and the whole
// EnrichmentOutput do not exist.
func TestUnroutedEnrichmentNamesGenerationZeroExplicitly(t *testing.T) {
	stack := newViewStack(t)
	out, err := stack.srv.beginEnrichmentOutput(context.Background(), EnrichProducerChurn, "repo", stack.repoRoot)
	if err != nil {
		t.Fatalf("beginEnrichmentOutput with no view: %v", err)
	}
	if out.Generation != 0 {
		t.Fatalf("corpus enrichment named generation %d, want 0", out.Generation)
	}
	if out.Root != stack.repoRoot {
		t.Fatalf("corpus enrichment reads root %q, want %q", out.Root, stack.repoRoot)
	}
	if !strings.HasPrefix(out.Owner, "enrich:"+EnrichProducerChurn+"|") {
		t.Fatalf("owner key %q is not producer-scoped", out.Owner)
	}
	probe := "repo/probe.go::BaseProbe"
	out.Store.AddBatch([]*graph.Node{enrichmentProbeNode(probe)}, nil)
	if err := out.Complete(); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if node := stack.store.GetNode(probe); node == nil {
		t.Fatal("a corpus enrichment did not write the corpus")
	}
}

// TestRequestReadingItsOwnCheckoutIsNotEnriched is the core refusal: a request
// that reads a checkout of its own has no writable output generation.
//
// Revert-red: with beginEnrichmentOutput returning the base output for every
// request (what every producer did before), this returns a usable generation-0
// handle and the corpus is enriched behind a request that read a worktree.
func TestRequestReadingItsOwnCheckoutIsNotEnriched(t *testing.T) {
	stack := newViewStack(t)
	ctx := routedEnrichmentCtx(t, stack)

	out, err := stack.srv.beginEnrichmentOutput(ctx, EnrichProducerBlame, "repo", stack.repoRoot)
	if err == nil {
		out.Abandon()
		t.Fatalf("a routed request was handed output generation %d", out.Generation)
	}
	if !errors.Is(err, ErrEnrichmentSnapshotNotWritable) {
		t.Fatalf("refusal identity is %v, want ErrEnrichmentSnapshotNotWritable", err)
	}
	// The same must hold for the commit-only route, which is the shape a
	// committed identity is served in.
	routeCommitOnly(t, stack)
	if out, err := stack.srv.beginEnrichmentOutput(routedEnrichmentCtx(t, stack),
		EnrichProducerCoverage, "repo", stack.repoRoot); err == nil {
		out.Abandon()
		t.Fatal("a committed identity was handed a writable enrichment output")
	}
}

// TestBaseScopedViewStillEnrichesTheCorpus guards the other direction: a
// base-scoped narrowing has a reader but reads the shared corpus, so it must
// answer exactly as an unrouted request does. Keying the refusal on routed()
// instead of readsOwnCheckout() would break it.
func TestBaseScopedViewStillEnrichesTheCorpus(t *testing.T) {
	stack := newViewStack(t)
	ctx := withRequestView(context.Background(), &requestView{
		kind:         requestViewKindBase,
		reader:       stack.store,
		baseNarrowed: true,
	})
	out, err := stack.srv.beginEnrichmentOutput(ctx, EnrichProducerReleases, "repo", stack.repoRoot)
	if err != nil {
		t.Fatalf("a base-scoped view was refused: %v", err)
	}
	if out.Generation != 0 {
		t.Fatalf("base-scoped enrichment named generation %d, want 0", out.Generation)
	}
	out.Abandon()
}

// TestBlameToolUnderARoutedViewRefusesInsteadOfBlamingAnotherTree is the
// PRODUCTION-ENTRYPOINT trace for the blame half: the whole middleware, the
// real `analyze kind=blame` handler, a cwd inside the routed worktree.
//
// It proves the wiring reaches the primitive. Before the change this call swept
// s.collectRepoRoots() and ran `git blame` over the TRACKED repository's live
// worktree — a different tree from the one the request selected — then stamped
// the corpus with the result.
func TestBlameToolUnderARoutedViewRefusesInsteadOfBlamingAnotherTree(t *testing.T) {
	stack := newViewStack(t)
	res, err := stack.callHandler(t, stack.worktreeRoot, "analyze", map[string]any{"kind": "blame"},
		stack.srv.handleAnalyzeBlame)
	if err != nil {
		t.Fatalf("analyze kind=blame: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a routed request enriched blame: %s", viewResultText(t, res))
	}
	assertEnrichmentRefusal(t, viewResultText(t, res))
}

// TestChurnAndReleaseToolsUnderARoutedViewRefuse is the same trace for the two
// enrich_* tools, which reached the multi-indexer's repository sweep directly.
func TestChurnAndReleaseToolsUnderARoutedViewRefuse(t *testing.T) {
	for _, tool := range []string{"enrich_churn", "enrich_releases"} {
		t.Run(tool, func(t *testing.T) {
			stack := newViewStack(t)
			handler := stack.srv.handleEnrichChurn
			if tool == "enrich_releases" {
				handler = stack.srv.handleEnrichReleases
			}
			res, err := stack.callHandler(t, stack.worktreeRoot, tool, nil, handler)
			if err != nil {
				t.Fatalf("%s: %v", tool, err)
			}
			if !res.IsError {
				t.Fatalf("a routed request ran %s: %s", tool, viewResultText(t, res))
			}
			assertEnrichmentRefusal(t, viewResultText(t, res))
		})
	}
}

// TestCoverageToolUnderARoutedViewRefusesBeforeReadingTheProfile pins the
// coverage input side: the output is admitted before the profile path is
// resolved, so a routed request never reads a caller-supplied file against the
// wrong root.
func TestCoverageToolUnderARoutedViewRefusesBeforeReadingTheProfile(t *testing.T) {
	stack := newViewStack(t)
	res, err := stack.callHandler(t, stack.worktreeRoot, "analyze",
		map[string]any{"kind": "coverage", "profile": "cover.out"}, stack.srv.handleAnalyzeCoverage)
	if err != nil {
		t.Fatalf("analyze kind=coverage: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a routed request enriched coverage: %s", viewResultText(t, res))
	}
	text := viewResultText(t, res)
	assertEnrichmentRefusal(t, text)
	if strings.Contains(text, "read profile") {
		t.Fatalf("the profile was read before the output was admitted: %s", text)
	}
}

// TestCorpusCoverageDeclaresItsUnboundInput is the declared narrowing on the
// enrichment INPUT side: a cover profile is a caller-supplied producer input
// and nothing ties it to the snapshot it is stamped onto, so the answer says
// so rather than claiming snapshot exactness.
//
// The profile is REAL and the repository is NAMED, so the handler takes its
// success path — the only path that carries the declaration. (An earlier
// version of this test asserted the narrowing on a branch the fixture could
// never reach, which made the declaration deletable with every test still
// green.)
//
// Revert-red: delete the input_narrowing entry from the answer and this fails
// on the first assertion.
func TestCorpusCoverageDeclaresItsUnboundInput(t *testing.T) {
	stack := newViewStack(t)
	writeCoverProfile(t, stack.repoRoot)

	res, err := stack.callHandler(t, stack.repoRoot, "analyze",
		map[string]any{"kind": "coverage", "profile": "cover.out", "repo": "repo"},
		stack.srv.handleAnalyzeCoverage)
	if err != nil {
		t.Fatalf("analyze kind=coverage: %v", err)
	}
	payload := enrichmentPayload(t, res)
	if payload["input_narrowing"] != coverageProfileInputNarrowing {
		t.Fatalf("a corpus coverage answer did not declare its unbound input: %v", payload)
	}
	// The declaration is only meaningful next to a bound OUTPUT: generation
	// zero, the corpus, and the root of the repository the call named.
	if got, want := payload["generation"], float64(0); got != want {
		t.Fatalf("corpus coverage named generation %v, want %v", got, want)
	}
	if got := payload["root"]; got != stack.repoRoot {
		t.Fatalf("coverage resolved root %v, want the named repository %q", got, stack.repoRoot)
	}
	if got := payload["profile"]; got != filepath.Join(stack.repoRoot, "cover.out") {
		t.Fatalf("coverage read profile %v, want it under the named repository", got)
	}
	if payload["segments"] == nil || payload["segments"].(float64) < 1 {
		t.Fatalf("the profile was not actually parsed: %v", payload)
	}
}

// TestCoverageRefusesToPickARepositoryTheCallNeverNamed is the other half of
// the enrichment input side, and it closes a hole the first attempt at this
// narrowing opened.
//
// The root a coverage run resolves is load-bearing twice: it resolves the
// relative `profile` path, and it supplies the module path that decides which
// symbols the write lands on. A multi-repo daemon has no lone indexer to take
// it from, and picking the alphabetically first tracked prefix reads a profile
// out of — and stamps coverage onto — a repository the caller never mentioned.
// That is the same wrong-root input read the narrowing exists to close, so the
// ambiguity is refused and named instead.
//
// Revert-red: with `slices.Sorted(maps.Keys(targets))[0]` back, the fixture's
// two repositories resolve to "other" and the call fails with
// `read profile: open <otherRoot>/cover.out` — a different repository's tree.
func TestCoverageRefusesToPickARepositoryTheCallNeverNamed(t *testing.T) {
	stack := newViewStack(t)
	// The profile exists in exactly one of the two tracked repositories.
	writeCoverProfile(t, stack.repoRoot)

	res, err := stack.callHandler(t, stack.repoRoot, "analyze",
		map[string]any{"kind": "coverage", "profile": "cover.out"}, stack.srv.handleAnalyzeCoverage)
	if err != nil {
		t.Fatalf("analyze kind=coverage: %v", err)
	}
	if !res.IsError {
		t.Fatalf("coverage picked a repository the call never named: %s", viewResultText(t, res))
	}
	text := viewResultText(t, res)
	if !strings.Contains(text, "`repo`") {
		t.Fatalf("the ambiguity refusal does not name the way out: %s", text)
	}
	if strings.Contains(text, stack.otherRoot) || strings.Contains(text, "read profile") {
		t.Fatalf("coverage resolved the profile against an unnamed repository: %s", text)
	}
}

// TestASupersededEnrichmentReportsTheWriteItMade pins the settlement contract
// for a producer that stamps as it goes.
//
// blame.EnrichGraph has written everything it is going to write before Complete
// is called, so a supersession there is an ORDERING statement — "a newer run
// owns this output" — not "the write did not happen". Reporting the repository
// as an error (which is what the first version of this code did) under-reports
// work that demonstrably landed.
//
// The rival is admitted from inside the producer's own first store read, which
// is the deterministic stand-in for a second `analyze kind=blame` arriving
// while this one walks the graph.
//
// Revert-red: with `if err := out.Complete(); err != nil { perRepo[prefix] =
// {"root":…, "error": err.Error()} }` back, per_repo carries `error` and no
// `superseded`, and both assertions below fail.
func TestASupersededEnrichmentReportsTheWriteItMade(t *testing.T) {
	stack := newViewStack(t)
	rival := &enrichmentRivalStore{
		Store:     stack.srv.graph,
		authority: stack.srv.outputGenerationAuthority(),
		producer:  EnrichProducerBlame,
		prefix:    "repo",
		root:      stack.repoRoot,
	}
	stack.srv.graph = rival

	res, err := stack.callHandler(t, stack.repoRoot, "analyze",
		map[string]any{"kind": "blame", "repo": "repo"}, stack.srv.handleAnalyzeBlame)
	if err != nil {
		t.Fatalf("analyze kind=blame: %v", err)
	}
	payload := enrichmentPayload(t, res)
	if !rival.fired.Load() {
		t.Fatal("the rival enrichment never ran; the producer did not read the store")
	}
	perRepo, ok := payload["per_repo"].(map[string]any)
	if !ok {
		t.Fatalf("per_repo missing: %v", payload)
	}
	entry, ok := perRepo["repo"].(map[string]any)
	if !ok {
		t.Fatalf("per_repo[repo] missing: %v", payload)
	}
	if entry["superseded"] != true {
		t.Fatalf("a superseded blame run did not say so: %v", entry)
	}
	if entry["error"] != nil {
		t.Fatalf("a write that landed was reported as a failure: %v", entry)
	}
	if entry["enriched"] == nil {
		t.Fatalf("a superseded run under-reported the stamps it wrote: %v", entry)
	}
}

// enrichmentRivalStore admits a competing enrichment for the SAME owner from
// inside the producer's first store read. It delegates everything else, so the
// handler under test runs unmodified.
type enrichmentRivalStore struct {
	graph.Store
	authority *indexer.OutputGenerationAuthority
	producer  string
	prefix    string
	root      string
	fired     atomic.Bool
}

func (s *enrichmentRivalStore) AllNodes() []*graph.Node {
	if s.fired.CompareAndSwap(false, true) {
		// Same store handle, same producer, same prefix: the same owner key.
		if rival, err := BeginBaseEnrichment(context.Background(), s.authority, s,
			s.producer, s.prefix, s.root); err == nil {
			rival.Abandon()
		}
	}
	return s.Store.AllNodes()
}

// TestEnrichmentSupersedesOnlyItsOwnProducer is the authority contract.
//
// Two runs of one producer over one output are the same output decision, so the
// later one takes the authority and the earlier cannot fulfil it. An enrichment
// and an INDEX mutation are not: an enrichment must never take the authority
// away from a live reindex, which would make a legitimate index publish report
// itself as superseded.
func TestEnrichmentSupersedesOnlyItsOwnProducer(t *testing.T) {
	authority := indexer.NewOutputGenerationAuthority(nil)
	store := newEnrichmentProbeStore(t)

	older, err := BeginBaseEnrichment(context.Background(), authority, store, EnrichProducerBlame, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("first blame enrichment: %v", err)
	}
	newer, err := BeginBaseEnrichment(context.Background(), authority, store, EnrichProducerBlame, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("second blame enrichment: %v", err)
	}
	if err := older.Complete(); !errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("the superseded blame run settled with %v, want ErrOutputMutationReceiptSuperseded", err)
	}
	if err := newer.Complete(); err != nil {
		t.Fatalf("the current blame run could not fulfil its generation: %v", err)
	}

	// A different producer over the same output is a different owner.
	blame, err := BeginBaseEnrichment(context.Background(), authority, store, EnrichProducerBlame, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("blame: %v", err)
	}
	churnOut, err := BeginBaseEnrichment(context.Background(), authority, store, EnrichProducerChurn, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("churn: %v", err)
	}
	if err := blame.Complete(); err != nil {
		t.Fatalf("churn superseded blame: %v", err)
	}
	if err := churnOut.Complete(); err != nil {
		t.Fatalf("churn could not fulfil its generation: %v", err)
	}

	// And an index mutation for the same repository keeps its own authority.
	index, err := authority.Begin(context.Background(), indexer.OutputEntryIndexFile, indexer.OutputMutationTarget{
		Kind: indexer.OutputGenerationLegacy, OwnerKey: "root:/tmp/repo", RepoPrefix: "repo",
	})
	if err != nil {
		t.Fatalf("index mutation: %v", err)
	}
	enrich, err := BeginBaseEnrichment(context.Background(), authority, store, EnrichProducerReleases, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("releases enrichment: %v", err)
	}
	if err := index.Complete(); err != nil {
		t.Fatalf("an enrichment took the authority away from a live index mutation: %v", err)
	}
	enrich.Abandon()
}

// TestConcurrentEnrichmentAdmissionSelectsOneWinner is the race-relevant half:
// many producers admitting at once must leave exactly one live authority per
// owner and must not corrupt the authority's accounting.
func TestConcurrentEnrichmentAdmissionSelectsOneWinner(t *testing.T) {
	authority := indexer.NewOutputGenerationAuthority(nil)
	store := newEnrichmentProbeStore(t)

	const runs = 16
	outs := make([]*EnrichmentOutput, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := BeginBaseEnrichment(context.Background(), authority, store,
				EnrichProducerCoverage, "repo", "/tmp/repo")
			if err != nil {
				t.Errorf("admission %d: %v", i, err)
				return
			}
			outs[i] = out
		}(i)
	}
	wg.Wait()

	fulfilled := 0
	for _, out := range outs {
		if out == nil {
			continue
		}
		if err := out.Complete(); err == nil {
			fulfilled++
		} else if !errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded) {
			t.Fatalf("unexpected settlement: %v", err)
		}
	}
	if fulfilled != 1 {
		t.Fatalf("%d concurrent coverage enrichments fulfilled the generation, want exactly 1", fulfilled)
	}
	if stats := authority.Stats(); stats.Issued != runs || stats.Settled != runs {
		t.Fatalf("authority accounting drifted: %+v", stats)
	}
}

func assertEnrichmentRefusal(t *testing.T, text string) {
	t.Helper()
	if !strings.Contains(text, "no writable output") {
		t.Fatalf("refusal %q does not name the reason", text)
	}
	if !strings.Contains(text, "gortex enrich") {
		t.Fatalf("refusal %q does not name what the caller can do instead", text)
	}
}

// newEnrichmentProbeStore is a throwaway store handle used only as an output
// identity in the authority tests.
func newEnrichmentProbeStore(t *testing.T) graph.Store {
	t.Helper()
	store, err := store_sqlite.Open(t.TempDir() + "/probe.sqlite")
	if err != nil {
		t.Fatalf("open probe store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func enrichmentPayload(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("handler returned an error result: %s", viewResultText(t, res))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &payload); err != nil {
		t.Fatalf("decode result: %v (%s)", err, viewResultText(t, res))
	}
	return payload
}

// TestEnrichmentFallsBackToTheIndexersOwnAuthority pins that "one authority
// per process" is literally one OBJECT, not one shape.
//
// An authority is a serialization universe: owner keys in one cannot supersede,
// order, or collide with owner keys in another. A package-local `sync.Once`
// here — which is what this was — meant that on a server nothing installed an
// indexer on, an enrichment named an output that nothing else in the process
// could see, while claiming in its own comment to mirror the indexer's default.
// "Mirroring" is exactly the wrong property.
//
// Revert-red: restore a `sync.Once` + NewOutputGenerationAuthority(nil) in this
// package and the standing receipt below settles cleanly, because the two doors
// are admitting into different authorities.
func TestEnrichmentFallsBackToTheIndexersOwnAuthority(t *testing.T) {
	store := newEnrichmentProbeStore(t)
	srv := &Server{graph: store}

	if got, want := srv.outputGenerationAuthority(), indexer.DefaultOutputGenerationAuthority(); got != want {
		t.Fatalf("a server with no indexer resolved a second authority (%p, want %p)", got, want)
	}

	// The behavioural form of the same statement: a receipt admitted through
	// the indexer package's own process authority is superseded by this
	// server's enrichment of the same output, which can only happen if both
	// admitted into one authority.
	standing, err := BeginBaseEnrichment(context.Background(), indexer.DefaultOutputGenerationAuthority(),
		store, EnrichProducerChurn, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("standing admission: %v", err)
	}
	out, err := srv.beginEnrichmentOutput(context.Background(), EnrichProducerChurn, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("server enrichment: %v", err)
	}
	defer out.Abandon()
	if err := standing.Complete(); !errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("the standing receipt settled with %v; the two doors are in different authorities", err)
	}
}

// TestSettlementIsOneSpellingForEveryDoor pins the shared settlement contract
// the CLI door now uses: a supersession is reported, a real failure is an
// error, and the caller is told which it was.
//
// It is exported for cmd/gortex — the control socket runs the same producers
// against the same corpus through the same authority, and the two doors
// diverged precisely because the settlement had two spellings.
func TestSettlementIsOneSpellingForEveryDoor(t *testing.T) {
	authority := indexer.NewOutputGenerationAuthority(nil)
	store := newEnrichmentProbeStore(t)

	older, err := BeginBaseEnrichment(context.Background(), authority, store, EnrichProducerBlame, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	newer, err := BeginBaseEnrichment(context.Background(), authority, store, EnrichProducerBlame, "repo", "/tmp/repo")
	if err != nil {
		t.Fatalf("second admission: %v", err)
	}

	superseded, err := older.Settle()
	if err != nil {
		t.Fatalf("a superseded settlement was reported as a failure: %v", err)
	}
	if !superseded {
		t.Fatal("the overtaken run was not told it had been superseded")
	}
	superseded, err = newer.Settle()
	if err != nil || superseded {
		t.Fatalf("the current run settled as superseded=%v err=%v", superseded, err)
	}
}

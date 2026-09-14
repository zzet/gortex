package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// copyProbeCall is one invocation of the row-level generation copy.
type copyProbeCall struct {
	From       int64
	To         int64
	RepoPrefix string
}

// copyProbe stands in for the store-side row-level copy primitive.
//
// It performs a REAL copy through the store's own generation handles — the
// destination generation ends up carrying generation zero's rows — so the
// assertions below are about the payload that actually landed rather than
// about a recorded intention. The production primitive does the same move in
// SQL across every generation-keyed table; this one moves what this package
// can reach, which is enough for the route, the masks and the composed view.
type copyProbe struct {
	mu    sync.Mutex
	store *store_sqlite.Store
	calls []copyProbeCall
	fail  error

	// bulk, when set, is sampled at the moment the copy runs. That is what
	// turns "the bracket was opened once" into "the bracket was open AROUND
	// THE WRITE", which is the property the window exists for.
	bulk           *bulkLoadProbe
	openDuringCopy []bool
}

func (c *copyProbe) CopyPayloadGeneration(ctx context.Context, from, to int64, repoPrefix string) (GenerationCopyCounts, error) {
	c.mu.Lock()
	c.calls = append(c.calls, copyProbeCall{From: from, To: to, RepoPrefix: repoPrefix})
	if c.bulk != nil {
		c.openDuringCopy = append(c.openDuringCopy, c.bulk.isOpen())
	}
	fail := c.fail
	c.mu.Unlock()
	if fail != nil {
		return GenerationCopyCounts{}, fail
	}
	source := c.store.AtGeneration(from)
	destination, err := c.store.AtManagedGeneration(to)
	if err != nil {
		return GenerationCopyCounts{}, err
	}
	nodes, edges := source.GetRepoNodes(repoPrefix), source.GetRepoEdges(repoPrefix)
	if err := destination.AddBatchChecked(nodes, edges); err != nil {
		return GenerationCopyCounts{}, err
	}
	metas, err := source.FileMetasForRepo(repoPrefix)
	if err != nil {
		return GenerationCopyCounts{}, err
	}
	if err := destination.SetFileMetas(repoPrefix, metas); err != nil {
		return GenerationCopyCounts{}, err
	}
	return GenerationCopyCounts{
		Nodes: int64(len(nodes)), Edges: int64(len(edges)),
		Rows: int64(len(nodes) + len(edges) + len(metas)),
	}, nil
}

// bulkLoadProbe records the generation-scoped bulk-load bracket. Its shape is
// *store_sqlite.Store's own: a compile-time assertion below keeps the two from
// drifting, because the production wiring is an optional interface assertion
// and a drifted signature would silently stop taking the window.
type bulkLoadProbe struct {
	mu     sync.Mutex
	begun  []int64
	ended  int
	open   bool
	refuse bool
	fail   error

	// contended counts the asks that arrived while a window was already open.
	// It is the ownership witness the begin/end totals cannot be: a follower
	// that wins the window refuses the leader instead, and the totals stay
	// 1 begin / 1 close either way. Zero contention is the exact statement of
	// "only the flight leader ever asks", which is what taking the bracket
	// inside the leader-only payload preparation buys.
	contended int
}

func (p *bulkLoadProbe) BeginGenerationBulkLoad(generationID int64) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return false, p.fail
	}
	if p.refuse {
		return false, nil
	}
	if p.open {
		// The store refuses a second window while one is open rather than
		// failing (bulk_load.go: `if s.bulkConn != nil … return false, nil`).
		// A probe that handed out two would hide a caller that opened one.
		p.contended++
		return false, nil
	}
	p.begun = append(p.begun, generationID)
	p.open = true
	return true, nil
}

func (p *bulkLoadProbe) EndGenerationBulkLoad() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended++
	p.open = false
	return nil
}

func (p *bulkLoadProbe) isOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.open
}

func (p *bulkLoadProbe) counts() ([]int64, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.begun...), p.ended
}

// contentions reports how many asks were refused because a window was already
// open.
func (p *bulkLoadProbe) contentions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contended
}

// The production path resolves the bracket by asserting *store_sqlite.Store
// against GenerationBulkLoader. These assertions are what make that binding a
// build-time fact instead of a runtime hope.
var (
	_ GenerationBulkLoader = (*bulkLoadProbe)(nil)
	_ GenerationBulkLoader = (*store_sqlite.Store)(nil)
)

// claimedCopyFixture is the private claimed-base fixture with an observed
// logger, so the route the build took can be read off the production log line
// rather than inferred from a side effect.
func claimedCopyFixture(t *testing.T) (*SparseGenerationBuilder, dedicatedBuilderFixtureRequest, store_sqlite.DedicatedBaseBuildClaim, *observer.ObservedLogs) {
	t.Helper()
	builder, request, claim := privateClaimedDedicatedFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	builder.Logger = zap.New(core)
	return builder, request, claim, logs
}

// indexGenerationZero indexes the checkout into the mutable working-copy view
// exactly as a cold daemon start does, which is what gives generation zero its
// payload AND its repo_index_state provenance row.
func indexGenerationZero(t *testing.T, builder *SparseGenerationBuilder, request dedicatedBuilderFixtureRequest) {
	t.Helper()
	cold := builder.Store.AtGeneration(0)
	idx := New(cold, builder.Registry, builder.Config, builder.Logger)
	idx.SetRepoPrefix(request.RepoPrefix)
	idx.SetWorkspaceID(request.WorkspaceID)
	idx.SetProjectID(request.ProjectID)
	_, err := idx.IndexCtx(context.Background(), request.RootPath)
	idx.Close()
	if err != nil {
		t.Fatalf("index generation zero: %v", err)
	}
	if nodes := cold.GetRepoNodes(request.RepoPrefix); len(nodes) == 0 {
		t.Fatal("generation zero indexed nothing; the fixture is not a positive control")
	}
}

// claimedRoute reads back the route the build logged.
func claimedRoute(t *testing.T, logs *observer.ObservedLogs) (string, string) {
	t.Helper()
	entries := logs.FilterMessage("claimed dedicated base source plan").All()
	if len(entries) != 1 {
		t.Fatalf("want exactly one source-plan log line, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	route, _ := fields["route"].(string)
	reason, _ := fields["reason"].(string)
	return route, reason
}

// claimedBasePayload renders every persisted node and edge field of a composed
// view. Node counts are explicitly NOT the oracle: resolved targets, locations,
// signatures, provenance and the semantic columns all ride on this string.
func claimedBasePayload(reader graph.Reader) ([]string, []string) {
	var nodes, edges []string
	for _, node := range reader.AllNodes() {
		nodes = append(nodes, renderClaimedNode(node))
	}
	for _, edge := range reader.AllEdges() {
		edges = append(edges, fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%g\x00%s\x00%v\x00%s",
			edge.From, edge.To, edge.Kind, edge.FilePath, edge.Line,
			edge.Origin, edge.ConfidenceLabel, edge.Confidence, edge.Tier, edge.CrossRepo,
			claimedBaseMeta(edge.Meta)))
	}
	sort.Strings(nodes)
	sort.Strings(edges)
	return nodes, edges
}

// renderClaimedNode renders every persisted node field.
func renderClaimedNode(node *graph.Node) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		node.ID, node.Kind, node.Name, node.QualName, node.FilePath,
		node.StartLine, node.EndLine, node.StartColumn, node.EndColumn,
		node.Language, node.RepoPrefix, node.WorkspaceID, node.ProjectID, node.Origin,
		claimedBaseMeta(node.Meta))
}

// claimedBaseMeta renders a metadata map deterministically so it can ride on
// the payload comparison instead of being excluded from it.
func claimedBaseMeta(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%v;", key, meta[key])
	}
	return b.String()
}

// materializeClaimedBase opens the published base as its own composed view.
func materializeClaimedBase(t *testing.T, builder *SparseGenerationBuilder, graphID string, generationID int64) *graphview.RepoView {
	t.Helper()
	materializer := &graphview.Materializer{
		Store: builder.Store, Catalog: builder.Store.Catalog(), Leases: graphview.NewLeaseManager(),
	}
	view, err := materializer.MaterializeRefView(context.Background(), graphID, generationID)
	if err != nil {
		t.Fatalf("materialize claimed base %d: %v", generationID, err)
	}
	t.Cleanup(func() { view.Close() })
	return view
}

// buildClaimedBase runs one claimed initial base to publication and adoption.
func buildClaimedBase(t *testing.T, builder *SparseGenerationBuilder, request ClaimedDedicatedBaseRequest) int64 {
	t.Helper()
	id, _ := buildClaimedBaseWithReport(t, builder, request)
	return id
}

// buildClaimedBaseWithReport is buildClaimedBase plus the report the build
// produced, for the assertions that are about what the build SAID it did.
func buildClaimedBaseWithReport(t *testing.T, builder *SparseGenerationBuilder, request ClaimedDedicatedBaseRequest) (int64, BuildReport) {
	t.Helper()
	ctx := context.Background()
	id, report, err := builder.BuildClaimedDedicatedBase(ctx, request)
	if err != nil {
		t.Fatalf("build claimed base: %v", err)
	}
	if report.Coalesced {
		t.Fatal("a freshly allocated reservation reported reuse")
	}
	if _, err := builder.Store.Catalog().AdoptDedicatedBaseGeneration(ctx,
		store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: request.Claim}); err != nil {
		t.Fatalf("adopt claimed base %d: %v", id, err)
	}
	return id, report
}

// A clean checkout whose HEAD tree is the tree the reservation names is the
// case the first committed base exists for, and it is the case that re-parses
// today. The copy route must take it, must reach the copy primitive from the
// production entry point, and must publish a base whose composed payload is
// the one the re-parse of the same tree publishes.
func TestClaimedDedicatedBaseCopiesGenerationZeroOnACleanMatchingTree(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	assertGenerationZeroMatchesTheReservedExtractors(t, builder, request)
	probe := &copyProbe{store: builder.Store}

	id, report := buildClaimedBaseWithReport(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "copy_generation_zero" {
		t.Fatalf("route = %q (%s), want the copy plan on a clean matching tree", route, reason)
	}
	want := []copyProbeCall{{From: 0, To: id, RepoPrefix: request.RepoPrefix}}
	if len(probe.calls) != 1 || probe.calls[0] != want[0] {
		t.Fatalf("copy calls = %+v, want %+v", probe.calls, want)
	}

	// The published base is the copy, and it is a whole base: the masks the
	// build derived have to claim the copied files, or the composed view would
	// serve nothing.
	view := materializeClaimedBase(t, builder, request.Identity.GraphID, id)
	gotNodes, gotEdges := claimedBasePayload(view.Reader)
	if len(gotNodes) == 0 || len(gotEdges) == 0 {
		t.Fatalf("copied base composed empty: %d nodes, %d edges", len(gotNodes), len(gotEdges))
	}
	if node := view.Reader.GetNode(request.RepoPrefix + "/base.go::Committed"); node == nil {
		t.Fatal("copied base does not carry the committed symbol")
	}

	// The oracle: an independent fixture over byte-identical content, whose
	// generation zero was indexed the same way, built through the re-parse
	// route. The two arms then differ in exactly one thing — where the base's
	// rows came from.
	//
	// The reference arm is sent down the re-parse route by an UNCOMMITTED file
	// written after its generation zero was indexed. That is the cheapest
	// refusal that leaves the committed tree — the bytes both arms describe —
	// untouched: a re-parse reads the tree at the reserved TreeOID, which has
	// never seen this file. (Before the store carried the copy primitive this
	// arm got its re-parse for free, by passing no CopySource; it cannot any
	// more, which is the whole point of the wiring this item lands.)
	reference, referenceRequest, referenceClaim, referenceLogs := claimedCopyFixture(t)
	indexGenerationZero(t, reference, referenceRequest)
	const uncommitted = "package dedicated\n\nfunc UncommittedOnly() string { return \"uncommitted\" }\n"
	if err := os.WriteFile(filepath.Join(referenceRequest.RootPath, "uncommitted.go"), []byte(uncommitted), 0o600); err != nil {
		t.Fatal(err)
	}
	referenceID, referenceReport := buildClaimedBaseWithReport(t, reference, ClaimedDedicatedBaseRequest{
		Claim: referenceClaim, RootPath: referenceRequest.RootPath,
		WorkspaceID: referenceRequest.WorkspaceID, ProjectID: referenceRequest.ProjectID,
	})
	if route, reason := claimedRoute(t, referenceLogs); route != "reparse_git_tree" {
		t.Fatalf("reference route = %q (%s), want the re-parse plan the uncommitted file forces", route, reason)
	}

	// The build report is part of what the route produces. Without an
	// assertion on it the copy route could publish a correct payload while
	// reporting an empty inventory and every other check here would stay green.
	//
	// Its oracle is generation ZERO's inventory — the source of the copy, not
	// the destination the report is read back from — bounded by the re-parse
	// arm's enumeration of the committed tree. The two routes count different
	// things on purpose and the report must not pretend otherwise: the
	// re-parse reports every file the snapshot source WALKED, while a copy
	// reports the files the generation carries payload for, which is a subset
	// (the fixture's go.mod is walked and carries no file-meta row).
	assertCopiedInventory(t, report, builder, request, referenceReport)
	if report.NodeCount == 0 || report.EdgeCount == 0 {
		t.Fatalf("the copy route reported no rows moved: %+v", report)
	}
	referenceView := materializeClaimedBase(t, reference, referenceRequest.Identity.GraphID, referenceID)
	wantNodes, wantEdges := claimedBasePayload(referenceView.Reader)
	if strings.Join(gotEdges, "\n") != strings.Join(wantEdges, "\n") {
		t.Fatalf("copied base edges differ from the re-parsed base:\ngot=%v\nwant=%v", gotEdges, wantEdges)
	}
	if strings.Join(gotNodes, "\n") != strings.Join(wantNodes, "\n") {
		assertOnlyBuiltinStampingDiffers(t, view.Reader, referenceView.Reader)
	}

	// Generation zero is still the mutable working-copy view: the copy read it,
	// it was not relabelled and it was not emptied.
	if nodes := builder.Store.AtGeneration(0).GetRepoNodes(request.RepoPrefix); len(nodes) == 0 {
		t.Fatal("the copy consumed generation zero instead of reading it")
	}
	row, found, err := builder.Store.Catalog().GetViewGeneration(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("published base has no catalog row: found=%v err=%v", found, err)
	}
	if row.TreeOID != request.Identity.TreeOID || row.ConfigHash != request.Identity.ConfigHash ||
		row.ExtractorVersions != request.Identity.ExtractorVersions ||
		row.ResolverVersion != request.Identity.ResolverVersion || row.BaseGenerationID != 0 {
		t.Fatalf("copied base did not keep its own reserved identity: %+v", row)
	}
}

// assertCopiedInventory pins what the copy route's build report says.
//
// The oracle is generation zero's own file inventory: that is what the copy
// moved, so the destination generation has to report exactly it. The re-parse
// arm bounds the answer from the other side — a copied base cannot report a
// file the committed tree's own enumeration never saw.
func assertCopiedInventory(t *testing.T, got BuildReport, builder *SparseGenerationBuilder, request dedicatedBuilderFixtureRequest, reparsed BuildReport) {
	t.Helper()
	if got.AddedFiles == 0 || len(got.IndexedPaths) == 0 {
		t.Fatalf("the copy route reported an empty inventory: %+v", got)
	}
	if got.AddedFiles != len(got.IndexedPaths) {
		t.Fatalf("AddedFiles=%d disagrees with %d reported paths", got.AddedFiles, len(got.IndexedPaths))
	}
	if got.SourceBytes <= 0 {
		t.Fatalf("the copy route reported %d source bytes behind %d files", got.SourceBytes, got.AddedFiles)
	}
	metas, err := builder.Store.AtGeneration(0).FileMetasForRepo(request.RepoPrefix)
	if err != nil {
		t.Fatalf("read generation zero's inventory: %v", err)
	}
	var want []string
	var wantBytes int64
	for _, meta := range metas {
		rel, owned := builderRelPath(request.RepoPrefix, meta.FilePath)
		if !owned {
			continue
		}
		want = append(want, rel)
		if meta.Size > 0 {
			wantBytes += int64(meta.Size)
		}
	}
	gotPaths := append([]string(nil), got.IndexedPaths...)
	sort.Strings(gotPaths)
	sort.Strings(want)
	if strings.Join(gotPaths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the copied base reports a different inventory than the generation it copied:\ngot=%v\nwant=%v",
			gotPaths, want)
	}
	if got.SourceBytes != wantBytes {
		t.Fatalf("SourceBytes = %d, want generation zero's %d", got.SourceBytes, wantBytes)
	}
	walked := make(map[string]struct{}, len(reparsed.IndexedPaths))
	for _, path := range reparsed.IndexedPaths {
		walked[path] = struct{}{}
	}
	for _, path := range gotPaths {
		if _, found := walked[path]; !found {
			t.Fatalf("the copied base reports %q, which the committed tree's own enumeration never walked: %v",
				path, reparsed.IndexedPaths)
		}
	}
}

// A generation's repo_index_state row is a per-generation sidecar written at
// the tail of an index pass — and the copy route runs no pass. Without an
// explicit write the copied base would be the one committed base that cannot
// say which commit it describes, which is also the row this route's own
// predicate reads out of generation zero.
//
// The re-parsed arm is the oracle again: the two bases describe the same
// commit, so they must record the same commit, the same clean bit and the same
// extractor versions.
func TestClaimedDedicatedBaseCopyRecordsItsOwnIndexState(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	assertGenerationZeroMatchesTheReservedExtractors(t, builder, request)
	// IndexedAt describes when THESE rows landed in THIS generation, so the
	// copy re-stamps it rather than inheriting the source's. Backdating
	// generation zero is what makes that checkable: without it the two stamps
	// would agree by coincidence inside the same second.
	backdated := generationZeroIndexState(t, builder, request)
	backdated.IndexedAt = time.Now().Add(-2 * time.Hour).Unix()
	setGenerationZeroIndexState(t, builder, backdated)
	probe := &copyProbe{store: builder.Store}

	id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})
	if route, reason := claimedRoute(t, logs); route != "copy_generation_zero" {
		t.Fatalf("route = %q (%s), want the copy plan", route, reason)
	}

	copied, found, err := builder.Store.AtGeneration(id).GetRepoIndexState(request.RepoPrefix)
	if err != nil {
		t.Fatalf("read the copied generation's index state: %v", err)
	}
	if !found {
		t.Fatal("the copied base carries no repo_index_state row of its own")
	}
	if copied.NodeCount == 0 || copied.EdgeCount == 0 {
		t.Fatalf("the copied base recorded an empty index state: %+v", copied)
	}

	reference, referenceRequest, referenceClaim, _ := claimedCopyFixture(t)
	indexGenerationZero(t, reference, referenceRequest)
	referenceID := buildClaimedBase(t, reference, ClaimedDedicatedBaseRequest{
		Claim: referenceClaim, RootPath: referenceRequest.RootPath,
		WorkspaceID: referenceRequest.WorkspaceID, ProjectID: referenceRequest.ProjectID,
	})
	want, found, err := reference.Store.AtGeneration(referenceID).GetRepoIndexState(referenceRequest.RepoPrefix)
	if err != nil || !found {
		t.Fatalf("the re-parsed base has no index state to compare against: found=%v err=%v", found, err)
	}
	// Each fixture is its own checkout, so the two arms cannot share a commit
	// id: what they must share is the RELATION — each base records the commit
	// its own checkout is on.
	if head := gitInFixture(t, request.RootPath)("rev-parse", "HEAD"); copied.IndexedSHA != head {
		t.Fatalf("copied base recorded commit %q, want the checkout's HEAD %q", copied.IndexedSHA, head)
	}
	if head := gitInFixture(t, referenceRequest.RootPath)("rev-parse", "HEAD"); want.IndexedSHA != head {
		t.Fatalf("the re-parsed oracle recorded %q, want its own HEAD %q; the comparison is not sound",
			want.IndexedSHA, head)
	}
	if copied.Dirty != want.Dirty {
		t.Fatalf("copied base recorded dirty=%v, want %v", copied.Dirty, want.Dirty)
	}
	if copied.ExtractorVersions != want.ExtractorVersions {
		t.Fatalf("copied base recorded extractor versions %q, want %q",
			copied.ExtractorVersions, want.ExtractorVersions)
	}
	// Generation zero's own row is untouched: the copy read it, it was not
	// moved and it was not restamped.
	zero, found, err := builder.Store.AtGeneration(0).GetRepoIndexState(request.RepoPrefix)
	if err != nil || !found || zero.IndexedSHA != copied.IndexedSHA {
		t.Fatalf("generation zero's own provenance was disturbed: found=%v err=%v state=%+v", found, err, zero)
	}
	if zero.IndexedAt != backdated.IndexedAt {
		t.Fatalf("generation zero's timestamp moved from %d to %d; the copy restamped its SOURCE",
			backdated.IndexedAt, zero.IndexedAt)
	}
	if copied.IndexedAt <= zero.IndexedAt {
		t.Fatalf("the copied generation inherited generation zero's timestamp (%d vs %d): its row says the "+
			"rows landed two hours before the build that wrote them", copied.IndexedAt, zero.IndexedAt)
	}
	if skew := time.Since(time.Unix(copied.IndexedAt, 0)); skew > 10*time.Minute || skew < -time.Minute {
		t.Fatalf("the copied generation stamped %s away from now; the stamp is not this build's",
			skew)
	}
}

// A working tree whose state cannot be READ is not a clean working tree.
//
// repoHeadAndDirty — the indexer's freshness probe — reports a failed `git
// status` as "not dirty", which is the right default for a stamp that must
// never block indexing and the wrong one for a route that publishes a
// committed base out of working-copy rows. Here HEAD is readable and matches,
// the tree matches, generation zero's provenance is clean, and containment
// holds: every other clause is satisfied, so an unreadable status is the only
// thing that can refuse. A corrupt index file is how that is produced without
// touching the tree git reports on.
func TestClaimedDedicatedBaseReparsesWhenTheWorkingTreeStateIsUnreadable(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	indexPath := gitInFixture(t, request.RootPath)("rev-parse", "--git-path", "index")
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(request.RootPath, indexPath)
	}
	if err := os.WriteFile(indexPath, []byte("not a git index"), 0o600); err != nil {
		t.Fatalf("stage a corrupt index at %s: %v", indexPath, err)
	}
	// The fixture must actually produce the asymmetry the clause is about:
	// HEAD readable, status not. This is staged with a RAW git invocation on
	// purpose — asking the production helper whether the hazard is staged
	// would let a helper that swallows the error declare the test
	// inapplicable instead of failing it.
	if _, err := rawGitInFixture(t, request.RootPath, "rev-parse", "HEAD"); err != nil {
		t.Fatalf("the corrupt index made HEAD unreadable too; the clause is no longer isolated: %v", err)
	}
	if _, err := rawGitInFixture(t, request.RootPath, "--no-optional-locks", "status", "--porcelain"); err == nil {
		t.Fatal("git status still succeeds over a corrupt index; the hazard is not staged")
	}
	probe := &copyProbe{store: builder.Store}

	buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "reparse_git_tree" || reason != "the working tree's state could not be read" {
		t.Fatalf("route = %q reason = %q, want the re-parse plan named by the unprovable working tree", route, reason)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy primitive was reached over an unreadable working tree: %+v", probe.calls)
	}
}

// A working tree that is dirty NOW is the case the copy must refuse even when
// generation zero was indexed clean: the working-copy view is continuously
// re-indexed, so a base copied out of it could carry an edit that was never
// committed. Generation zero is indexed first here, so the provenance clause is
// satisfied and the live dirty probe is the only clause that can refuse.
func TestClaimedDedicatedBaseReparsesADirtyWorkingTree(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	const dirty = "package dedicated\n\nfunc DirtyOnly() string { return \"dirty\" }\n"
	if err := os.WriteFile(filepath.Join(request.RootPath, "dirty.go"), []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := &copyProbe{store: builder.Store}

	id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "reparse_git_tree" || reason != "the working tree has uncommitted changes" {
		t.Fatalf("route = %q reason = %q, want the re-parse plan named by the dirty tree", route, reason)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy primitive was reached on a dirty tree: %+v", probe.calls)
	}
	view := materializeClaimedBase(t, builder, request.Identity.GraphID, id)
	if node := view.Reader.GetNode(request.RepoPrefix + "/dirty.go::DirtyOnly"); node != nil {
		t.Fatalf("uncommitted content leaked into the committed base: %+v", node)
	}
	if node := view.Reader.GetNode(request.RepoPrefix + "/base.go::Committed"); node == nil {
		t.Fatal("the re-parse route lost the committed symbol")
	}
}

// Generation zero having INDEXED a dirty tree is a separate refusal from the
// tree being dirty now. The edit is reverted before the build here, so the live
// probe is satisfied and only generation zero's own recorded provenance — the
// dirty bit the indexer stamps at the end of every pass — can refuse.
func TestClaimedDedicatedBaseReparsesWhenGenerationZeroIndexedADirtyTree(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	const dirty = "package dedicated\n\nfunc DirtyOnly() string { return \"dirty\" }\n"
	dirtyPath := filepath.Join(request.RootPath, "dirty.go")
	if err := os.WriteFile(dirtyPath, []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	indexGenerationZero(t, builder, request)
	if err := os.Remove(dirtyPath); err != nil {
		t.Fatal(err)
	}
	probe := &copyProbe{store: builder.Store}

	buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "reparse_git_tree" || reason != "generation zero indexed a dirty working tree" {
		t.Fatalf("route = %q reason = %q, want the re-parse plan named by generation zero's own dirty bit", route, reason)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy primitive was reached over dirty provenance: %+v", probe.calls)
	}
}

// assertGenerationZeroMatchesTheReservedExtractors is a fixture-integrity
// check for every POSITIVE copy-route test.
//
// The route now proves generation zero's rows were produced by the extractor
// versions the reservation claims. If the fixture's identity ever drifts from
// the string an index pass writes, every positive test in this file would
// silently become a version-mismatch refusal test that still passes its own
// assertions about refusing. This fails loudly instead.
func assertGenerationZeroMatchesTheReservedExtractors(t *testing.T, builder *SparseGenerationBuilder, request dedicatedBuilderFixtureRequest) {
	t.Helper()
	state := generationZeroIndexState(t, builder, request)
	if state.ExtractorVersions == "" {
		t.Fatal("generation zero recorded no extractor versions; the copy route can no longer be admitted")
	}
	if state.ExtractorVersions != request.Identity.ExtractorVersions {
		t.Fatalf("the fixture reserves extractor versions %q while an index pass records %q; "+
			"every positive copy-route assertion in this file would be testing a refusal",
			request.Identity.ExtractorVersions, state.ExtractorVersions)
	}
}

// generationZeroIndexState reads the working-copy view's own freshness row.
func generationZeroIndexState(t *testing.T, builder *SparseGenerationBuilder, request dedicatedBuilderFixtureRequest) graph.RepoIndexState {
	t.Helper()
	state, found, err := builder.Store.AtGeneration(0).GetRepoIndexState(request.RepoPrefix)
	if err != nil || !found {
		t.Fatalf("generation zero has no index state: found=%v err=%v", found, err)
	}
	return state
}

// setGenerationZeroIndexState rewrites the working-copy view's freshness row,
// which is how a provenance hazard is staged without touching the tree.
func setGenerationZeroIndexState(t *testing.T, builder *SparseGenerationBuilder, state graph.RepoIndexState) {
	t.Helper()
	writer, ok := graph.Store(builder.Store.AtGeneration(0)).(graph.RepoIndexStateWriter)
	if !ok {
		t.Fatal("the fixture store cannot write repo_index_state; the provenance clauses cannot be staged")
	}
	if err := writer.SetRepoIndexState(state); err != nil {
		t.Fatalf("restate generation zero's index state: %v", err)
	}
}

// Extractor versions are part of the reserved identity AND of generation
// zero's own provenance row, written from the same snapshot. They disagree
// exactly in the window a version bump opens: generation zero still holds the
// rows the OLD extractors produced, it is still clean at HEAD, the tree still
// matches and containment still holds — every other clause passes. Copying
// there would publish old rows into a generation whose catalog row claims the
// new versions, and the reuse guards compare precisely that identity, so the
// mislabelled base would then be served as current.
//
// Both halves are refusals: a row that records no versions at all cannot prove
// anything either.
func TestClaimedDedicatedBaseReparsesWhenGenerationZeroUsedOtherExtractorVersions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		reason   string
	}{
		{
			name:     "bumped",
			recorded: `{"go":9999,"post_extraction_policy":9999}`,
			reason:   "generation zero was indexed by different extractor versions",
		},
		{
			name:     "unrecorded",
			recorded: "",
			reason:   "generation zero recorded no extractor versions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder, request, claim, logs := claimedCopyFixture(t)
			indexGenerationZero(t, builder, request)
			assertGenerationZeroMatchesTheReservedExtractors(t, builder, request)
			// Only the versions move: the commit, the clean bit and the rows
			// generation zero carries are untouched, so every other clause is
			// still satisfied and this one is isolated.
			state := generationZeroIndexState(t, builder, request)
			if tc.recorded == request.Identity.ExtractorVersions {
				t.Fatalf("the staged versions %q are the reserved ones; the case is not staged", tc.recorded)
			}
			state.ExtractorVersions = tc.recorded
			setGenerationZeroIndexState(t, builder, state)
			probe := &copyProbe{store: builder.Store}

			id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
				Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
				ProjectID: request.ProjectID, CopySource: probe,
			})

			route, reason := claimedRoute(t, logs)
			if route != "reparse_git_tree" || reason != tc.reason {
				t.Fatalf("route = %q reason = %q, want the re-parse plan named by %q", route, reason, tc.reason)
			}
			if len(probe.calls) != 0 {
				t.Fatalf("the copy primitive was reached over foreign extractor provenance: %+v", probe.calls)
			}
			// The re-parse is not a degraded answer: it extracts with the
			// running process's extractors, which is what makes the published
			// base's identity true.
			view := materializeClaimedBase(t, builder, request.Identity.GraphID, id)
			if node := view.Reader.GetNode(request.RepoPrefix + "/base.go::Committed"); node == nil {
				t.Fatal("the re-parse route lost the committed symbol")
			}
		})
	}
}

// A checkout whose committed tree is not the tree the reservation names is the
// second refusal. Generation zero is clean, current and consistent here — it
// simply describes a different tree, so its rows are not this base's payload.
//
// The fixture commits the second revision BEFORE indexing generation zero on
// purpose: that leaves the head/provenance clause satisfied, so the only clause
// that can refuse is the tree comparison.
func TestClaimedDedicatedBaseReparsesWhenGenerationZeroCoversAnotherTree(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	git := gitInFixture(t, request.RootPath)
	const added = "package dedicated\n\nfunc Later() string { return \"later\" }\n"
	if err := os.WriteFile(filepath.Join(request.RootPath, "later.go"), []byte(added), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "later.go")
	git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-qm", "a revision the reservation does not name")
	indexGenerationZero(t, builder, request)
	probe := &copyProbe{store: builder.Store}

	// The reservation still names the ORIGINAL tree, which is exactly the
	// mismatch the predicate has to notice.
	buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "reparse_git_tree" || reason != "generation zero covers a different tree" {
		t.Fatalf("route = %q reason = %q, want the re-parse plan named by the tree mismatch", route, reason)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy primitive was reached over a different tree: %+v", probe.calls)
	}
}

// A HEAD that moved after generation zero was indexed is the third refusal,
// and it is distinct from the tree comparison: an empty commit leaves the tree
// identical while making generation zero's recorded provenance obsolete. What
// generation zero READ is no longer what the checkout says it is, so the rows
// can no longer be certified against the commit they came from.
func TestClaimedDedicatedBaseReparsesAfterTheWorkingTreeMovedPastGenerationZero(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	git := gitInFixture(t, request.RootPath)
	git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "same tree, later commit")
	if got := git("rev-parse", "HEAD^{tree}"); got != request.Identity.TreeOID {
		t.Fatalf("the empty commit changed the tree to %q; the fixture no longer isolates the head clause", got)
	}
	probe := &copyProbe{store: builder.Store}

	buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "reparse_git_tree" || reason != "the working tree moved since generation zero was indexed" {
		t.Fatalf("route = %q reason = %q, want the re-parse plan named by the head move", route, reason)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy primitive was reached past a HEAD move: %+v", probe.calls)
	}
}

// A reservation that already carries rows is the fourth refusal. The copy
// materialises a whole generation; it does not complete a partial one, and the
// route it takes skips the index pass that would otherwise re-derive them.
func TestClaimedDedicatedBaseReparsesWhenTheReservationAlreadyCarriesPayload(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	reserved, err := builder.Store.AtManagedGeneration(claim.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	leftover := request.RepoPrefix + "/leftover.go"
	if err := reserved.AddBatchChecked([]*graph.Node{
		{ID: leftover, Name: "leftover.go", Kind: graph.KindFile, FilePath: leftover, RepoPrefix: request.RepoPrefix},
	}, nil); err != nil {
		t.Fatal(err)
	}
	probe := &copyProbe{store: builder.Store}

	buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "reparse_git_tree" || reason != "the reserved generation already carries payload" {
		t.Fatalf("route = %q reason = %q, want the re-parse plan named by the partial reservation", route, reason)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy primitive was reached over a partial reservation: %+v", probe.calls)
	}
}

// Generation zero carrying payload the committed tree has no bytes for — an
// ignored-but-indexed file is the ordinary way this happens — is the third
// refusal. git status says nothing about such a path, so cleanliness alone is
// not a sufficient predicate and the inventory check is what closes it.
func TestClaimedDedicatedBaseReparsesWhenGenerationZeroCarriesUntrackedPayload(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	// Seed generation zero with a file the committed tree does not contain,
	// the way an ignored source file reaches the working-copy index.
	ignored := request.RepoPrefix + "/ignored.go"
	builder.Store.AtGeneration(0).AddBatch([]*graph.Node{
		{ID: ignored, Name: "ignored.go", Kind: graph.KindFile, FilePath: ignored, RepoPrefix: request.RepoPrefix},
	}, nil)
	if err := builder.Store.AtGeneration(0).SetFileMetas(request.RepoPrefix, []graph.FileMetaRow{
		{FilePath: ignored, ContentHash: "ignored", Size: 1},
	}); err != nil {
		t.Fatal(err)
	}
	probe := &copyProbe{store: builder.Store}

	buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
	})

	route, reason := claimedRoute(t, logs)
	if route != "reparse_git_tree" {
		t.Fatalf("route = %q (%s), want the re-parse plan when generation zero reaches outside the tree", route, reason)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("the copy primitive was reached over out-of-tree payload: %+v", probe.calls)
	}
}

// The production store IS the copy primitive, and the production entry point
// reaches it without being handed one.
//
// This is the wiring assertion the route's tests could not make while no type
// implemented CopyPayloadGeneration: every one of them supplied a probe, so a
// green suite said nothing about whether a daemon would ever take the route.
// Here the request carries neither CopySource nor BulkLoad, so both are
// resolved from the builder's own store — which is what buildObservedClaim
// constructs — and the logged route is the one a real build takes.
func TestTheProductionStoreIsTheGenerationCopyPrimitive(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)

	copier, hasCopier := builder.generationCopier(ClaimedDedicatedBaseRequest{})
	if !hasCopier {
		t.Fatal("the builder's own store does not satisfy GenerationPayloadCopier, so every production " +
			"build re-parses and the copy route is dead")
	}
	if copier != any(builder.Store) {
		t.Fatalf("generationCopier resolved %T, want the builder's own store", copier)
	}

	id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID,
	})

	route, reason := claimedRoute(t, logs)
	if route != "copy_generation_zero" {
		t.Fatalf("route = %q (%s), want the copy plan from a store that carries the primitive", route, reason)
	}
	view := materializeClaimedBase(t, builder, request.Identity.GraphID, id)
	if node := view.Reader.GetNode(request.RepoPrefix + "/base.go::Committed"); node == nil {
		t.Fatal("the store's own copy lost the committed symbol")
	}
	gotNodes, gotEdges := claimedBasePayload(view.Reader)
	if len(gotNodes) == 0 || len(gotEdges) == 0 {
		t.Fatalf("the store's own copy composed empty: %d nodes, %d edges", len(gotNodes), len(gotEdges))
	}

	// The gate-1 oracle against the REAL primitive: an independent fixture over
	// byte-identical content, built through the re-parse route, must compose
	// the same payload. The probe-driven oracle above
	// (TestClaimedDedicatedBaseCopiesGenerationZeroOnACleanMatchingTree) says
	// the ROUTE is sound; this one says the store's SQL is.
	reference, referenceRequest, referenceClaim, referenceLogs := claimedCopyFixture(t)
	indexGenerationZero(t, reference, referenceRequest)
	const uncommitted = "package dedicated\n\nfunc UncommittedOnly() string { return \"uncommitted\" }\n"
	if err := os.WriteFile(filepath.Join(referenceRequest.RootPath, "uncommitted.go"), []byte(uncommitted), 0o600); err != nil {
		t.Fatal(err)
	}
	referenceID := buildClaimedBase(t, reference, ClaimedDedicatedBaseRequest{
		Claim: referenceClaim, RootPath: referenceRequest.RootPath,
		WorkspaceID: referenceRequest.WorkspaceID, ProjectID: referenceRequest.ProjectID,
	})
	if route, reason := claimedRoute(t, referenceLogs); route != "reparse_git_tree" {
		t.Fatalf("reference route = %q (%s), want the re-parse plan", route, reason)
	}
	referenceView := materializeClaimedBase(t, reference, referenceRequest.Identity.GraphID, referenceID)
	wantNodes, wantEdges := claimedBasePayload(referenceView.Reader)
	if strings.Join(gotEdges, "\n") != strings.Join(wantEdges, "\n") {
		t.Fatalf("the store's copy produced different edges from a re-parse:\ngot=%v\nwant=%v", gotEdges, wantEdges)
	}
	if strings.Join(gotNodes, "\n") != strings.Join(wantNodes, "\n") {
		assertOnlyBuiltinStampingDiffers(t, view.Reader, referenceView.Reader)
	}

	// The symbol FTS projection is part of what a generation carries, and it is
	// the one the copy has to re-address rather than carry: a docid names one
	// row of one shared virtual table. A copied base whose corpus were empty
	// would answer every symbol search with nothing while every other
	// assertion here stayed green.
	copied, err := builder.Store.AtGeneration(id).SymbolFTSCount()
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := reference.Store.AtGeneration(referenceID).SymbolFTSCount()
	if err != nil {
		t.Fatal(err)
	}
	if copied == 0 || copied != reparsed {
		t.Fatalf("the copied base carries %d symbol-FTS documents against the re-parsed base's %d", copied, reparsed)
	}
}

// The absent-primitive clause is still a clause: it is the first thing the
// predicate asks and it costs nothing at all, so a store that never gains the
// primitive pays exactly what it paid before this route existed. It is asserted
// on the predicate directly because no production store can be made to lack the
// method any more.
func TestClaimedBaseCopyPlanRefusesWithoutACopyPrimitive(t *testing.T) {
	builder, request, claim, _ := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	handle, err := builder.Store.AtManagedGeneration(claim.GenerationID)
	if err != nil {
		t.Fatal(err)
	}

	take, _, reason := builder.claimedBaseCopyPlan(context.Background(), handle, request.RootPath,
		request.RepoPrefix, claim.Desire.Identity.TreeOID, claim.Desire.Identity.ExtractorVersions, false)
	if take || reason != "no generation copy primitive on this store" {
		t.Fatalf("copy plan = (%v, %q), want the short-circuit on the absent primitive", take, reason)
	}
	// The control: the same inputs with a primitive present take the route, so
	// the refusal above is the primitive clause and not some other one.
	if take, _, reason := builder.claimedBaseCopyPlan(context.Background(), handle, request.RootPath,
		request.RepoPrefix, claim.Desire.Identity.TreeOID, claim.Desire.Identity.ExtractorVersions, true); !take {
		t.Fatalf("copy plan with a primitive = (%v, %q), want the copy route so the clause above is isolated", take, reason)
	}
}

// The bulk-load bracket covers BOTH routes of a claimed base build.
//
// It used to cover the copy route only, because an index pass's FlushBulk
// adopted and closed any window it found — taking it from its owner mid-payload
// and charging the pass an inline TRUNCATE. FlushBulk now leaves a
// generation-scoped window alone (pinned by
// TestAnIndexPassFlushLeavesAGenerationBulkWindowToItsOwner), so the re-parse
// route gets the same shape the copy route does.
//
// Both arms drive the production entry point and assert three things: the
// window is opened exactly once on the RESERVED generation, it is still open at
// the pre-publication boundary — which is AFTER the payload write, the
// enrichment and the masks, so it really did span the write — and it is closed
// exactly once.
func TestClaimedDedicatedBaseBracketsBothRoutes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty bool
		route string
	}{
		{name: "copy", route: "copy_generation_zero"},
		{name: "reparse", dirty: true, route: "reparse_git_tree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder, request, claim, logs := claimedCopyFixture(t)
			indexGenerationZero(t, builder, request)
			if tc.dirty {
				// An uncommitted edit is the cheapest way to send the build
				// down the re-parse route without changing anything else: the
				// re-parse reads the committed tree, so the payload it writes
				// is the same one the copy arm moves.
				const dirty = "package dedicated\n\nfunc DirtyOnly() string { return \"dirty\" }\n"
				if err := os.WriteFile(filepath.Join(request.RootPath, "dirty.go"), []byte(dirty), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			loader := &bulkLoadProbe{}
			probe := &copyProbe{store: builder.Store, bulk: loader}
			openAtPrePublish := make([]bool, 0, 1)
			id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
				Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
				ProjectID: request.ProjectID, BulkLoad: loader, CopySource: probe,
				PrePublish: func(context.Context, int64) error {
					openAtPrePublish = append(openAtPrePublish, loader.isOpen())
					return nil
				},
			})
			if route, reason := claimedRoute(t, logs); route != tc.route {
				t.Fatalf("route = %q (%s), want %q", route, reason, tc.route)
			}
			// The logged route is not the only witness: the copy primitive
			// must have been reached on the copy arm and left alone otherwise.
			if want := map[bool]int{false: 1, true: 0}[tc.dirty]; len(probe.calls) != want {
				t.Fatalf("copy primitive reached %d time(s), want %d: %+v", len(probe.calls), want, probe.calls)
			}
			begun, ended := loader.counts()
			if len(begun) != 1 || ended != 1 {
				t.Fatalf("bulk windows begun=%v closed=%d, want exactly one begin and one close on the %s route",
					begun, ended, tc.name)
			}
			if begun[0] != id {
				t.Fatalf("bulk load begun on generation %d, want the reserved %d", begun[0], id)
			}
			if len(openAtPrePublish) != 1 || !openAtPrePublish[0] {
				t.Fatalf("the window was not open at the pre-publication boundary (%v), so it did not span "+
					"the %s route's payload write", openAtPrePublish, tc.name)
			}
			if tc.dirty {
				return
			}
			if len(probe.openDuringCopy) != 1 || !probe.openDuringCopy[0] {
				t.Fatalf("the window was not open while the copy ran: %v", probe.openDuringCopy)
			}
		})
	}
}

// A failing payload write must close the window rather than leave the store
// holding a pinned writer with an unbounded log.
func TestClaimedDedicatedBaseClosesTheBulkLoadWhenTheCopyFails(t *testing.T) {
	builder, request, claim, _ := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	loader := &bulkLoadProbe{}
	broken := errors.New("copy refused by the store")

	_, _, err := builder.BuildClaimedDedicatedBase(context.Background(), ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, BulkLoad: loader, CopySource: &copyProbe{store: builder.Store, fail: broken},
	})
	if !errors.Is(err, broken) {
		t.Fatalf("build error = %v, want the copy failure", err)
	}
	if begun, ended := loader.counts(); len(begun) != 1 || ended != 1 {
		t.Fatalf("bulk load begun=%v closed=%d; want one begin and one close", begun, ended)
	}
}

// A refused begin must leave the copy alone: no close for a window that was
// never opened, and one ordinary published base.
func TestClaimedDedicatedBaseRunsWithoutARefusedBulkLoadBracket(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	loader := &bulkLoadProbe{refuse: true}
	probe := &copyProbe{store: builder.Store, bulk: loader}

	id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, BulkLoad: loader, CopySource: probe,
	})
	if route, reason := claimedRoute(t, logs); route != "copy_generation_zero" {
		t.Fatalf("route = %q (%s), want the copy plan so the refusal is actually exercised", route, reason)
	}
	if begun, ended := loader.counts(); len(begun) != 0 || ended != 0 {
		t.Fatalf("a refused bracket was still driven: begun=%v closed=%d", begun, ended)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("a refused bracket changed the write: copy reached %d time(s)", len(probe.calls))
	}
	if view := materializeClaimedBase(t, builder, request.Identity.GraphID, id); len(view.Reader.AllNodes()) == 0 {
		t.Fatal("a refused bracket lost the payload")
	}
}

// Only the physical flight leader may hold the window: it is a store-level
// singleton, and a coalescing follower that won it would pin the writer across
// its wait while the goroutine that actually writes rows was refused one.
//
// Taking the bracket inside the payload preparation is what guarantees that —
// the flight runs a preparation for the leader only — and this is the test that
// notices if it moves back out.
//
// The witness is CONTENTION, not the begin/end totals. Both racers reach route
// selection (each coalescing build evaluates its own plan before joining the
// flight), so a bracket taken around the whole build — anywhere before
// JoinPayloadBuildFlight — has both goroutines asking the store for the
// window. The store hands it to whichever asks first and refuses the other,
// which leaves the totals at one begin and one close however the race lands,
// and leaves "a window was open while the copy ran" true even when the window
// is held by the follower across its wait. Only the refused ask is visible,
// and only when the bracket is outside the leader-only preparation: with it
// inside, exactly one goroutine ever calls Begin.
func TestClaimedDedicatedBaseConcurrentCopyTakesOneLeaderWindow(t *testing.T) {
	builder, request, claim, _ := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	loader := &bulkLoadProbe{}
	probe := &copyProbe{store: builder.Store, bulk: loader}
	build := ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, BulkLoad: loader, CopySource: probe,
	}

	const racers = 2
	start := make(chan struct{})
	ids := make([]int64, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], _, errs[i] = builder.BuildClaimedDedicatedBase(context.Background(), build)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		if ids[i] != claim.GenerationID {
			t.Fatalf("racer %d built generation %d, want the reserved %d", i, ids[i], claim.GenerationID)
		}
	}
	begun, ended := loader.counts()
	if len(begun) != 1 || ended != 1 {
		t.Fatalf("two coalescing builds drove begun=%v closed=%d, want exactly one begin and one close", begun, ended)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("the copy ran %d times for one physical build: %+v", len(probe.calls), probe.calls)
	}
	if len(probe.openDuringCopy) != 1 || !probe.openDuringCopy[0] {
		t.Fatalf("the goroutine that copied did not hold the window: %v", probe.openDuringCopy)
	}
	if got := loader.contentions(); got != 0 {
		t.Fatalf("%d goroutine(s) asked for the window while another held it: the bracket is no longer "+
			"inside the leader-only payload preparation, so a coalescing follower can pin the writer "+
			"across its wait while the leader writes unbracketed", got)
	}
}

// gitInFixture runs git in a fixture checkout with the same hermetic
// environment privateDedicatedBuilderFixture uses.
func gitInFixture(t *testing.T, root string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		out, err := rawGitInFixture(t, root, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return out
	}
}

// rawGitInFixture is gitInFixture without the fatal: it returns git's own
// verdict, for the cases that are ABOUT git failing.
func rawGitInFixture(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// assertOnlyBuiltinStampingDiffers pins the ONE observed difference between a
// base copied out of generation zero and a base re-parsed from the same tree.
//
// A pathless builtin sentinel node (KindBuiltin, no file path) is not produced
// by extraction at all: it is materialised lazily so that ::builtin:: edge
// targets are not orphans. Which layer materialises it decides whether it
// carries the build's workspace/project slugs. The generation-scoped pass holds
// its corpus in memory and stamps every node in it before the drain; the cold
// streamed index of generation zero materialises the same sentinel inside the
// store's own batch path, after stamping, so generation zero's copy of it is
// unstamped. A verbatim row copy therefore inherits generation zero's blank
// slugs where a re-parse would have stamped them.
//
// The delta is named here rather than excluded from the comparison so that it
// stays visible: everything else — every file-scoped node, every field, every
// edge — must match exactly, and if the sentinel's stamping is ever fixed on
// either side this assertion is what says so.
func assertOnlyBuiltinStampingDiffers(t *testing.T, copied, reparsed graph.Reader) {
	t.Helper()
	byID := make(map[string]*graph.Node)
	for _, node := range reparsed.AllNodes() {
		byID[node.ID] = node
	}
	stampingDeltas := 0
	for _, node := range copied.AllNodes() {
		want, found := byID[node.ID]
		if !found {
			t.Fatalf("copied base carries a node the re-parsed base does not: %q", node.ID)
		}
		delete(byID, node.ID)
		if node.WorkspaceID == want.WorkspaceID && node.ProjectID == want.ProjectID {
			if !sameNodeIgnoringStamping(node, want) {
				t.Fatalf("copied node %q differs from the re-parsed node:\ngot=%#v\nwant=%#v", node.ID, node, want)
			}
			continue
		}
		if node.Kind != graph.KindBuiltin || node.FilePath != "" {
			t.Fatalf("copied node %q has different workspace/project stamping and is not a pathless builtin sentinel:\ngot=%#v\nwant=%#v",
				node.ID, node, want)
		}
		if !sameNodeIgnoringStamping(node, want) {
			t.Fatalf("copied builtin sentinel %q differs beyond its stamping:\ngot=%#v\nwant=%#v", node.ID, node, want)
		}
		stampingDeltas++
	}
	if len(byID) != 0 {
		t.Fatalf("the re-parsed base carries %d node(s) the copy does not: %v", len(byID), byID)
	}
	if stampingDeltas == 0 {
		t.Fatal("the payloads differed but no builtin-sentinel stamping delta explains it")
	}
}

// sameNodeIgnoringStamping compares two nodes on every rendered field except
// the workspace/project slugs.
func sameNodeIgnoringStamping(a, b *graph.Node) bool {
	left, right := *a, *b
	left.WorkspaceID, right.WorkspaceID = "", ""
	left.ProjectID, right.ProjectID = "", ""
	return renderClaimedNode(&left) == renderClaimedNode(&right)
}

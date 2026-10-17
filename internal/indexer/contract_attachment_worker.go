package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/fixtures"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// ContractFollowupFile is one member of a complete accepted source census.
// Path is the indexed graph path; fingerprints/policy bind historical bytes.
type ContractFollowupFile struct {
	RepoPrefix, WorkspaceID, ProjectID        string
	Path, Language, SourceFingerprint, Policy string
}

type ContractAcceptedSource struct {
	Bytes                     []byte
	SourceFingerprint, Policy string
}

// ContractFollowupCoreFile contains complete selected declaration and outgoing
// edge evidence. The caller must use checked reads and reject truncation.
type ContractFollowupCoreFile struct {
	Nodes []*graph.Node
	Edges []*graph.Edge
}

// ContractFollowupSnapshot is captured before queue admission under the actual
// selected view handoff. Files must be the complete accepted namespace census,
// not only Work's changed files. ReadAccepted serves accepted transformed
// extractor bytes with their exact source/policy proof; it never falls back to current
// filesystem bytes. The runner owns Release on every exit.
type ContractFollowupSnapshot struct {
	Key    graph.ContractAttachmentKey
	Core   graph.Reader
	Inputs []graph.ContractInputWitness
	Work   []graph.ContractWork
	Files  []ContractFollowupFile
	// Excluded names the census paths whose accepted receipt is a failure
	// envelope: they are not live, so no file of the job reads them.
	Excluded []string
	// InFlight names the census paths captured while a mutation had their
	// core-write window open. Only a job that takes stamp scans (Scan) can
	// prove reads of them; a snapshot without Scan that names any is refused.
	InFlight []string
	// RepoConfigs is captured immutable extractor/transform policy authority.
	RepoConfigs map[string]config.IndexConfig
	// RepoExtractionOptions freezes repository parser options for baseline replay.
	RepoExtractionOptions map[string]parser.ExtractionOptions
	// TrackedRepoModules freezes accepted companion module identity.
	TrackedRepoModules map[string]string
	ReadAccepted       func(context.Context, ContractFollowupFile) (ContractAcceptedSource, error)
	ReadCoreFile       func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error)
	// Scan takes a stamp scan of the selected chain: every row and the live
	// census for nil paths, else the named paths. Nil means the captured
	// inputs cannot move (a test's frozen cohort): every census file is
	// accepted at its captured source fingerprint.
	Scan func(context.Context, []string) (ContractFollowupScan, error)
	// CensusFile classifies a path that joined the census during the job.
	CensusFile func(context.Context, string) (ContractFollowupFile, bool, error)
	// ReadPinned returns the transformed bytes whose fingerprint is the given
	// receipt source fingerprint, from the accepted-bytes cache or the disk,
	// and errContractSourceMoved when neither holds them.
	ReadPinned func(context.Context, ContractFollowupFile, string) ([]byte, error)
	// Seeds re-derives one repository's enrichment seeds (DI owner edges,
	// Spring readers, Java methods) from the selected core. supported=false
	// asks the enrichment round to load every live file of the repository.
	Seeds func(context.Context, string) (nodes []*graph.Node, edges []*graph.Edge, supported bool, err error)
	// ReceiptEvents counts the selected chain's ended receipt writes and
	// settles, with a channel closed by the next one: a job's retry wakes on
	// them as well as on Published.
	ReceiptEvents func() (uint64, <-chan struct{})
	// ReceiptWindowClose returns the earliest recorded close of an exact
	// waited-on window, for measurement only. Missing history is unknown.
	ReceiptWindowClose func(map[string]ContractReceiptStamp) (time.Time, bool)
	// WindowLastCloses reports the primary paths last known closes before the
	// proof ended, plus paths whose bounded history has no such close.
	WindowLastCloses func([]string, time.Time) ([]store_sqlite.ContractCoreWindowClose, []string)
	// ReceiptContents reads the contract content of each stamped path's
	// receipt row and of its accepted predecessor, keyed by receipt
	// fingerprint. Nil means no move is known to keep a file's contract
	// content.
	ReceiptContents func(context.Context, map[string]ContractReceiptStamp) (map[string]string, error)
	// Opaque overflow sources require full analysis with no retained delta state.
	opaqueContractInputs bool
	deltaReceipts        func(context.Context, map[string]ContractReceiptStamp) (map[string]contractDeltaReceipt, error)
	// WindowGaps returns the quiet gaps a writer left between the core-write
	// windows of the named paths that opened at or after since. Nil records
	// none.
	WindowGaps func(paths []string, since time.Time) []time.Duration
	// Suspect reports paths whose open core-write window a job was refused
	// on, unmoved, for a repair period. Nil reports nothing.
	Suspect func(paths []string)
	Release func()
}

// errContractSourceMoved reports that a file's accepted bytes are no longer
// readable: the disk moved past the stamped receipt and the accepted-bytes
// cache no longer holds them. The read is unproven, not failed.
var errContractSourceMoved = errors.New("contract followup: accepted source moved")

// ContractFollowupRequest receives an installer-reserved Building managed
// payload. Catalog retains the selected core generation used for CAS; it is
// not an actor-latest handle. Leases is the materializer's shared lease manager.
// Yield lets the existing scheduler defer to interactive load between files.
type ContractFollowupRebuildReason string

const (
	ContractFollowupColdBaseline    ContractFollowupRebuildReason = "cold_baseline"
	ContractFollowupUnknownInterval ContractFollowupRebuildReason = "unknown_interval"
	ContractFollowupChangedBoundary ContractFollowupRebuildReason = "changed_boundary"
)

type ContractFollowupRequest struct {
	Snapshot ContractFollowupSnapshot
	// Explicit full rebuild reason makes boundary-edit cost visible. Unchanged
	// identity reuses the exact attachment upstream and must schedule no worker.
	RebuildReason          ContractFollowupRebuildReason
	Payload, Catalog       *store_sqlite.Store
	Registry               *parser.Registry
	Config                 config.IndexConfig
	Logger                 *zap.Logger
	Leases                 *graphview.LeaseManager
	WorkspaceID, ProjectID string
	RepoConfigs            map[string]config.IndexConfig
	ScratchParent          string
	Yield                  func(context.Context) error
	// PublishedEpoch counts core acceptances announced to the runtime, and
	// WaitPublished blocks until the count passes since or until until. A
	// job retries an unproven read only on a new acceptance. Nil retries
	// after a short pause.
	PublishedEpoch func() uint64
	WaitPublished  func(context.Context, uint64, time.Time) bool
	// OtherActorWaiting permits a fill to return a resumable checkpoint.
	OtherActorWaiting func() bool
	// convergenceHooks are test seams at the job's interleaving points.
	convergenceHooks *contractConvergenceHooks
	delta            *contractDeltaSession
	coverageWork     []graph.ContractWork
}

type ContractFollowupReport struct {
	Mode, CacheReason                                string
	ChangedFiles                                     []string
	Files, Records, Nodes, Edges, Shapes             int
	SourceReads, SourceBytes                         int
	RebuildReason                                    ContractFollowupRebuildReason
	ExtractDuration, EnrichDuration, PublishDuration time.Duration
	PayloadGeneration                                int64
	// UnusedPayloadGeneration remains caller-owned after a duplicate wins.
	UnusedPayloadGeneration    int64
	ScratchNodes, ScratchEdges int
	ScratchBytes               int64
	PrepareDuration            time.Duration
	SymbolDocuments            int
	FTSDuration                time.Duration
	Published                  bool
	// Phase ledger of the job's convergence: census and stamp scans, scratch
	// evidence opens, proof rounds and payload assembly. Iterations and
	// Rounds count the convergence loop's passes and proof rounds.
	CensusDuration, StampsDuration, ScratchDuration time.Duration
	RoundsDuration, PayloadDuration                 time.Duration
	Iterations, Rounds                              int
	// EnrichRuns counts enrichment rounds that ran to completion and
	// EnrichReruns those after the first a recorded input change forced;
	// EnrichRechecks counts fence re-checks of the round's recorded inputs
	// and EnrichRecheckReads the store reads they asked again (census reads
	// are always asked); Revalidated counts units re-proven by asking their
	// reads again.
	EnrichRuns, EnrichReruns, Revalidated int
	EnrichRechecks, EnrichRecheckReads    int
	// ContentKept counts observed receipt moves whose contract content was
	// unchanged: they make no unit and no enrichment round stale.
	ContentKept int
	// NeedSum sums, over the iterations after the first, what each needed of
	// a writer's quiet gap (its re-asks' p90 lead and span, its enrichment
	// re-check and its fence); NeedIterations counts them. WindowGaps are
	// the quiet gaps between the core-write windows of the paths those
	// iterations waited on.
	NeedSum        time.Duration
	NeedIterations int
	WindowGaps     []time.Duration
}

// ContractFollowupTarget reserves one independently published repository payload.
// Catalog is the exact selected receiver handle, not an actor-latest lookup.
type ContractFollowupTarget struct {
	Key              graph.ContractAttachmentKey
	Work             []graph.ContractWork
	Payload, Catalog *store_sqlite.Store
}

// ContractFollowupBatchRequest uses one proof-bound cohort and source lifetime.
// Snapshot.Key/Work are ignored; target keys and work remain independent.
type ContractFollowupBatchRequest struct {
	Snapshot               ContractFollowupSnapshot
	Targets                []ContractFollowupTarget
	RebuildReason          ContractFollowupRebuildReason
	Registry               *parser.Registry
	Config                 config.IndexConfig
	Logger                 *zap.Logger
	Leases                 *graphview.LeaseManager
	WorkspaceID, ProjectID string
	RepoConfigs            map[string]config.IndexConfig
	ScratchParent          string
	Yield                  func(context.Context) error
	PublishedEpoch         func() uint64
	WaitPublished          func(context.Context, uint64, time.Time) bool
	OtherActorWaiting      func() bool
	convergenceHooks       *contractConvergenceHooks
	delta                  *contractDeltaSession
}

type ContractFollowupTargetResult struct {
	Key    graph.ContractAttachmentKey
	Report ContractFollowupReport
	Err    error
}

type ContractFollowupBatchReport struct {
	Publication *ContractPublishedCohort
	Cohort      ContractFollowupReport
	Targets     []ContractFollowupTargetResult
}

type contractFollowupPrepared struct {
	publications []store_sqlite.ContractAttachmentPublishTarget
	proof        *ContractPublishedCohort
	coverage     contractCoveredWork
	work         []graph.ContractWork
	registry     *contracts.Registry
	evidence     *contractFollowupEvidence
	report       ContractFollowupReport
	scratch      *store_sqlite.Store
	scratchDir   string
}

// RunContractFollowupBatch extracts and enriches the captured cohort once.
// Each target keeps its own payload and immutable work; publication commits
// every target together after all payloads and coverage candidates are ready.
func RunContractFollowupBatch(ctx context.Context, req ContractFollowupBatchRequest) (batch ContractFollowupBatchReport, err error) {
	if req.Snapshot.Release != nil {
		defer req.Snapshot.Release()
	}
	if len(req.Targets) == 0 || req.Snapshot.Release == nil || req.Leases == nil {
		return batch, fmt.Errorf("contract followup batch: incomplete target handoff")
	}
	seen := make(map[graph.ContractAttachmentKey]bool)
	payloads := make(map[int64]bool)
	for _, target := range req.Targets {
		if target.Payload == nil || target.Catalog == nil || target.Payload.ViewGeneration() <= 0 || seen[target.Key] || payloads[target.Payload.ViewGeneration()] {
			return batch, fmt.Errorf("contract followup batch: invalid or duplicate target")
		}
		seen[target.Key] = true
		payloads[target.Payload.ViewGeneration()] = true
		lease := req.Leases.Acquire(target.Payload.ViewGeneration())
		defer lease.Release()
	}
	if req.delta != nil {
		for _, target := range req.Targets {
			req.delta.membershipUnknown = req.delta.membershipUnknown || contractDeltaMembershipUnknown(target.Work)
		}
	}
	prepared := &contractFollowupPrepared{}
	for _, target := range req.Targets {
		prepared.work = append(prepared.work, target.Work...)
	}
	defer func() {
		if cleanupErr := prepared.close(); err == nil {
			err = cleanupErr
		}
	}()
	for _, target := range req.Targets {
		batch.Targets = append(batch.Targets, ContractFollowupTargetResult{Key: target.Key})
	}
	fail := func(cause error) (ContractFollowupBatchReport, error) {
		for i := range batch.Targets {
			batch.Targets[i].Err = cause
		}
		batch.Cohort = prepared.report
		return batch, cause
	}
	for i, target := range req.Targets {
		snap := req.Snapshot
		snap.Key, snap.Work, snap.Release = target.Key, target.Work, func() {}
		single := ContractFollowupRequest{Snapshot: snap, RebuildReason: req.RebuildReason, Payload: target.Payload, Catalog: target.Catalog, Registry: req.Registry, Config: req.Config, Logger: req.Logger, Leases: req.Leases, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID, RepoConfigs: req.RepoConfigs, ScratchParent: req.ScratchParent, Yield: req.Yield,
			PublishedEpoch: req.PublishedEpoch, WaitPublished: req.WaitPublished, OtherActorWaiting: req.OtherActorWaiting, convergenceHooks: req.convergenceHooks, delta: req.delta}
		report, targetErr := buildContractFollowupPrepared(ctx, single, prepared)
		batch.Targets[i].Report = report
		if targetErr != nil {
			return fail(targetErr)
		}
	}
	// Every payload and exact covered-work candidate is ready before the
	// store acquires its write gate. A refusal publishes no target.
	started := time.Now()
	results, publishErr := prepared.publications[0].Store.PublishContractAttachmentsWithInputsContext(ctx, prepared.publications, started.Unix())
	if publishErr != nil {
		return fail(publishErr)
	}
	elapsed := time.Since(started)
	for i, result := range results {
		acceptContractPublication(&batch.Targets[i].Report, result, elapsed)
	}
	prepared.report.PublishDuration, prepared.report.Published = elapsed, true
	batch.Publication = contractPublishedResults(prepared.proof, prepared.publications, results)

	batch.Cohort = prepared.report
	return batch, nil
}

// RunContractFollowup performs no ordinary core indexing or route publication.
// All legacy contract enrichment operates on private disk-backed evidence.
func RunContractFollowup(ctx context.Context, req ContractFollowupRequest) (report ContractFollowupReport, err error) {
	prepared := &contractFollowupPrepared{}
	defer func() {
		if cleanupErr := prepared.close(); err == nil {
			err = cleanupErr
		}
	}()
	return runContractFollowupPrepared(ctx, req, prepared)
}

func runContractFollowupPrepared(ctx context.Context, req ContractFollowupRequest, prepared *contractFollowupPrepared) (report ContractFollowupReport, err error) {
	// Keep the selected source lifetime and reservation lease through the
	// transaction, including a validated duplicate's winning-payload lookup.
	if req.Snapshot.Release != nil {
		defer req.Snapshot.Release()
		req.Snapshot.Release = func() {}
	}
	if req.Leases != nil && req.Payload != nil && req.Payload.ViewGeneration() > 0 {
		lease := req.Leases.Acquire(req.Payload.ViewGeneration())
		defer lease.Release()
	}
	before := len(prepared.publications)
	report, err = buildContractFollowupPrepared(ctx, req, prepared)
	if err != nil {
		return report, err
	}
	candidates := prepared.publications[before:]
	started := time.Now()
	results, err := req.Catalog.PublishContractAttachmentsWithInputsContext(ctx, candidates, started.Unix())
	if err != nil {
		return report, err
	}
	acceptContractPublication(&report, results[0], time.Since(started))
	return report, nil
}

func acceptContractPublication(report *ContractFollowupReport, result store_sqlite.ContractAttachmentPublication, elapsed time.Duration) {
	report.PayloadGeneration = result.PayloadGeneration
	report.UnusedPayloadGeneration = result.UnusedPayloadGeneration
	report.PublishDuration, report.Published = elapsed, true
}

func buildContractFollowupPrepared(ctx context.Context, req ContractFollowupRequest, prepared *contractFollowupPrepared) (report ContractFollowupReport, err error) {
	snap := req.Snapshot
	if snap.Release != nil {
		defer snap.Release()
	}
	if ctx == nil || snap.Release == nil || snap.Core == nil || snap.ReadAccepted == nil || snap.ReadCoreFile == nil || req.Payload == nil || req.Catalog == nil || req.Leases == nil || req.Yield == nil {
		return report, fmt.Errorf("contract followup: incomplete selected handoff")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if len(snap.InFlight) > 0 && snap.Scan == nil {
		// A frozen snapshot reads every census file as captured: it cannot
		// wait for an in-flight file's window to close.
		return report, graph.ErrContractProjectionStale
	}
	if req.RebuildReason != ContractFollowupColdBaseline && req.RebuildReason != ContractFollowupUnknownInterval && req.RebuildReason != ContractFollowupChangedBoundary {
		return report, fmt.Errorf("contract followup: explicit rebuild reason required")
	}
	report.RebuildReason = req.RebuildReason
	generation := req.Payload.ViewGeneration()
	if generation <= 0 {
		return report, fmt.Errorf("contract followup: payload must be isolated positive generation")
	}
	lease := req.Leases.Acquire(generation)
	defer lease.Release()
	report.PayloadGeneration = generation
	logical, err := graph.ComposeContractInputState(snap.Key.RepoPrefix, snap.Key.CheckoutID, snap.Inputs)
	if err != nil {
		return report, err
	}
	if !logical.Accepted || logical.InputVersion != snap.Key.InputVersion || logical.InputFingerprint != snap.Key.InputFingerprint {
		return report, graph.ErrContractProjectionStale
	}
	registry, evidence := prepared.registry, prepared.evidence
	if registry == nil {
		fileByPath := make(map[string]ContractFollowupFile, len(snap.Files))
		repos := make(map[string]bool)
		for _, w := range snap.Inputs {
			if w.Found && w.State.Accepted {
				repos[w.State.RepoPrefix] = true
			}
		}
		for _, file := range snap.Files {
			if file.Path == "" || file.SourceFingerprint == "" || file.Policy == "" || !repos[file.RepoPrefix] {
				return report, fmt.Errorf("contract followup: uncertified source census member %q", file.Path)
			}
			if _, duplicate := fileByPath[file.Path]; duplicate {
				return report, fmt.Errorf("contract followup: duplicate source path %q", file.Path)
			}
			if file.RepoPrefix != "" && !strings.HasPrefix(file.Path, file.RepoPrefix+"/") {
				return report, fmt.Errorf("contract followup: source namespace mismatch %q", file.Path)
			}
			fileByPath[file.Path] = file
		}
		scopes := make(map[string][2]string)
		for _, file := range snap.Files {
			if prior, seen := scopes[file.RepoPrefix]; seen && prior != [2]string{file.WorkspaceID, file.ProjectID} {
				return report, fmt.Errorf("contract followup: inconsistent accepted namespace scope %q", file.RepoPrefix)
			}
			scopes[file.RepoPrefix] = [2]string{file.WorkspaceID, file.ProjectID}
		}
		prepareStart := time.Now()
		req.coverageWork = prepared.work
		if req.coverageWork == nil {
			req.coverageWork = snap.Work
		}
		engine := newContractConvergence(ctx, req, fileByPath, repos, &report)
		runErr := engine.run()
		// The final enrichment round's store is the payload's evidence; it is
		// closed with the prepared cohort, on failure too.
		prepared.scratch, prepared.scratchDir = engine.scratch, engine.scratchDir
		if runErr != nil {
			// A superseded cohort still performed the work recorded by the
			// engine; keep it for the batch failure's mode and work metrics.
			prepared.report = report
			return report, runErr
		}
		prepared.coverage = engine.coverage.sealed
		prepared.proof = contractPublishedProof(snap.Inputs, engine.clock, snap.Key.CheckoutID)
		registry, evidence = engine.registry, engine.evidence
		report.PrepareDuration = time.Since(prepareStart)
		report.ScratchNodes, report.ScratchEdges = evidence.writtenNodes, evidence.writtenEdges
		report.ScratchBytes = contractFollowupScratchBytes(prepared.scratchDir)
		prepared.registry, prepared.evidence, prepared.report = registry, evidence, report
	} else {
		report = contractPreparedReport(report, prepared.report)
	}
	payloadStart := time.Now()
	own := registry.ByRepo(snap.Key.RepoPrefix)
	nodes, allOwnerEdges, missing := contractGraphRows(evidence, registry.All(), true)
	var edges []*graph.Edge
	for _, edge := range allOwnerEdges {
		if owner, _ := edge.Meta["contract_owner_repo_prefix"].(string); owner == snap.Key.RepoPrefix {
			edges = append(edges, edge)
		}
	}
	if missing != 0 {
		return report, fmt.Errorf("contract followup: %d missing selected source owners", missing)
	}
	report.Records = len(own)
	matches := contracts.Match(registry).Matched
	groups := make(map[bridgeGroupKey]bool)
	for _, match := range matches {
		if match.Provider.RepoPrefix == snap.Key.RepoPrefix || match.Consumer.RepoPrefix == snap.Key.RepoPrefix {
			groups[contractLinkBridgeGroup(match)] = true
		}
	}
	var relevant []contracts.CrossLink
	for _, match := range matches {
		if groups[contractLinkBridgeGroup(match)] {
			relevant = append(relevant, match)
			if match.Consumer.SymbolID != "" && match.Provider.SymbolID != "" {
				edges = append(edges, contractMatchEdge(match))
			}
		}
	}
	bridgeNodes, bridgeEdges := buildContractBridgeBatch(relevant)
	nodes = append(nodes, bridgeNodes...)
	edges = append(edges, bridgeEdges...)
	ids := make([]string, 0, len(evidence.outputIDs))
	for id := range evidence.outputIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += contractFrontierReadBatchSize {
		found, readErr := evidence.scratch.GetNodesByIDsContext(ctx, ids[start:min(start+contractFrontierReadBatchSize, len(ids))])
		if readErr == nil && len(found) != min(contractFrontierReadBatchSize, len(ids)-start) {
			return report, fmt.Errorf("contract followup: analysis output endpoint missing")
		}
		if readErr != nil {
			return report, readErr
		}
		for _, node := range found {
			nodes = append(nodes, node)
			if (node.Kind == graph.KindType || node.Kind == graph.KindInterface) && node.Meta["shape"] != nil {
				report.Shapes++
			}
		}
	}
	for _, edge := range evidence.outputEdges {
		edges = append(edges, edge)
	}
	if evidence.err != nil {
		return report, evidence.err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	encodedRows, encodeErr := json.Marshal(struct {
		Nodes []*graph.Node
		Edges []*graph.Edge
	}{nodes, edges})
	if encodeErr != nil {
		return report, encodeErr
	}
	if len(encodedRows) > contractFollowupCompactLimit || len(nodes)+len(edges) > graph.ContractProjectionRowLimit {
		return report, graph.ErrContractProjectionLimit
	}
	report.PayloadDuration = time.Since(payloadStart)
	for start := 0; start < len(nodes); start += contractFrontierReadBatchSize {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		if err := req.Payload.AddBatchChecked(nodes[start:min(start+contractFrontierReadBatchSize, len(nodes))], nil); err != nil {
			return report, err
		}
	}
	for start := 0; start < len(edges); start += contractFrontierReadBatchSize {
		if req.Yield != nil {
			if err := req.Yield(ctx); err != nil {
				return report, err
			}
		}
		if err := req.Payload.AddBatchChecked(nil, edges[start:min(start+contractFrontierReadBatchSize, len(edges))]); err != nil {
			return report, err
		}
	}
	report.Nodes = len(nodes)
	report.Edges = len(edges)
	ftsStart := time.Now()
	seenDocuments := make(map[string]bool)
	documentRepos := map[string]bool{snap.Key.RepoPrefix: true}
	var documents []graph.SymbolFTSItem
	flushDocuments := func() error {
		if len(documents) == 0 {
			return nil
		}
		if err := req.Yield(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := req.Payload.BatchUpsertSymbolFTS(documents); err != nil {
			return err
		}
		documents = documents[:0]
		return nil
	}
	documentBytes := 0
	for _, node := range nodes {
		if node.Kind != graph.KindContract && node.Kind != graph.KindContractBridge && node.Kind != graph.KindConfigKey {
			continue
		}
		if seenDocuments[node.ID] {
			continue
		}
		seenDocuments[node.ID] = true
		documentRepos[node.RepoPrefix] = true
		tokens := ftsTokensFor(node, "")
		documents = append(documents, graph.SymbolFTSItem{NodeID: node.ID, Tokens: tokens})
		report.SymbolDocuments++
		documentBytes += len(node.ID) + len(tokens)
		if len(documents) >= symbolFTSDirectChunkRows || documentBytes >= symbolFTSDirectChunkBytes {
			if err := flushDocuments(); err != nil {
				return report, err
			}
			documentBytes = 0
		}
	}
	if err := flushDocuments(); err != nil {
		return report, err
	}
	repos := make([]string, 0, len(documentRepos))
	for repo := range documentRepos {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := req.Payload.SetSymbolFTSNormalization(repo, search.FTSNormalizationMode()); err != nil {
			return report, err
		}
	}
	report.FTSDuration = time.Since(ftsStart)
	covered, coverageErr := prepared.coverage.rows(snap.Work)
	if coverageErr != nil {
		return report, coverageErr
	}
	if err := req.Payload.SetProducerState(store_sqlite.ProducerCompleteness{Producer: "graph.contracts", State: store_sqlite.ProducerStateComplete}); err != nil {
		return report, err
	}
	tokens := make([]string, 0, len(covered))
	for _, w := range covered {
		tokens = append(tokens, w.Token)
	}
	attachment := graph.ContractAttachment{RepoPrefix: snap.Key.RepoPrefix, CheckoutID: snap.Key.CheckoutID, InputVersion: snap.Key.InputVersion, InputFingerprint: snap.Key.InputFingerprint, PayloadGeneration: generation, CompletedTokens: tokens}
	report.PayloadDuration = time.Since(payloadStart)
	prepared.publications = append(prepared.publications, store_sqlite.ContractAttachmentPublishTarget{
		Store: req.Catalog, Expected: logical, Witnesses: snap.Inputs, Attachment: attachment, Work: covered,
	})
	return report, nil
}

// contractPreparedReport carries the shared cohort's preparation figures into
// a later target's report.
func contractPreparedReport(report, prepared ContractFollowupReport) ContractFollowupReport {
	report.Mode, report.CacheReason, report.ChangedFiles, report.RebuildReason = prepared.Mode, prepared.CacheReason, prepared.ChangedFiles, prepared.RebuildReason
	report.Files = prepared.Files
	report.SourceReads = prepared.SourceReads
	report.SourceBytes = prepared.SourceBytes
	report.ExtractDuration = prepared.ExtractDuration
	report.EnrichDuration = prepared.EnrichDuration
	report.ScratchNodes = prepared.ScratchNodes
	report.ScratchEdges = prepared.ScratchEdges
	report.ScratchBytes = prepared.ScratchBytes
	report.PrepareDuration = prepared.PrepareDuration
	report.CensusDuration, report.StampsDuration = prepared.CensusDuration, prepared.StampsDuration
	report.ScratchDuration, report.RoundsDuration = prepared.ScratchDuration, prepared.RoundsDuration
	report.Iterations, report.Rounds = prepared.Iterations, prepared.Rounds
	report.EnrichRuns, report.EnrichReruns, report.Revalidated = prepared.EnrichRuns, prepared.EnrichReruns, prepared.Revalidated
	report.EnrichRechecks, report.EnrichRecheckReads = prepared.EnrichRechecks, prepared.EnrichRecheckReads
	report.NeedSum, report.NeedIterations, report.WindowGaps = prepared.NeedSum, prepared.NeedIterations, prepared.WindowGaps
	return report
}

func contractFollowupOtherOwners(rows []contracts.Contract, repo string) []contracts.Contract {
	var out []contracts.Contract
	for _, row := range rows {
		if row.RepoPrefix != repo {
			out = append(out, row)
		}
	}
	return out
}
func contractFollowupPolicy(idx *Indexer, language string) (string, error) {
	return contractBoundaryPolicy(idx, language, 0)
}

const contractFollowupCompactLimit = 128 << 20

func (p *contractFollowupPrepared) close() error {
	var err error
	if p.scratch != nil {
		err = p.scratch.Close()
	}
	if p.scratchDir != "" {
		if removeErr := os.RemoveAll(p.scratchDir); err == nil {
			err = removeErr
		}
	}
	return err
}
func contractFollowupScratchBytes(dir string) int64 {
	var total int64
	for _, name := range []string{"evidence.sqlite", "evidence.sqlite-wal", "evidence.sqlite-shm"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Name imports use a synchronous, read-only selected visitor and private,
// single-worker evidence; they do not support reentrant scratch mutations.
// A page preserves the first
// missing ID and existing scratch precedence, while avoiding one commit per row.
// Completion markers are installed only after every page and live lookup succeed.
type contractNameImportPage struct {
	evidence     *contractFollowupEvidence
	nodes        []*graph.Node
	ids          map[string]bool
	encodedBytes int
	byteLimit    int
}

const contractNameImportEnvelopeBytes = len(`{"Nodes":[],"Edges":null}`)

func (p *contractNameImportPage) flush() bool {
	if len(p.nodes) == 0 {
		return p.evidence.err == nil
	}
	p.evidence.AddBatch(p.nodes, nil)
	if p.evidence.err != nil {
		return false
	}
	p.nodes = nil
	p.ids = nil
	p.encodedBytes = 0
	return true
}

func (p *contractNameImportPage) add(node *graph.Node) bool {
	if p.ids[node.ID] {
		return true
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		p.evidence.fail(err)
		return false
	}
	if len(encoded) > p.byteLimit-contractNameImportEnvelopeBytes {
		p.evidence.fail(graph.ErrContractProjectionLimit)
		return false
	}
	additional := len(encoded)
	if len(p.nodes) != 0 {
		additional++ // JSON array separator.
	}
	if contractNameImportEnvelopeBytes+p.encodedBytes+additional > p.byteLimit {
		if !p.flush() {
			return false
		}
		additional = len(encoded)
	}
	if p.ids == nil {
		p.ids = make(map[string]bool)
	}
	p.ids[node.ID] = true
	p.nodes = append(p.nodes, node)
	p.encodedBytes += additional
	return len(p.nodes) < contractFrontierReadBatchSize || p.flush()
}

// Probe only private scratch, in bounded groups. The selected visitor is
// synchronous and cannot mutate scratch. Its losing rows must never be encoded:
// a pre-existing/earlier ID may have unencodable or oversized selected metadata.
type contractNameImportProbes struct {
	page         *contractNameImportPage
	probe        func([]string) (map[string]*graph.Node, error)
	nodes        []*graph.Node
	ids          map[string]bool
	boundedBytes int
}

func (p *contractNameImportProbes) flush() bool {
	if len(p.nodes) == 0 {
		return p.page.evidence.err == nil
	}
	ids := make([]string, len(p.nodes))
	for i, node := range p.nodes {
		ids[i] = node.ID
	}
	present, err := p.probe(ids)
	if err != nil {
		p.page.evidence.fail(err)
		return false
	}
	for _, node := range p.nodes {
		if present[node.ID] == nil && !p.page.add(node) {
			return false
		}
	}
	p.nodes, p.ids, p.boundedBytes = nil, nil, 0
	return true
}

func (p *contractNameImportProbes) add(node *graph.Node) bool {
	if ctx := p.page.evidence.ctx; ctx != nil && ctx.Err() != nil {
		p.page.evidence.fail(ctx.Err())
		return false
	}
	if p.ids[node.ID] || p.page.ids[node.ID] {
		return true
	}
	bound, bounded := contractNameImportCandidateBound(node, p.page.byteLimit)
	remaining := func() int {
		return p.page.byteLimit - contractNameImportEnvelopeBytes - p.page.encodedBytes
	}
	if len(p.nodes) != 0 && (!bounded || bound > remaining()-p.boundedBytes) {
		if !p.flush() {
			return false
		}
	}
	if !bounded || bound > remaining() {
		// Preserve precedence before encoding or rejecting an unusual/large row.
		// Draining probes above does not commit a short import page.
		present, err := p.probe([]string{node.ID})
		if err != nil {
			p.page.evidence.fail(err)
			return false
		}
		return present[node.ID] != nil || p.page.add(node)
	}
	if p.ids == nil {
		p.ids = make(map[string]bool)
	}
	p.ids[node.ID] = true
	p.nodes = append(p.nodes, node)
	p.boundedBytes += bound
	// A possible full write page must be resolved before the next visitor row.
	return len(p.nodes) < contractFrontierReadBatchSize-len(p.page.nodes) || p.flush()
}

// Bound both detached pending storage and its possible JSON representation,
// without encoding a row before scratch precedence is known. Ordinary decoded
// JSON metadata batches; custom/unsupported values take the single-probe path.
// The bound only chooses probe grouping, never refuses a selected row.
func contractNameImportCandidateBound(node *graph.Node, limit int) (int, bool) {
	remaining := limit
	charge := func(n int) bool {
		if n > remaining {
			return false
		}
		remaining -= n
		return true
	}
	text := func(s string) bool {
		return charge(64) && len(s) <= remaining/6 && charge(6*len(s))
	}
	// Traversal limits only choose the existing single-probe fallback.
	// In particular, a deep/cyclic loser must not exhaust the worker stack.
	visited := 0
	var value func(any, int) bool
	value = func(v any, depth int) bool {
		if depth > 64 || visited >= 4096 {
			return false
		}
		visited++
		switch v := v.(type) {
		case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return charge(64)
		case float32:
			return !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0) && charge(64)
		case float64:
			return !math.IsNaN(v) && !math.IsInf(v, 0) && charge(64)
		case string:
			return text(v)
		case []any:
			if !charge(64) {
				return false
			}
			for _, item := range v {
				if !charge(16) || !value(item, depth+1) {
					return false
				}
			}
			return true
		case map[string]any:
			if !charge(512) {
				return false
			}
			for key, item := range v {
				if !charge(64) || !text(key) || !value(item, depth+1) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	// Fixed field names, numeric fields, time encoding, headers and a separator.
	if !node.FetchedAt.IsZero() || !charge(1024) {
		return 0, false
	}
	for _, s := range []string{node.ID, string(node.Kind), node.Name, node.QualName, node.FilePath, node.Language, node.RepoPrefix, node.WorkspaceID, node.ProjectID, node.AbsoluteFilePath, node.Origin} {
		if !text(s) {
			return 0, false
		}
	}
	if !value(node.Meta, 0) {
		return 0, false
	}
	return limit - remaining, true
}

func (e *contractFollowupEvidence) AddNode(node *graph.Node) { e.AddBatch([]*graph.Node{node}, nil) }
func (e *contractFollowupEvidence) AddEdge(edge *graph.Edge) { e.AddBatch(nil, []*graph.Edge{edge}) }
func (e *contractFollowupEvidence) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if e.err != nil {
		return
	}
	if err := e.ctx.Err(); err != nil {
		e.fail(err)
		return
	}
	if err := contractEvidenceRowsWithinLimit(nodes, edges); err != nil {
		e.fail(err)
		return
	}
	if err := e.scratch.AddBatchChecked(nodes, edges); err != nil {
		e.fail(err)
		return
	}
	e.writtenNodes += len(nodes)
	e.writtenEdges += len(edges)
	if e.outputIDs == nil {
		e.outputIDs = make(map[string]bool)
		e.outputEdges = make(map[string]*graph.Edge)
		e.diEdges = make(map[string]*graph.Edge)
		e.springIDs = make(map[string]bool)
		e.javaMethodIDs = make(map[string]bool)
	}
	for _, node := range nodes {
		if node != nil {
			if _, ok := node.Meta["spring_config_keys"]; ok {
				e.springIDs[node.ID] = true
			}
			if node.Kind == graph.KindMethod && node.Language == "java" {
				e.javaMethodIDs[node.ID] = true
			}
		}
	}
	for _, node := range nodes {
		if node != nil && (node.Kind == graph.KindConfigKey || ((node.Kind == graph.KindType || node.Kind == graph.KindInterface) && node.Meta["shape"] != nil)) {
			e.outputIDs[node.ID] = true
		}
	}
	for _, edge := range edges {
		if edge != nil && (edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes) {
			if _, di := diContractFromEdge(edge); di {
				encoded, encodeErr := json.Marshal(edge)
				if encodeErr != nil {
					e.fail(encodeErr)
					return
				}
				e.diEdges[string(encoded)] = edge
			}
		}
	}
	for _, edge := range edges {
		if edge != nil && (edge.Kind == graph.EdgeReadsConfig || (edge.Kind == graph.EdgeCalls && edge.Meta["via"] == "spring.Bean")) {
			e.outputIDs[edge.From] = true
			e.outputIDs[edge.To] = true
			encoded, encodeErr := json.Marshal(edge)
			if encodeErr != nil {
				e.fail(encodeErr)
				return
			}
			e.outputEdges[string(encoded)] = edge
		}
	}
	if len(e.outputIDs)+len(e.outputEdges)+len(e.diEdges)+len(e.springIDs)+len(e.javaMethodIDs) > graph.ContractProjectionRowLimit {
		e.fail(graph.ErrContractProjectionLimit)
	}
}

// Legacy enrichment gets a private graph.Store facade. Every missing core
// lookup is checked and sticky; writes target the private scratch store.
type contractFollowupEvidence struct {
	graph.Store
	scratch                    *store_sqlite.Store
	writtenNodes, writtenEdges int
	outputIDs                  map[string]bool
	outputEdges                map[string]*graph.Edge
	diEdges                    map[string]*graph.Edge
	springIDs                  map[string]bool
	javaMethodIDs              map[string]bool
	ctx                        context.Context
	core                       graph.Reader
	files                      map[string]ContractFollowupFile
	// excluded census paths are failure envelopes: loading one yields no
	// evidence rather than an out-of-census refusal.
	excluded map[string]bool
	// ledger, while one file's extraction or an enrichment round runs,
	// records every key requested, the owning file of every node answered,
	// and every read answered from the core with a digest of its answer.
	ledger *contractUnitLedger
	// guard brackets every read the facade answers from the core.
	guard *contractReadGuard
	// semantic answers binding-type lookups from the selected core.
	semantic         graph.SemanticBindingTypeReader
	readCore         func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error)
	allowedRepos     map[string]bool
	loaded           map[string]bool
	loadedExtraction map[string]bool
	loadedFileNodes  map[string]bool
	// Completed names belong to this private, single-worker captured evidence.
	// Keep raw row counts so cached names still consume the request's budget;
	// scratch results and final accepted-source fences remain live.
	completedCoreNames     map[string]int
	completedCoreNameBytes int
	endpointReplay         *contractEndpointReplayBatch
	err                    error
}

// RepoFilePaths serves the complete accepted census directly; no errorless
// repository-wide scratch query is needed for Spring configuration discovery.
func (e *contractFollowupEvidence) RepoFilePaths(repo, workspace string, languages, extensions []string) []string {
	out := e.repoFilePaths(repo, workspace, languages, extensions)
	// The census is a read too: a join or departure answers differently.
	e.ledger.read(contractReadRepoFiles, []string{repo, workspace, contractJoinArgs(languages), contractJoinArgs(extensions)}, out)
	return out
}

func (e *contractFollowupEvidence) repoFilePaths(repo, workspace string, languages, extensions []string) []string {
	var out []string
	for path, file := range e.files {
		if file.RepoPrefix != repo || (workspace != "" && file.WorkspaceID != workspace) {
			continue
		}
		match := false
		for _, language := range languages {
			match = match || file.Language == language
		}
		for _, extension := range extensions {
			match = match || strings.HasSuffix(path, extension)
		}
		if match {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}
func (e *contractFollowupEvidence) selectedScratchNodes(ids map[string]bool, repo, workspace string, kinds []graph.NodeKind) []*graph.Node {
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	wanted := make(map[graph.NodeKind]bool)
	for _, kind := range kinds {
		wanted[kind] = true
	}
	var out []*graph.Node
	for start := 0; start < len(keys); start += contractFrontierReadBatchSize {
		found, err := e.scratch.GetNodesByIDsContext(e.ctx, keys[start:min(start+contractFrontierReadBatchSize, len(keys))])
		if err != nil {
			e.fail(err)
			return nil
		}
		for _, node := range found {
			if node.RepoPrefix == repo && (workspace == "" || node.WorkspaceID == workspace) && wanted[node.Kind] {
				out = append(out, node)
			}
		}
	}
	return out
}
func (e *contractFollowupEvidence) RepoNodesByKindsWithMetaKey(repo, workspace string, kinds []graph.NodeKind, key string) []*graph.Node {
	if key != "spring_config_keys" {
		e.fail(fmt.Errorf("contract followup: unsupported private metadata projection %s", key))
		return nil
	}
	return e.selectedScratchNodes(e.springIDs, repo, workspace, kinds)
}

// The worker's sole repo-language enrichment caller needs Java method inputs.
// Keep that admitted compact projection instead of scanning all Java payload.
func (e *contractFollowupEvidence) GetRepoNodesByLanguage(repo, language string) []*graph.Node {
	if language != "java" {
		e.fail(fmt.Errorf("contract followup: unsupported private language projection %s", language))
		return nil
	}
	return e.selectedScratchNodes(e.javaMethodIDs, repo, "", []graph.NodeKind{graph.KindMethod})
}

// RepoEdgesByKinds serves only the admitted DI seeds required by enrichment.
// It never invokes an errorless scratch SQL scan or loads all ordinary edges.
func (e *contractFollowupEvidence) RepoEdgesByKinds(repos []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	wantedRepos := make(map[string]bool)
	for _, repo := range repos {
		wantedRepos[repo] = true
	}
	wantedKinds := make(map[graph.EdgeKind]bool)
	for _, kind := range kinds {
		wantedKinds[kind] = true
	}
	var rows []graph.RepoEdgeRow
	for _, edge := range e.diEdges {
		if !wantedKinds[edge.Kind] {
			continue
		}
		found, err := e.scratch.GetNodesByIDsContext(e.ctx, []string{edge.From})
		if err != nil {
			e.fail(err)
			return nil
		}
		source := found[edge.From]
		if source == nil {
			e.fail(fmt.Errorf("contract followup: missing DI source %s", edge.From))
			return nil
		}
		if wantedRepos[source.RepoPrefix] {
			rows = append(rows, graph.RepoEdgeRow{Edge: edge, RepoPrefix: source.RepoPrefix})
		}
	}
	return rows
}
func (e *contractFollowupEvidence) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	rows, truncated, err := e.scratch.GetOutEdgesByNodeIDsWithMetadataContext(e.ctx, ids, graph.ContractProjectionRowLimit)
	if err != nil {
		e.fail(err)
		return nil
	}
	if truncated {
		e.fail(graph.ErrContractProjectionLimit)
		return nil
	}
	return rows
}
func (e *contractFollowupEvidence) GetOutEdges(id string) []*graph.Edge {
	return e.GetOutEdgesByNodeIDs([]string{id})[id]
}

func (e *contractFollowupEvidence) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}
func (e *contractFollowupEvidence) detach(node *graph.Node) *graph.Node {
	if node == nil || node.ID == "" {
		e.fail(fmt.Errorf("contract followup: malformed selected node"))
		return nil
	}
	if !e.allowedRepos[node.RepoPrefix] {
		e.fail(fmt.Errorf("contract followup: unwitnessed selected namespace %s", node.RepoPrefix))
		return nil
	}
	copyNode := *node
	if node.Meta != nil {
		copyNode.Meta = contractFollowupCloneValue(reflect.ValueOf(node.Meta)).Interface().(map[string]any)
	}
	if copyNode.Kind == graph.KindType || copyNode.Kind == graph.KindInterface {
		delete(copyNode.Meta, "shape")
	}
	return &copyNode
}
func contractFollowupCloneValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(contractFollowupCloneValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), contractFollowupCloneValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(contractFollowupCloneValue(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(contractFollowupCloneValue(v.Elem()))
		return out
	default:
		return v
	}
}
func (e *contractFollowupEvidence) loadFile(path string) ([]*graph.Node, []*graph.Edge, error) {
	// The target is an owner before the read: one caught between its evict
	// and add answers with no file node, and its stamp shows the move.
	e.ledger.owner(path)
	e.ledger.key("path:", path)
	defer e.guard.enter()()
	nodes, edges, err := e.loadFileRows(path)
	if err == nil {
		e.ledger.read(contractReadFile, []string{path}, contractFileAnswer{Nodes: nodes, Edges: edges}, path)
	}
	return nodes, edges, err
}

func (e *contractFollowupEvidence) loadFileRows(path string) ([]*graph.Node, []*graph.Edge, error) {
	return e.loadFileRowsContext(path, e.ctx, false)
}

func (e *contractFollowupEvidence) loadFileRowsContext(path string, ctx context.Context, extraction bool) ([]*graph.Node, []*graph.Edge, error) {
	if e.loaded == nil {
		e.loaded = make(map[string]bool)
	}
	if e.loaded[path] || extraction && e.loadedExtraction[path] {
		file := e.files[path]
		p, readErr := e.scratch.LayerContractFileProjectionContext(e.ctx, file.RepoPrefix, []string{path})
		if readErr != nil {
			e.fail(readErr)
			return nil, nil, readErr
		}
		nodes := p.FileNodes[path]
		var ids []string
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		var edges []*graph.Edge
		bySource, truncated, readErr := e.scratch.GetOutEdgesByNodeIDsWithMetadataContext(e.ctx, ids, graph.ContractProjectionRowLimit)
		if readErr != nil {
			e.fail(readErr)
			return nil, nil, readErr
		}
		if truncated {
			e.fail(graph.ErrContractProjectionLimit)
			return nil, nil, e.err
		}
		for _, rows := range bySource {
			edges = append(edges, rows...)
		}
		return nodes, edges, e.err
	}
	if _, present := e.files[path]; !present && e.excluded[path] {
		return nil, nil, nil
	}
	nodes, edges, err := e.readSelectedFileRowsContext(path, ctx)
	if err != nil {
		return nil, nil, err
	}
	if e.loadedFileNodes[path] {
		// Node-only consumers may already have enriched these private rows.
		// Extend their edge evidence without replacing scratch mutations or
		// derived nodes with the original selected-core declarations.
		current, readErr := graph.ContractFileNodesContext(e.ctx, e.scratch, e.files[path].RepoPrefix, []string{path})
		if readErr != nil {
			e.fail(readErr)
			return nil, nil, readErr
		}
		nodes = current[path]
		e.AddBatch(nil, edges)
	} else {
		e.AddBatch(nodes, edges)
	}
	if extraction {
		if e.loadedExtraction == nil {
			e.loadedExtraction = make(map[string]bool)
		}
		if e.loadedFileNodes == nil {
			e.loadedFileNodes = make(map[string]bool)
		}
		e.loadedExtraction[path] = true
		e.loadedFileNodes[path] = true
	} else {
		e.loaded[path] = true
	}
	return nodes, edges, e.err
}

// readSelectedFileRowsContext normalizes one checked core answer without
// importing it into scratch. Extraction and its replay share these exact
// ownership, row-bound, node and retained-edge rules.
func (e *contractFollowupEvidence) readSelectedFileRowsContext(path string, ctx context.Context) ([]*graph.Node, []*graph.Edge, error) {
	file, ok := e.files[path]
	if !ok {
		if e.excluded[path] {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("contract followup: selected file outside accepted census %q", path)
	}
	rows, err := e.readCore(ctx, file)
	if err != nil {
		e.fail(err)
		return nil, nil, err
	}
	if len(rows.Nodes)+len(rows.Edges) > graph.ContractProjectionRowLimit {
		e.fail(graph.ErrContractProjectionLimit)
		return nil, nil, e.err
	}
	nodes, err := e.detachFileNodes(path, file, rows.Nodes)
	if err != nil {
		return nil, nil, err
	}
	var edges []*graph.Edge
	for _, edge := range rows.Edges {
		if edge == nil || edge.From == "" || edge.To == "" {
			e.fail(fmt.Errorf("contract followup: malformed selected edge"))
			return nil, nil, e.err
		}
		// Existing contract output must never seed an isolated new analysis.
		if edge.Kind == graph.EdgeHandlesRoute || edge.Kind == graph.EdgeMatches || edge.Kind == graph.EdgeBridges || edge.Kind == graph.EdgeReadsConfig {
			continue
		}
		if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
			if _, di := diContractFromEdge(edge); !di {
				continue
			}
		}
		copyEdge := *edge
		if edge.Meta != nil {
			copyEdge.Meta = contractFollowupCloneValue(reflect.ValueOf(edge.Meta)).Interface().(map[string]any)
		}
		edges = append(edges, &copyEdge)
	}
	return nodes, edges, e.err
}

func (e *contractFollowupEvidence) detachFileNodes(path string, file ContractFollowupFile, raw []*graph.Node) ([]*graph.Node, error) {
	var nodes []*graph.Node
	fileFound := false
	for _, node := range raw {
		if node == nil || node.FilePath != path || node.RepoPrefix != file.RepoPrefix {
			e.fail(fmt.Errorf("contract followup: selected file identity mismatch %q", path))
			return nil, e.err
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge || node.Kind == graph.KindConfigKey {
			continue
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return nil, e.err
		}
		nodes = append(nodes, copyNode)
		if node.Kind == graph.KindFile || (node.Kind == graph.KindFixture && node.ID == path && fixtures.IsFixturePath(path) && node.Meta["fixture"] == true) {
			fileFound = true
		}
	}
	if !fileFound {
		e.fail(fmt.Errorf("contract followup: accepted census file has no selected core owner %q", path))
		return nil, e.err
	}
	return nodes, nil
}

// loadFileNodes answers node-only consumers without importing outgoing edges.
// A later full file load still reads every edge through loadFileRows.
func (e *contractFollowupEvidence) loadFileNodes(path string) ([]*graph.Node, error) {
	e.ledger.owner(path)
	e.ledger.key("path:", path)
	defer e.guard.enter()()
	nodes, err := e.loadFileNodeRows(path)
	if err == nil {
		e.ledger.read(contractReadFileNodes, []string{path}, nodes, path)
	}
	return nodes, err
}

func (e *contractFollowupEvidence) loadFileNodeRows(path string) ([]*graph.Node, error) {
	file, ok := e.files[path]
	if !ok {
		if e.excluded[path] {
			return nil, nil
		}
		return nil, fmt.Errorf("contract followup: selected file outside accepted census %q", path)
	}
	reader := e.core
	loaded := e.loaded[path] || e.loadedFileNodes[path]
	if loaded {
		reader = e.scratch
	}
	rows, err := graph.ContractFileNodesContext(e.ctx, reader, file.RepoPrefix, []string{path})
	if err != nil {
		e.fail(err)
		return nil, err
	}
	if loaded {
		return rows[path], nil
	}
	nodes, err := e.detachFileNodes(path, file, rows[path])
	if err != nil {
		return nil, err
	}
	e.AddBatch(nodes, nil)
	if e.err != nil {
		return nil, e.err
	}
	if e.loadedFileNodes == nil {
		e.loadedFileNodes = make(map[string]bool)
	}
	e.loadedFileNodes[path] = true
	return nodes, nil
}

func (e *contractFollowupEvidence) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node, len(paths))
	for _, path := range paths {
		nodes, err := e.loadFileNodes(path)
		if err != nil {
			e.fail(err)
			return nil
		}
		out[path] = nodes
	}
	return out
}
func (e *contractFollowupEvidence) GetNodesByIDs(ids []string) map[string]*graph.Node {
	e.ledger.key("id:", ids...)
	defer e.guard.enter()()
	out := e.getNodesByIDs(ids)
	for _, node := range out {
		e.ledger.nodes(node)
	}
	return out
}

func (e *contractFollowupEvidence) getNodesByIDs(ids []string) map[string]*graph.Node {
	out, readErr := e.scratch.GetNodesByIDsContext(e.ctx, ids)
	if readErr != nil {
		e.fail(readErr)
		return nil
	}
	var missing []string
	for _, id := range ids {
		if out[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out
	}
	found, err := graph.ContractSourceNodesContext(e.ctx, e.core, missing)
	if err != nil {
		e.fail(err)
		return nil
	}
	// Only a selected-core fetch is an input. Scratch hits may already
	// include shapes derived from earlier recorded inputs in this round.
	// Comparing those derived values with pristine core rows would invalidate
	// an otherwise unchanged round during its next recheck.
	imported := make(map[string]*graph.Node, len(found))
	// Reuse the existing row/encoded-byte bounded importer. Selected values
	// still enter private scratch before the ID proof is recorded, while one
	// checked read no longer starts a transaction for each returned node.
	page := contractNameImportPage{evidence: e, byteLimit: contractFollowupCompactLimit}
	for _, node := range found {
		if node == nil {
			e.fail(fmt.Errorf("contract followup: nil selected source"))
			return nil
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			e.fail(fmt.Errorf("contract followup: attempted to import core contract output"))
			return nil
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return nil
		}
		if !page.add(copyNode) {
			return nil
		}
		out[copyNode.ID] = copyNode
		imported[copyNode.ID] = copyNode
	}
	if !page.flush() {
		return nil
	}
	if e.err == nil {
		e.ledger.read(contractReadIDs, missing, imported)
	}
	return out
}
func (e *contractFollowupEvidence) GetNode(id string) *graph.Node {
	return e.GetNodesByIDs([]string{id})[id]
}
func (e *contractFollowupEvidence) FindNodesByNames(names []string) map[string][]*graph.Node {
	e.ledger.key("name:", names...)
	defer e.guard.enter()()
	out := e.findNodesByNames(names)
	for _, nodes := range out {
		e.ledger.nodes(nodes...)
	}
	if e.err == nil {
		e.ledger.read(contractReadNames, names, out)
	}
	return out
}

func (e *contractFollowupEvidence) findNodesByNames(names []string) map[string][]*graph.Node {
	if e.err != nil {
		return nil
	}
	if e.ctx != nil && e.ctx.Err() != nil {
		e.fail(e.ctx.Err())
		return nil
	}
	count := 0
	page := contractNameImportPage{evidence: e, byteLimit: contractFollowupCompactLimit}
	probes := contractNameImportProbes{page: &page, probe: func(ids []string) (map[string]*graph.Node, error) {
		return e.scratch.GetNodesByIDsContext(e.ctx, ids)
	}}
	pending := names
	newCounts := make(map[string]int, len(names))
	cacheable := e.core != nil
	for _, name := range names {
		if _, duplicate := newCounts[name]; duplicate {
			// Optional fallback readers may visit duplicate names repeatedly.
			// Preserve that exact legacy visit and its row budget, without memo.
			cacheable = false
		}
		newCounts[name] = 0
	}
	if cacheable {
		pending = make([]string, 0, len(names))
		for _, name := range names {
			if rows, done := e.completedCoreNames[name]; done {
				count += rows
				delete(newCounts, name)
				if count > graph.ContractProjectionRowLimit {
					e.fail(graph.ErrContractProjectionLimit)
					return nil
				}
			} else {
				pending = append(pending, name)
			}
		}
	}
	err := graph.VisitNodesByNamesContext(e.ctx, e.core, pending, func(node *graph.Node) bool {
		count++
		if count > graph.ContractProjectionRowLimit {
			e.fail(graph.ErrContractProjectionLimit)
			return false
		}
		if node == nil {
			cacheable = false
			return true
		}
		if _, exact := newCounts[node.Name]; exact {
			newCounts[node.Name]++
		} else {
			cacheable = false
		}
		if node.Kind == graph.KindContract || node.Kind == graph.KindContractBridge {
			return true
		}
		copyNode := e.detach(node)
		if copyNode == nil {
			return false
		}
		return probes.add(copyNode)
	})
	if err != nil {
		e.fail(err)
	}
	if e.err != nil || !probes.flush() || !page.flush() {
		return nil
	}
	out := make(map[string][]*graph.Node)
	err = graph.VisitNodesByNamesContext(e.ctx, e.scratch, names, func(node *graph.Node) bool { out[node.Name] = append(out[node.Name], node); return true })
	if err != nil {
		e.fail(err)
		return nil
	}
	if cacheable {
		if e.completedCoreNames == nil {
			e.completedCoreNames = make(map[string]int)
		}
		for _, name := range pending {
			if len(e.completedCoreNames) >= graph.ContractProjectionRowLimit || len(name) > contractCoreReceiptPayloadLimit-e.completedCoreNameBytes {
				continue
			}
			e.completedCoreNames[name] = newCounts[name]
			e.completedCoreNameBytes += len(name)
		}
	}
	return out
}
func (e *contractFollowupEvidence) FindNodesByName(name string) []*graph.Node {
	return e.FindNodesByNames([]string{name})[name]
}
func (e *contractFollowupEvidence) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	e.ledger.key("id:", ids...)
	defer e.guard.enter()()
	out, owners, err := e.constantValues(ids)
	if err == nil {
		e.ledger.read(contractReadConstants, ids, out, owners...)
	}
	return out, err
}

// semanticTypes answers a binding-type lookup from the selected core through
// the facade, recorded like every other core read.
func (e *contractFollowupEvidence) semanticTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	if e.semantic == nil {
		return nil, nil
	}
	defer e.guard.enter()()
	out, err := e.semantic.SemanticBindingTypes(sites)
	if err != nil {
		return out, err
	}
	encoded, encodeErr := json.Marshal(sites)
	if encodeErr != nil {
		if e.ledger != nil {
			e.ledger.opaque = true
		}
		return out, nil
	}
	files := make([]string, 0, len(sites))
	for _, site := range sites {
		files = append(files, site.FilePath)
	}
	e.ledger.read(contractReadSemantic, []string{string(encoded)}, out, files...)
	return out, nil
}

// contractLedgerSemanticReader is the semantic reader an analysis indexer
// sees: every lookup goes through the evidence facade.
type contractLedgerSemanticReader struct{ evidence *contractFollowupEvidence }

func (r contractLedgerSemanticReader) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	return r.evidence.semanticTypes(sites)
}

// constantValues reads constant values and, for a recording ledger, the
// files declaring the answered constants: the value rows carry no file.
func (e *contractFollowupEvidence) constantValues(ids []string) (map[string]string, []string, error) {
	out, err := graph.ConstantValuesByNodeIDsContext(e.ctx, e.core, ids)
	if err != nil {
		e.fail(err)
		return out, nil, err
	}
	var files []string
	if e.ledger != nil && len(out) > 0 {
		answered := make([]string, 0, len(out))
		for id := range out {
			answered = append(answered, id)
		}
		sort.Strings(answered)
		owners, ownerErr := graph.ContractSourceNodesContext(e.ctx, e.core, answered)
		if ownerErr != nil {
			e.fail(ownerErr)
			return out, nil, ownerErr
		}
		for _, owner := range owners {
			e.ledger.nodes(owner)
			if owner != nil {
				files = append(files, owner.FilePath)
			}
		}
	}
	return out, files, err
}

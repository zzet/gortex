package indexer

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func continuationProofFixture(t *testing.T) (*contractConvergence, *contractDeltaCache, []graph.ContractWork) {
	t.Helper()
	work := []graph.ContractWork{{Token: "captured", OriginGeneration: 7, RepoPrefix: "repo", FilePath: "repo/producer.go", InputVersion: "v", InputFingerprint: "f", State: graph.ContractWorkPending, Scope: graph.ContractWorkScope{LookupKeys: []string{"name:Value"}}}}
	stamp := ContractReceiptStamp{Fingerprint: "f", SourceFingerprint: "bytes", Accepted: true}
	producer := &contractUnit{path: "repo/producer.go", stamp: stamp, round: 1, declared: []string{"name:Value"}, rawDigest: "raw"}
	reader := &contractUnit{path: "repo/reader.go", stamp: stamp, round: 1, keys: map[string]bool{"name:Value": true}, rawDigest: "reader"}
	w := newContractWorkCoverage(work)
	w.anchored = true
	c := &contractConvergence{ctx: t.Context(), coverage: w, units: map[string]*contractUnit{producer.path: producer, reader.path: reader}, live: map[string]bool{producer.path: true, reader.path: true}, suspect: map[string]bool{}, repos: map[string]bool{"repo": true}, clock: contractChangeClock{clock: 1, last: map[string]ContractReceiptStamp{producer.path: stamp, reader.path: stamp}, lastCensus: map[string]bool{producer.path: true, reader.path: true}, changedAt: map[string]uint64{}}}
	w.observe(c)
	w.extracted(producer, producer.path)
	w.record(reader, []string{producer.path, reader.path})
	identity := contractDeltaIdentity{selection: "selected"}
	c.req.delta = &contractDeltaSession{identity: identity}
	p := &contractDeltaCache{identity: identity, units: cloneContractUnits(c.units), clock: cloneContractClock(c.clock)}
	p.continuation = checkpointContractFill(c, p, &contractProofStrings{})
	require.NotNil(t, p.continuation)
	require.Positive(t, p.continuation.metadataSize())
	for _, proof := range p.continuation.units {
		require.Same(t, p.units[proof.unit.path], proof.unit, "provenance aliases only its installed checkpoint unit")
	}
	c.units = p.units
	c.coverage = newContractWorkCoverage(work)
	c.coverage.anchored = true
	return c, p, work
}

func TestContractFillContinuationRetainsOnlyIdenticalObligations(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(*graph.ContractWork)
	}{
		{"token", func(w *graph.ContractWork) { w.Token = "replacement" }},
		{"origin", func(w *graph.ContractWork) { w.OriginGeneration++ }},
		{"checkout", func(w *graph.ContractWork) { w.CheckoutID = "other" }},
		{"version", func(w *graph.ContractWork) { w.InputVersion = "other" }},
		{"fingerprint", func(w *graph.ContractWork) { w.InputFingerprint = "other" }},
		{"scope", func(w *graph.ContractWork) { w.Scope.LookupKeys = []string{"name:Other"} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, p, work := continuationProofFixture(t)
			changed := work[0]
			row.change(&changed)
			c.coverage = newContractWorkCoverage([]graph.ContractWork{changed})
			c.coverage.anchored = true
			p.continuation.resume(c, p)
			require.Empty(t, c.coverage.own, "same stamped bytes do not satisfy replaced work")
			require.Empty(t, c.coverage.read, "a replaced obligation requires current reader proofs")
		})
	}
}

func TestContractFillContinuationNewUnrelatedWorkPreservesPriorProgress(t *testing.T) {
	c, p, work := continuationProofFixture(t)
	work = append(work, graph.ContractWork{Token: "new-unrelated", RepoPrefix: "repo", FilePath: "repo/other.go", State: graph.ContractWorkPending})
	c.coverage = newContractWorkCoverage(work)
	c.coverage.anchored = true
	p.continuation.resume(c, p)
	require.Same(t, c.units["repo/producer.go"], c.coverage.own["repo/producer.go"])
	require.True(t, c.coverage.readSatisfied(c.units["repo/reader.go"]))
	require.Nil(t, c.coverage.own["repo/other.go"], "new debt has no inherited extraction")
}

func TestContractFillContinuationGuardChangesRequireReplay(t *testing.T) {
	for _, state := range []string{"changed", "writing", "absent", "new_owner", "different_unit", "selection"} {
		t.Run(state, func(t *testing.T) {
			c, p, _ := continuationProofFixture(t)
			switch state {
			case "changed":
				stamp := c.clock.last["repo/producer.go"]
				stamp.Seq++
				c.clock.last["repo/producer.go"] = stamp
			case "writing":
				stamp := c.clock.last["repo/producer.go"]
				stamp.Writing = true
				c.clock.last["repo/producer.go"] = stamp
			case "absent":
				delete(c.clock.last, "repo/producer.go")
			case "new_owner":
				c.coverage.extra["repo/reader.go"] = map[string]bool{"repo/new.go": true}
			case "different_unit":
				unit := *c.units["repo/reader.go"]
				c.units[unit.path] = &unit
			case "selection":
				c.req.delta.identity.selection = "other"
			}
			p.continuation.resume(c, p)
			require.False(t, c.coverage.readSatisfied(c.units["repo/reader.go"]), "a changed proof must use the normal guarded replay")
		})
	}
}

func TestContractFillContinuationNamespaceResetUsesExactOriginalRows(t *testing.T) {
	c, p, work := continuationProofFixture(t)
	barrier := graph.ContractWork{Token: "namespace", RepoPrefix: "repo", FilePath: "repo/", State: graph.ContractWorkPending}
	work = append(work, barrier)
	p.continuation.barrierRows = contractFillBarrierRows(work)
	require.True(t, p.continuation.resumesBarrier(p.identity, work))
	work[len(work)-1].Token = "new-namespace"
	require.False(t, p.continuation.resumesBarrier(p.identity, work))
	require.False(t, p.continuation.resumesBarrier(contractDeltaIdentity{selection: "other"}, work))
	c.coverage = newContractWorkCoverage(work)
	c.coverage.anchored = true
	p.continuation.resume(c, p)
	require.False(t, c.coverage.readSatisfied(c.units["repo/reader.go"]), "new namespace work cannot reuse the old read frontier")
}

func TestContractFillContinuationCancellationDoesNotMintProvenance(t *testing.T) {
	c, _, _ := continuationProofFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.ctx = ctx
	c.req = ContractFollowupRequest{delta: &contractDeltaSession{identity: contractDeltaIdentity{selection: "selected"}}}
	c.report = &ContractFollowupReport{Mode: viewmetrics.ContractModeFill}
	require.ErrorIs(t, c.continueFill(1, true), context.Canceled)
	require.False(t, c.req.delta.continued)
}

func TestContractFillContinuationSelectionChangesRefuseOldAnchor(t *testing.T) {
	for _, field := range []string{"selection", "config", "modules", "receipt"} {
		t.Run(field, func(t *testing.T) {
			c, p, _ := continuationProofFixture(t)
			switch field {
			case "selection":
				c.req.delta.identity.selection = "new"
			case "config":
				c.req.delta.identity.config = "new"
			case "modules":
				c.req.delta.identity.modules = "new"
			case "receipt":
				c.req.delta.identity.receiptVersion = "new"
			}
			p.continuation.resume(c, p)
			require.Empty(t, c.coverage.own)
			require.Empty(t, c.coverage.read)
		})
	}
}

func TestContractFillContinuationAbsentGuardBecomingPresentRequiresReplay(t *testing.T) {
	c, p, _ := continuationProofFixture(t)
	entry := continuationUnitProof(p.continuation, "repo/reader.go")
	// Recreate an admitted absent owner using the real checkpoint builder.
	c.coverage = newContractWorkCoverage(c.coverage.work)
	c.coverage.anchored = true
	c.coverage.extra["repo/reader.go"] = map[string]bool{"repo/absent.go": true}
	c.coverage.record(entry.unit, []string{"repo/absent.go", "repo/producer.go", "repo/reader.go"})
	p.continuation = checkpointContractFill(c, p, &contractProofStrings{})
	c.coverage = newContractWorkCoverage(c.coverage.work)
	c.coverage.anchored = true
	p.continuation.resume(c, p)
	require.True(t, c.coverage.readSatisfied(c.units["repo/reader.go"]), "continued absence is an exact guard witness")
	c.coverage.read = map[string]*contractUnit{}
	c.clock.last["repo/absent.go"] = ContractReceiptStamp{Accepted: true, Fingerprint: "new"}
	p.continuation.resume(c, p)
	require.False(t, c.coverage.readSatisfied(c.units["repo/reader.go"]))
}

func TestContractFillContinuationRemovedOwnDebtDoesNotCoverNewPath(t *testing.T) {
	c, p, work := continuationProofFixture(t)
	changed := work[0]
	changed.FilePath = "repo/new.go"
	c.coverage = newContractWorkCoverage([]graph.ContractWork{changed})
	c.coverage.anchored = true
	p.continuation.resume(c, p)
	require.Empty(t, c.coverage.own)
	require.False(t, c.coverage.readSatisfied(c.units["repo/reader.go"]))
}

func TestContractFillContinuationMetadataChargesAliasesOnce(t *testing.T) {
	_, p, _ := continuationProofFixture(t)
	before := p.continuation.metadataSize()
	require.Positive(t, before)
	unit := p.units["repo/producer.go"]
	unit.raw = []contracts.Contract{{Meta: map[string]any{"large": strings.Repeat("x", 1<<20)}}}
	require.Equal(t, before, p.continuation.metadataSize(), "raw referents are already charged by cache units; provenance charges its own metadata and pointer slots")
}

func continuationUnitProof(proof *contractFillContinuation, path string) *contractFillUnitProof {
	for i := range proof.units {
		if proof.units[i].unit.path == path {
			return &proof.units[i]
		}
	}
	return nil
}

func TestContractFillContinuationNamespaceReadsBindAdmittedCensus(t *testing.T) {
	for _, changeAfterRead := range []bool{false, true} {
		t.Run(fmt.Sprint(changeAfterRead), func(t *testing.T) {
			c, p, _ := continuationProofFixture(t)
			p.continuation.resume(c, p)
			reader := c.units["repo/reader.go"]
			reader.ledger = newContractUnitLedger()
			reader.ledger.reads = []contractRecordedRead{{kind: contractReadRepoFiles, args: []string{"repo"}}}
			c.coverage.record(reader, []string{"repo/producer.go", "repo/reader.go"})
			c.noteFillRead(reader)
			if changeAfterRead {
				c.clock.lastCensus["repo/new.go"] = true
			}
			p.continuation = checkpointContractFill(c, p, &contractProofStrings{})
			require.NotNil(t, p.continuation)
			entry := continuationUnitProof(p.continuation, "repo/reader.go")
			if changeAfterRead {
				require.True(t, entry == nil || entry.flags&contractFillRead == 0, "a checkpoint's later census cannot impersonate the query's admitted census")
				return
			}
			require.NotNil(t, entry)
			require.NotZero(t, entry.flags&contractFillNamespace)
			c.coverage = newContractWorkCoverage(c.coverage.work)
			c.coverage.anchored = true
			p.continuation.resume(c, p)
			require.True(t, c.coverage.readSatisfied(reader))
			c.coverage.read = map[string]*contractUnit{}
			c.clock.lastCensus["repo/new.go"] = true
			p.continuation.resume(c, p)
			require.False(t, c.coverage.readSatisfied(reader), "a census-only join needs actual guarded replay even with unchanged receipts and work")
		})
	}
}

func TestContractFillContinuationCompactMetadataFitsNearBudget(t *testing.T) {
	const files = 4930
	stamp := ContractReceiptStamp{Accepted: true, Fingerprint: "same", SourceFingerprint: "bytes"}
	work := make([]graph.ContractWork, 0, files)
	units := make(map[string]*contractUnit, files)
	stamps := make(map[string]ContractReceiptStamp, files)
	census := make(map[string]bool, files)
	legacyRawSizes := make(map[string]int64, files)
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("repo/file%04d.go", i)
		work = append(work, graph.ContractWork{Token: path, RepoPrefix: "repo", FilePath: path, InputVersion: "v", InputFingerprint: "f", State: graph.ContractWorkPending})
		units[path] = &contractUnit{path: path, stamp: stamp, round: 1, retainedRawBytes: 24}
		stamps[path], census[path], legacyRawSizes[path] = stamp, true, 24
	}
	w := newContractWorkCoverage(work)
	w.anchored = true
	c := &contractConvergence{ctx: t.Context(), coverage: w, units: units, live: census, clock: contractChangeClock{clock: 1, last: stamps, lastCensus: census}}
	for path, unit := range units {
		w.extracted(unit, path)
	}
	p := &contractDeltaCache{identity: contractDeltaIdentity{selection: "selected"}, units: cloneContractUnits(units), clock: cloneContractClock(c.clock)}
	p.continuation = checkpointContractFill(c, p, &contractProofStrings{})
	require.NotNil(t, p.continuation)
	metadata := p.continuation.metadataSize()
	// The duplicate legacy raw-size table disappeared; both the new unit
	// field and its ordinary recursive estimator charge remain accounted.
	released := contractRetainedSizeMode(reflect.ValueOf(legacyRawSizes), false) - int64(files)*2*int64(reflect.TypeFor[int64]().Size())
	const oldHeadroom = int64(125 << 10)
	require.Less(t, metadata, released+oldHeadroom, "shared vectors fit the headroom recovered from the duplicate raw-size map")
	// Model already charged payload bytes at the previous corpus admission
	// boundary. This tests admission arithmetic without allocating 64MiB raw.
	p.retainedBytes = contractDeltaCacheBudget - oldHeadroom - released + metadata
	lru := contractDeltaLRU{}
	actor := NewContractActor("", []string{"repo"})
	admission := lru.put(actor, p)
	require.Equal(t, "admitted", admission.reason, "a near-complete checkpoint must not reject and restart its prefix")
	require.LessOrEqual(t, lru.bytes, contractDeltaCacheBudget)
	require.Same(t, p, lru.get(actor))
	for _, proof := range p.continuation.units {
		require.Same(t, p.units[proof.unit.path], proof.unit)
	}
}

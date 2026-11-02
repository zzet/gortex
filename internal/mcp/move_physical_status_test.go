package mcp

import (
	"testing"
	"os"
	"time"
	"weak"
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"

	"github.com/stretchr/testify/require"
)

func TestMovePhysicalStatusMatchesExistingGuardedRecord(t *testing.T) {
	s, move, ctx, ack, _, target := moveObservationExecutionFixture(t)
	ack.finishActual()
	ack.finishReply(nil, nil)
	timing := move.records[target]
	physical := timing.moveFile.physical.Value()
	require.NotNil(t, physical)
	before := s.mutationStatusPayload(physical)
	status, handled := s.moveFileObservationPayload(ctx, timing.id)
	require.True(t, handled)
	require.Equal(t, "observed", status["physical_commit_state"])
	require.Equal(t, before, status["physical_commit"], "render the same owned physical status, including unavailable/null publication evidence")
	require.Equal(t, true, status["physical_commit_shared_acknowledgement"])
	require.Equal(t, false, status["physical_commit_additive_with_acknowledgement"])
	require.Equal(t, "move_file_observation", status["receipt_kind"])
	require.NotContains(t, status, "mutation_id")
	require.NotContains(t, status, "graph_status_terminal", "the timing row does not become physical authority")
	child := status["physical_commit"].(map[string]any)
	require.Equal(t, physical.id, child["receipt"])
	child["graph_status"] = "fabricated-client-value"
	again, _ := s.moveFileObservationPayload(ctx, timing.id)
	require.NotEqual(t, "fabricated-client-value", again["physical_commit"].(map[string]any)["graph_status"])
}

func TestMovePhysicalStatusRejectsBrokenExactOwners(t *testing.T) {
	for _, mode := range []string{"ack", "server", "path", "blob", "sequence", "checkout", "incarnation", "evicted", "replaced", "forward-link", "reverse-link", "legacy-in-flight"} {
		t.Run(mode, func(t *testing.T) {
			s, move, ctx, _, _, target := moveObservationExecutionFixture(t)
			timing := move.records[target]
			physical := timing.moveFile.physical.Value()
			require.NotNil(t, physical)
			ledger := &s.mutationCommits
			ledger.mu.Lock()
			timing.mu.Lock()
			physical.mu.Lock()
			switch mode {
			case "ack": physical.acknowledgement = &editAcknowledgement{}
			case "server": physical.owner = &Server{}
			case "path": physical.absPath = target+".foreign"
			case "blob": physical.newSHA = "foreign"
			case "sequence": physical.commitSeq++
			case "checkout": physical.checkoutID = "foreign"
			case "incarnation": physical.checkoutIncarnation = "foreign"
			case "evicted": delete(ledger.byID, physical.id)
			case "replaced": ledger.byID[physical.id] = &mutationCommitRecord{id:physical.id}
			case "forward-link": timing.moveFile.physical = weak.Pointer[mutationCommitRecord]{}
			case "reverse-link": physical.moveTimingOwner = weak.Pointer[mutationCommitRecord]{}
			case "legacy-in-flight":
				// Model the legacy schedule explicitly. Real held-path admission
				// now prevents this old admitted writer from overtaking a move.
				legacy := &mutationCommitRecord{owner:s, id:"legacy-in-flight", absPath:target, disk:mutationDiskInFlight,
					startedAt:timing.startedAt.Add(-time.Second), diskWriteStartedAt:timing.committedAt.Add(-time.Nanosecond)}
				ledger.byID[legacy.id] = legacy
				ledger.ordered = append([]*mutationCommitRecord{legacy}, ledger.ordered...)
			}
			physical.mu.Unlock()
			timing.mu.Unlock()
			ledger.mu.Unlock()
			status, _ := s.moveFileObservationPayload(ctx, timing.id)
			require.Equal(t, true, status["found"])
			require.Equal(t, "unavailable", status["physical_commit_state"])
			require.NotContains(t, status, "physical_commit")
		})
	}
}

func TestMovePhysicalStatusDoesNotSearchPastAnotherWriter(t *testing.T) {
	for _, noWrite := range []bool{false,true} {
		t.Run(map[bool]string{false:"same-bytes-new-ack",true:"cancelled-no-write"}[noWrite], func(t *testing.T) {
			s, move, ctx, _, _, target := moveObservationExecutionFixture(t)
			original, err := os.ReadFile(target)
			require.NoError(t, err)
			writeCtx, ack, _ := s.withEditAcknowledgement(t.Context())
			defer ack.finishActual()
			writeCtx, cancel := context.WithCancel(writeCtx)
			defer cancel()
			writeCtx, unlock, err := acquireMutationPath(writeCtx, target)
			require.NoError(t, err)
			defer unlock()
			if noWrite { cancel() }
			later, err := s.commitFileMutation(writeCtx,"write_file","new-owner","","target.go",target,original,0600)
			if noWrite { require.ErrorIs(t, err, errMutationNotApplied) } else { require.NoError(t, err) }
			require.NotNil(t, later)
			status, _ := s.moveFileObservationPayload(ctx, move.records[target].id)
			require.Equal(t, "unavailable", status["physical_commit_state"])
			require.NotContains(t, status,"physical_commit")
			after, err := os.ReadFile(target)
			require.NoError(t,err)
			require.Equal(t,original,after)
		})
	}
}

func TestMovePhysicalStatusRevalidatesAfterUnlockedRendering(t *testing.T) {
	for _, mode := range []string{"evict", "relink", "same-scalars-new-owner"} {
		t.Run(mode, func(t *testing.T) {
			s, move, _, _, _, target := moveObservationExecutionFixture(t)
			timing := move.records[target]
			child := s.movePhysicalStatusChild(timing,"","",func(physical *mutationCommitRecord) map[string]any {
				actual := s.mutationStatusPayload(physical) // proves ledger/timing locks were released
				ledger := &s.mutationCommits
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				timing.mu.Lock()
				defer timing.mu.Unlock()
				switch mode {
				case "evict": delete(ledger.byID,physical.id)
				case "relink": timing.moveFile.physical = weak.Pointer[mutationCommitRecord]{}
				case "same-scalars-new-owner": timing.acknowledgement = &editAcknowledgement{}
				}
				return actual
			})
			require.Nil(t,child,"an earlier valid scalar snapshot cannot survive lost membership or ownership")
		})
	}
}

type movePhysicalStatusExtractor struct {
	parser.Extractor
	active atomic.Bool
	entered chan struct{}
	release chan struct{}
}
func (e *movePhysicalStatusExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult,error) {
	if e.active.Load() {
		select { case e.entered <- struct{}{}: default: }
		<-e.release
	}
	return e.Extractor.Extract(path,raw)
}

func TestMovePhysicalStatusTracksRealQueuedPublicationWithoutReparse(t *testing.T) {
	extractor := &movePhysicalStatusExtractor{Extractor:languages.NewGoExtractor(),entered:make(chan struct{},16),release:make(chan struct{})}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	s,root := setupTestServerWithRegistry(t,registry)
	s.editAckMode = config.EditAckLocal
	source,target := filepath.Join(root,"source.go"),filepath.Join(root,"target.go")
	require.NoError(t,os.WriteFile(source,[]byte("package main\nfunc Before() {}\n"),0600))
	require.NoError(t,os.WriteFile(target,[]byte("package main\nfunc OldTarget() {}\n"),0600))
	_,err := s.indexer.IncrementalReindexPaths(root,[]string{source,target})
	require.NoError(t,err)
	watcher,err := indexer.NewWatcher(s.indexer,config.WatchConfig{Enabled:true,DebounceMs:5},zap.NewNop())
	require.NoError(t,err)
	var release sync.Once
	t.Cleanup(func(){ release.Do(func(){close(extractor.release)}); stopErr := watcher.Stop(); s.DrainBackground(); s.indexer.Close(); require.NoError(t,stopErr) })
	s.SetWatcher(watcher)
	ctx,ack,_ := s.withEditAcknowledgement(canonicalTimingContext())
	ctx,unlock,err := acquireMutationPaths(ctx,[]string{source,target})
	require.NoError(t,err)
	defer unlock()
	move := s.beginMoveObservation(ctx,"source.go::Before","target.go::Before",source,target,"source.go","target.go",true)
	require.NotNil(t,move)
	physical := make(map[string]*mutationCommitRecord)
	physical[target],err = move.write(ctx,s,target,[]byte("package main\nfunc Moved() {}\n"))
	require.NoError(t,err)
	physical[source],err = move.write(ctx,s,source,[]byte("package main\nfunc After() {}\n"))
	require.NoError(t,err)
	move.finishWrites(true)
	extractor.active.Store(true) // only the REAL background indexer is held
	var admissions []sourceMutationAdmission
	for _,path := range []string{source,target} {
		admission := s.admitSourceMutations(ctx,path)
		require.NotNil(t,admission.receipt)
		move.observeAdmission(path,&admission)
		admissions = append(admissions,admission)
	}
	unlock()
	outcomes := s.awaitSourceMutationFrontier(ctx,admissions)
	for i,path := range []string{source,target} { require.True(t,outcomes[i].Pending); physical[path].recordGraph(outcomes[i]) }
	ack.finishActual()
	ack.finishReply(nil,nil)
	select { case <-extractor.entered: case <-time.After(3*time.Second): t.Fatal("real watcher did not enter extraction") }
	for _,path := range []string{source,target} {
		status,_ := s.moveFileObservationPayload(ctx,move.records[path].id)
		require.Equal(t,"observed",status["physical_commit_state"])
		require.Equal(t,"admitted",status["requested_background_state"])
		require.Equal(t,"unavailable_queued_publication",status["direct_reconcile_state"])
		child := status["physical_commit"].(map[string]any)
		require.Equal(t,mutationGraphPending,child["graph_status"])
		require.Equal(t,false,child["graph_status_terminal"])
		require.Equal(t,s.mutationStatusPayload(physical[path]),child)
		require.NotContains(t,status,"graph_status_terminal")
	}
	release.Do(func(){close(extractor.release)})
	for _,path := range []string{source,target} {
		require.Eventually(t,func()bool {
			status,_ := s.moveFileObservationPayload(ctx,move.records[path].id)
			child,ok := status["physical_commit"].(map[string]any)
			return ok && child["graph_status"]==mutationGraphFresh && child["graph_status_terminal"]==true
		},5*time.Second,time.Millisecond)
		status,_ := s.moveFileObservationPayload(ctx,move.records[path].id)
		child := status["physical_commit"].(map[string]any)
		require.Equal(t,s.mutationStatusPayload(physical[path]),child)
		phases,ok := child["publication_phases"].(indexer.PublicationPhaseSnapshot)
		require.True(t,ok)
		require.True(t,phases.Terminal)
		require.Equal(t,indexer.PublicationSourceMCPEdit,phases.Source)
	}
	parses := parser.ParseCount()
	beforeSource,err := os.ReadFile(source);require.NoError(t,err)
	beforeTarget,err := os.ReadFile(target);require.NoError(t,err)
	for range 3 { for _,path := range []string{source,target} { s.moveFileObservationPayload(ctx,move.records[path].id) } }
	require.Equal(t,parses,parser.ParseCount(),"status cannot trigger a second parse/reconcile")
	afterSource,err := os.ReadFile(source);require.NoError(t,err)
	afterTarget,err := os.ReadFile(target);require.NoError(t,err)
	require.Equal(t,beforeSource,afterSource)
	require.Equal(t,beforeTarget,afterTarget)
}

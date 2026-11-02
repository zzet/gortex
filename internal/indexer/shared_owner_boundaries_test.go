package indexer

import (
 "bytes"
 "context"
 "errors"
 "io"
 "path/filepath"
 "sync"
 "testing"
 "time"

 "github.com/stretchr/testify/require"
)

type sharedEntropyFailure struct{}
func (sharedEntropyFailure) Read([]byte) (int,error) { return 0,errors.New("entropy unavailable") }

func TestSharedOwnerConstructorNameFailsClosed(t *testing.T) {
 require.Empty(t,sharedConstructorName(sharedEntropyFailure{}))
 require.Empty(t,sharedConstructorName(io.LimitReader(bytes.NewReader(make([]byte,32)),31)))
 require.Len(t,sharedConstructorName(bytes.NewReader(make([]byte,32))),64)
 first,second:=NewPublicationPhaseRecorder(2,1),NewPublicationPhaseRecorder(2,1)
 require.NotEmpty(t,first.sharedInstance)
 require.NotEqual(t,first.sharedInstance,second.sharedInstance)
 require.Same(t,first,first.sharedSelf.Value())
}

// The legacy finalizer's timestamp argument is captured before its body runs.
// The owner boundary must instead observe the caller's actual drain return,
// including a delayed legacy tail, but excluding projection and outer Done.
func TestSharedOwnerC1IncludesLegacyFinalizationExcludesProjection(t *testing.T) {
 root,_,w:=inertMutationSetWatcher(t,"a.go","package set\nfunc A(){}\n")
 a,b:=filepath.Join(root,"a.go"),filepath.Join(root,"b.go")
 writeTestFile(t,b,"package set\nfunc B(){}\n")
 owner:=NewPublicationPhaseRecorder(8,2)
 first:=owner.Begin("","c1-first",PublicationSourceMCPEdit,time.Now())
 second:=owner.Begin("","c1-second",PublicationSourceMCPEdit,time.Now())
 legacy,projection:=make(chan time.Time,1),make(chan time.Time,1)
 releaseLegacy,releaseProjection:=make(chan struct{}),make(chan struct{})
 var legacyOnce,projectionOnce sync.Once
 w.sharedOwnerLegacyFinalizing=func(at time.Time){legacy<-at;<-releaseLegacy}
 w.sharedOwnerBeforeProjection=func(at time.Time){projection<-at;<-releaseProjection}
 t.Cleanup(func(){legacyOnce.Do(func(){close(releaseLegacy)});projectionOnce.Do(func(){close(releaseProjection)});require.NoError(t,w.Stop())})
 ctx:=WithFilePublicationRecords(context.Background(),map[string]*PublicationPhaseRecord{a:first,b:second})
 tickets,err:=w.EnqueueFileMutations(ctx,[]string{a,b});require.NoError(t,err)
 var old time.Time
 select {case old=<-legacy:case <-time.After(5*time.Second):t.Fatal("legacy finalization not reached")}
 select {case <-projection:t.Fatal("C1 projected before legacy finalization returned");default:}
 released:=time.Now();legacyOnce.Do(func(){close(releaseLegacy)})
 var c1 time.Time
 select {case c1=<-projection:case <-time.After(5*time.Second):t.Fatal("actual drain return not reached")}
 require.False(t,old.After(released))
 require.False(t,c1.Before(released),"caller C1 includes actual legacy tail")
 joined:=make(chan struct{});go func(){w.stormWork.Wait();close(joined)}()
 select {case <-joined:t.Fatal("outer Done preceded excluded projection");default:}
 projectionOnce.Do(func(){close(releaseProjection)})
 select {case <-joined:case <-time.After(5*time.Second):t.Fatal("outer work did not join")}
 for _,ticket:=range tickets {require.NoError(t,awaitMutationSetTicket(t,ticket).Err)}
 require.Nil(t,first.SharedOwnerProof(owner),"unit records have no physical MCP ownership")
}

// Scalar controls are not runtime timing receipts: repeated real collector
// events must remain separate, and overflow must discard the entire vector.
func TestSharedOwnerIntervalCaptureBoundedAndPointUnallocated(t *testing.T) {
 origin:=time.Now()
 for _,limit:=range []int{0,2} {
  laps:=&canonicalPatchLaps{origin:origin,sharedIntervalLimit:limit}
  for n:=0;n<2;n++ {
   start:=origin.Add(time.Duration(n*3+1)*time.Millisecond)
   laps.beginLapLocked(patchParse,start)
   laps.endLapLocked(patchParse,start,start.Add(time.Millisecond))
  }
  if limit==0 {require.Nil(t,laps.sharedIntervals);continue}
  require.Len(t,laps.sharedIntervals,2)
  require.Equal(t,int64(time.Millisecond),laps.sharedIntervals[0].End-laps.sharedIntervals[0].Start)
  require.Less(t,laps.sharedIntervals[0].End,laps.sharedIntervals[1].Start,"do not replace repeated work with first-to-last wall")
  start:=origin.Add(8*time.Millisecond)
  laps.beginLapLocked(patchParse,start);laps.endLapLocked(patchParse,start,start.Add(time.Millisecond))
  require.Negative(t,laps.sharedIntervalLimit)
  require.Nil(t,laps.sharedIntervals,"overflow cannot leave a trimmed proof")
 }
}

func TestSharedOwnerMissingOptionalPhasesStayUnknown(t *testing.T) {
 f:=&sharedOwnerFrozen{members:[]sharedOwnerMember{{}},invocation:&sharedSetInvocation{},backgroundReason:"unobserved"}
 execution:=f.definitionWire()["execution"].(map[string]any)
 states:=execution["phase_states"].(map[string]string)
 phases:=execution["phases_ms"].(map[string]any)
 for _,name:=range []string{"patch_analysis","batch_admission"} {
  require.Equal(t,"not_observed",states[name],"direct entry does not prove optional branch applicability")
  require.Nil(t,phases[name])
 }
}

func TestSharedOwnerBatchApplicabilityUsesActualCoordinatorBranch(t *testing.T) {
 for _,condition:=range []string{"absent","gate","hook","point"} {
  t.Run(condition,func(t *testing.T){
   stamps:=&PublicationStamps{patchLaps:&canonicalPatchLaps{origin:time.Now(),sharedIntervalLimit:32}}
   if condition!="point" {stamps.markDirectSharedOwnerEntry()}
   coordinator:=newRepositoryMutationCoordinator(nil)
   if condition=="gate" {coordinator.batchMutationGate=&sync.RWMutex{}}
   if condition=="hook" {coordinator.batchAdmissionHook=func(){}}
   ctx:=context.WithValue(t.Context(),publicationStampsKey{},stamps)
   require.NoError(t,coordinator.runExclusive(ctx,func()error{return nil}))
   stamps.mu.Lock();defer stamps.mu.Unlock()
   if condition=="absent" {require.Equal(t,"no_batch_gate_or_hook",stamps.patchLaps.sharedOptionalDispositions["batch_admission"])} else {require.Empty(t,stamps.patchLaps.sharedOptionalDispositions)}
   if condition=="gate"||condition=="hook" {require.Equal(t,uint64(1),stamps.patchLaps.laps[patchBatchAdmission].done)}
  })
 }
}

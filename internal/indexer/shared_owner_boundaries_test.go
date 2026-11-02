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

package indexer

import (
 "context"
 "path/filepath"
 "sync"
 "testing"
 "time"

 "github.com/stretchr/testify/require"
)

type stormPathObserver interface { StormActiveForFile(string) bool }

func observesQueuedStorm(observer any, path string) bool {
 scoped,ok := observer.(stormPathObserver)
 return ok && scoped.StormActiveForFile(path)
}

func TestStormActiveForFileReportsOnlyActualQueuedPath(t *testing.T) {
 root,_,w := inertMutationSetWatcher(t,"storm.go","package p\nfunc Storm(){}\n")
 w.config.StormQuietPeriodMs = 60000
 w.config.DebounceMs = 60000
 t.Cleanup(func(){require.NoError(t,w.Stop())})
 path := filepath.Join(root,"storm.go")
 point := filepath.Join(root,"point.go")
 writeTestFile(t,point,"package p\nfunc Point(){}\n")
 require.False(t,observesQueuedStorm(w,path))
 w.recordInStorm(path,ChangeModified) // actual native admission and live quiet timer
 _,err := w.EnqueueFileMutation(context.Background(),point)
 require.NoError(t,err)
 require.True(t,observesQueuedStorm(w,path),"the real queued storm path must have its scoped hint")
 require.False(t,observesQueuedStorm(w,point),"a live unrelated point timer cannot borrow global storm state")
 require.False(t,observesQueuedStorm(w,filepath.Join(root,"unrelated.go")))
 require.False(t,observesQueuedStorm(w,"storm.go"))
 require.True(t,observesQueuedStorm(w,root+string(filepath.Separator)+"unused"+string(filepath.Separator)+".."+string(filepath.Separator)+"storm.go"))
}

func TestStormActiveForFileDetachedWorkRemainsGenericPending(t *testing.T) {
 root,_,w := inertMutationSetWatcher(t,"storm.go","package p\nfunc Storm(){}\n")
 w.config.StormQuietPeriodMs = 1
 path := filepath.Join(root,"storm.go")
 entered,release := make(chan struct{}),make(chan struct{})
 var unblock sync.Once
 w.stormBeforeLock = func(){close(entered);<-release}
 t.Cleanup(func(){unblock.Do(func(){close(release)});require.NoError(t,w.Stop())})
 w.recordInStorm(path,ChangeModified)
 select {case <-entered:case <-time.After(3*time.Second):t.Fatal("real storm callback did not detach")}
 state := w.TierState(path)
 require.NotZero(t,state.Generation)
 require.True(t,state.FileGraphAt.IsZero(),"detached work is still pending")
 require.False(t,observesQueuedStorm(w,path),"queued-only hint cannot invent a mode for detached generic waiters")
 unblock.Do(func(){close(release)})
 w.stormWork.Wait()
 require.False(t,observesQueuedStorm(w,path))
}

func TestMultiWatcherStormActiveForFileUsesStartedPathOwner(t *testing.T) {
 root,idx,w := inertMutationSetWatcher(t,"storm.go","package p\nfunc Storm(){}\n")
 otherRoot,otherIdx,other := inertMutationSetWatcher(t,"point.go","package p\nfunc Point(){}\n")
 w.config.StormQuietPeriodMs = 60000
 other.config.DebounceMs = 60000
 t.Cleanup(func(){require.NoError(t,w.Stop());require.NoError(t,other.Stop())})
 path,point := filepath.Join(root,"storm.go"),filepath.Join(otherRoot,"point.go")
 w.recordInStorm(path,ChangeModified)
 _,err := other.EnqueueFileMutation(context.Background(),point)
 require.NoError(t,err)
 mi := &MultiIndexer{repos:map[string]*RepoMetadata{"storm":{RepoPrefix:"storm",RootPath:root},"point":{RepoPrefix:"point",RootPath:otherRoot}},indexers:map[string]*Indexer{"storm":idx,"point":otherIdx}}
 mw := &MultiWatcher{multi:mi,watchers:map[string]*Watcher{"storm":w,"point":other},started:map[string]bool{"storm":true,"point":true}}
 require.True(t,observesQueuedStorm(mw,path))
 require.False(t,observesQueuedStorm(mw,point))
 require.False(t,observesQueuedStorm(mw,filepath.Join(t.TempDir(),"unknown.go")))
 mw.mu.Lock();mw.started["storm"]=false;mw.mu.Unlock()
 require.False(t,observesQueuedStorm(mw,path))
 mw.mu.Lock();mw.started["storm"]=true;mw.stopped=true;mw.mu.Unlock()
 require.False(t,observesQueuedStorm(mw,path))
 require.False(t,observesQueuedStorm((*Watcher)(nil),path))
 require.False(t,observesQueuedStorm((*MultiWatcher)(nil),path))
}

func TestStormActiveForFileRefusesSupersededQueuedGeneration(t *testing.T) {
 root,_,w := inertMutationSetWatcher(t,"storm.go","package p\nfunc Storm(){}\n")
 w.config.StormQuietPeriodMs = 60000
 w.config.DebounceMs = 60000
 t.Cleanup(func(){require.NoError(t,w.Stop())})
 path := filepath.Join(root,"storm.go")
 w.recordInStorm(path,ChangeModified)
 require.True(t,observesQueuedStorm(w,path))
 older := w.TierState(path).Generation
 ticket,err := w.EnqueueFileMutation(context.Background(),path)
 require.NoError(t,err)
 require.Greater(t,ticket.Generation,older)
 require.False(t,observesQueuedStorm(w,path),"an older queued storm cannot donate a reason to a newer point owner")
}

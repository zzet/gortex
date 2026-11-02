//go:build linux || darwin

package main

import (
 "os"
 "path/filepath"
 "strings"
 "testing"

 "github.com/stretchr/testify/assert"
 "github.com/stretchr/testify/require"
)

func discoveryRuntimeDestination(t *testing.T,o runOptions)string{
 t.Helper()
 scratch,err:=canonicalPath(o.Scratch);require.NoError(t,err)
 return filepath.Join(scratch,"load-runtime")
}

func discoverySourceHashes(t *testing.T,root string)map[string]string{
 t.Helper()
 out:=map[string]string{}
 require.NoError(t,filepath.WalkDir(root,func(path string,entry os.DirEntry,err error)error{
  if err!=nil{return err}
  if entry.Type().IsRegular(){sha,err:=discoveryFileSHA(path);if err!=nil{return err};out[path]=sha}
  return nil
 }))
 return out
}

func TestDiscoveryRuntimeRootAllowsRetainedSourceStore(t *testing.T){
 o,corpora,l:=syntheticDiscoverySeed(t)
 originalStore:=filepath.Join(l.OwnedRoot,"store.db")
 require.NoError(t,os.WriteFile(originalStore,[]byte("original retained fixture store: do not touch"),0600))
 require.NoError(t,discoveryWriteJSON(o.Discovery.Resume,l))
 o.Discovery.RuntimeRoot=discoveryRuntimeDestination(t,o)
 before:=discoverySourceHashes(t,l.OwnedRoot)
 p,err:=resumeDiscoveryState(t.Context(),o,corpora)
 require.NoError(t,err,"fresh runtime must not collide with retained source store")
 t.Cleanup(func(){_ = os.RemoveAll(p.SocketDir)})
 require.Equal(t,o.Discovery.RuntimeRoot,p.Root)
 require.Equal(t,filepath.Join(p.Root,"store.db"),p.Store)
 require.Equal(t,filepath.Join(p.Root,"config.yaml"),p.Config)
 for i,repo:=range p.Repos{require.Equal(t,l.Repos[i].Repo,repo,"fixed repository/catalog identity may not relocate")}
 for _,rel:=range []string{"config.yaml","gitconfig","config/gortex/config.yaml"}{
  source,err:=os.ReadFile(filepath.Join(l.OwnedRoot,rel));require.NoError(t,err)
  copied,err:=os.ReadFile(filepath.Join(p.Root,rel));require.NoError(t,err)
  require.Equal(t,source,copied,"configuration must be a byte copy")
 }
 require.Contains(t,p.Env,"HOME="+filepath.Join(p.Root,"home"))
 require.Contains(t,p.Env,"XDG_CONFIG_HOME="+filepath.Join(p.Root,"config"))
 require.Contains(t,p.Env,"GIT_CONFIG_GLOBAL="+filepath.Join(p.Root,"gitconfig"))
 require.FileExists(t,filepath.Join(p.Root,"discovery-consumed.json"))
 require.NoFileExists(t,filepath.Join(l.OwnedRoot,"discovery-consumed.json"))
 require.NoError(t,p.removeState())
 require.Equal(t,before,discoverySourceHashes(t,l.OwnedRoot),"resume/config/cleanup must leave the source owner untouched")
 _,err=resumeDiscoveryState(t.Context(),o,corpora)
 require.Error(t,err,"fresh runtime path cannot be reused after cleanup")
 require.Equal(t,before,discoverySourceHashes(t,l.OwnedRoot))
}

func TestDiscoveryRuntimeCleanupNeverDeletesSourceRepos(t *testing.T){
 // Deliberate regression control on fresh disposable unit clones ONLY. The
 // previous root alias would delete these source repos during cleanup.
 o,corpora,l:=syntheticDiscoverySeed(t)
 require.NoError(t,discoveryWriteJSON(o.Discovery.Resume,l))
 o.Discovery.RuntimeRoot=discoveryRuntimeDestination(t,o)
 before:=discoverySourceHashes(t,l.OwnedRoot)
 p,err:=resumeDiscoveryState(t.Context(),o,corpora);require.NoError(t,err)
 t.Cleanup(func(){_ = os.RemoveAll(p.SocketDir)})
 assert.Equal(t,o.Discovery.RuntimeRoot,p.Root)
 require.NoError(t,p.removeState())
 assert.DirExists(t,l.Repos[0].Repo.Path,"cleanup must never own the source repo tree")
 assert.Equal(t,before,discoverySourceHashes(t,l.OwnedRoot))
}

func TestDiscoveryRuntimeRootFlagRequiresResume(t *testing.T){
 require.Error(t,(discoveryOptions{RuntimeRoot:"/tmp/load-fresh"}).validate(checkoutDiscoveryScenario,1))
 require.Error(t,(discoveryOptions{RuntimeRoot:"/tmp/load-fresh",Resume:"lineage"}).validate("read",1))
 require.NoError(t,(discoveryOptions{RuntimeRoot:"/tmp/load-fresh",Resume:"lineage"}).validate(checkoutDiscoveryScenario,1))
 args:=[]string{"--binary","unused","--manifest","unused","--scratch",t.TempDir(),"--scenario",checkoutDiscoveryScenario,"--discovery-resume","lineage","--discovery-runtime-root","/tmp/load-fresh"}
 got,err:=parseRun(args);require.NoError(t,err)
 require.Equal(t,"/tmp/load-fresh",got.Discovery.RuntimeRoot)
}

func TestDiscoveryRuntimeRootRefusesInvalidDestination(t *testing.T){
 for _,name:=range []string{"outside","source","nested_source","ancestor_source","reuse","symlink","wrong_identity"}{
  t.Run(name,func(t *testing.T){
   o,corpora,l:=syntheticDiscoverySeed(t)
   root:=discoveryRuntimeDestination(t,o)
   switch name{
   case "outside":root=filepath.Join(t.TempDir(),"load-runtime")
   case "source":root=l.OwnedRoot
   case "nested_source":root=filepath.Join(l.OwnedRoot,"load-runtime")
   case "ancestor_source":root=o.Scratch
   case "reuse":require.NoError(t,os.Mkdir(root,0700))
   case "symlink":require.NoError(t,os.Symlink(t.TempDir(),root))
   case "wrong_identity":l.Repos[0].CommonDir=strings.Repeat("x",10)
   }
   require.NoError(t,discoveryWriteJSON(o.Discovery.Resume,l))
   o.Discovery.RuntimeRoot=root
   before:=discoverySourceHashes(t,l.OwnedRoot)
   p,err:=resumeDiscoveryState(t.Context(),o,corpora)
   if p!=nil{t.Cleanup(func(){_ = os.RemoveAll(p.SocketDir)})}
   require.Error(t,err)
   require.Equal(t,before,discoverySourceHashes(t,l.OwnedRoot))
   require.NoFileExists(t,filepath.Join(l.OwnedRoot,"discovery-consumed.json"))
  })
 }
}

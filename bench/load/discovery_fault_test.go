//go:build linux || darwin

package main

import (
 "context"
 "bytes"
 "os"
 "os/exec"
 "path/filepath"
 "strconv"
 "strings"
 "syscall"
 "testing"
 "time"

 "github.com/stretchr/testify/require"
)

func TestDiscoveryFaultRetainsOwnedPipeAndDoesNotAffectHarnessGit(t *testing.T){
 first,second:=gitFixture(t),gitFixture(t);second.Label="other"
 p,err:=newPrivateRun(t.Context(),t.TempDir(),"unused",[]Corpus{first,second});require.NoError(t,err)
 t.Cleanup(func(){_=os.RemoveAll(p.SocketDir)})
 originalEnv:=append([]string(nil),p.Env...)
 release,files,err:=p.installDiscoveryFault();require.NoError(t,err)
 t.Cleanup(func(){_=release()})
 require.Equal(t,originalEnv,p.Env,"harness environment remains unchanged")
 require.NotEqual(t,p.Env,p.daemonEnv)
 require.NoError(t,p.addDiscoveryWorktree(t.Context(),0),"harness worktree creation must never enter its daemon shim")
 linked:=p.Repos[len(p.Repos)-1]
 ctx,cancel:=context.WithTimeout(t.Context(),10*time.Second);defer cancel()
 cmd:=exec.CommandContext(ctx,files["discovery_git_shim"],"-C",linked.Path,"rev-parse","--show-toplevel")
 configureOwnedProcess(cmd);cmd.Cancel=func()error{return killOwnedProcess(cmd)}
 var output bytes.Buffer
 cmd.Stdout,cmd.Stderr=&output,&output
 cmd.WaitDelay=3*time.Second
 require.NoError(t,cmd.Start())
 done:=make(chan error,1);go func(){done<-cmd.Wait()}()
 joined:=false
 t.Cleanup(func(){_=release();if !joined{_=killOwnedProcess(cmd);<-done}})
 var child int
 require.Eventually(t,func()bool{raw,err:=os.ReadFile(files["discovery_git_events"]);if err!=nil{return false};fields:=strings.Fields(string(raw));if len(fields)!=3{return false};child,err=strconv.Atoi(fields[0]);return err==nil&&fields[1]==linked.Path&&fields[2]=="rev-parse"},3*time.Second,5*time.Millisecond)
 require.NoError(t,cmd.Process.Kill(),"cancelled Git parent exits while descendant retains its stdout pipe")
 require.NoError(t,syscall.Kill(child,syscall.SIGTERM),"TERM goes only to recorded owned descendant")
 select{case err:=<-done:joined=true;t.Fatalf("stall unexpectedly exited after TERM: %v",err);case <-time.After(30*time.Millisecond):}
 require.NoError(t,release())
 select{case err:=<-done:joined=true;require.Error(t,err);case <-ctx.Done():t.Fatal("released owned shim did not join")}
 _,err=p.git(t.Context(),p.Repos[1].Path,"rev-parse","--show-toplevel");require.NoError(t,err)
 _,err=os.Stat(filepath.Join(p.Root,"discovery-git-fault","release"));require.NoError(t,err)
}

func TestDiscoveryStallOptionsRequireActualAdditions(t *testing.T){
 base:=[]string{"--binary","unused","--manifest","unused","--scratch",t.TempDir(),"--agents","2","--edit-agents","0","--discovery-stall-first"}
 _,err:=parseRun(append(base,"--scenario",checkoutDiscoveryScenario,"--worktrees","2"));require.NoError(t,err)
 _,err=parseRun(append(base,"--scenario",checkoutDiscoveryScenario,"--worktrees","0"));require.Error(t,err)
 _,err=parseRun(append(base,"--scenario","read"));require.Error(t,err)
}

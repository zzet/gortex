//go:build darwin || linux || windows

package main

import (
 "bytes"
 "os"
 "sync/atomic"
 "testing"

 "github.com/stretchr/testify/require"
 "github.com/zzet/gortex/internal/platform"
)

// A no-store daemon may exit between the initial completion observation and
// the post-grace free-store check. A free result alone must not bypass the
// captured runtime completion proof, nor turn completed cleanup into failure.
func TestDaemonStopRechecksCompletionWhenLegacyOwnerExitsBetweenPolls(t *testing.T) {
 f:=&fakeClosingDaemon{}
 f.install(t,os.Getpid())
 target,err:=captureDaemonStopTarget(os.Getpid())
 require.NoError(t,err)
 require.True(t,target.noStore)
 calls:=0
 daemonCapturedProcessRunning=func(platform.ProcessIdentity)(bool,error){
  calls++
  if calls==1 {return true,nil}
  f.exited.Store(true)
  return false,nil
 }
 var out bytes.Buffer
 result,err:=waitForDaemonTarget(&out,target,0,false)
 require.NoError(t,err,"a second captured completion check must observe the finished legacy owner")
 require.False(t,result.forced)
 require.False(t,f.killed.Load(),"completion cannot authorize a signal")
 gone,err:=target.runtimeGone()
 require.NoError(t,err)
 require.True(t,gone,"successful stop requires actual captured runtime cleanup")
}

// The recheck must not confuse lock release with completion when the exact
// captured process and runtime are both still present.
func TestDaemonStopFreeStoreWithLiveRuntimeStillRefusesWithoutSignal(t *testing.T) {
 target,exit,wait,unlock:=ownedStopTarget(t)
 require.NoError(t,unlock())
 previous:=daemonKillCapturedProcess
 var kills atomic.Int32
 daemonKillCapturedProcess=func(platform.ProcessIdentity)error{kills.Add(1);return nil}
 t.Cleanup(func(){daemonKillCapturedProcess=previous})
 var out bytes.Buffer
 result,err:=waitForDaemonTarget(&out,target,0,false)
 require.ErrorContains(t,err,"captured runtime cleanup has not completed")
 require.False(t,result.forced)
 require.Zero(t,kills.Load())
 gone,err:=target.runtimeGone();require.NoError(t,err);require.False(t,gone)
 running,err:=target.process.Running();require.NoError(t,err);require.True(t,running)
 require.NoError(t,exit());require.NoError(t,wait())
}

package main

import (
 "bufio"
 "context"
 "encoding/json"
 "net"
 "sync"
 "testing"
 "time"

 "github.com/stretchr/testify/require"
)

// Actual framed transport, not a fabricated last-response cache. This test
// retains the first refused frame separately from the final successful frame.
func TestReadOnlyFreshnessRetryKeepsFirstWireAndLogicalCost(t *testing.T) {
 for _,code:=range []string{"projection_superseded","file_view_pending"}{t.Run(code,func(t *testing.T){
  left,right:=net.Pipe();client:=&socketClient{conn:left,reader:bufio.NewReader(left),writeGate:make(chan struct{},1),pending:make(map[int64]chan wireReply),done:make(chan struct{}),cwd:"/owned/private"};go client.readLoop()
  var peer sync.WaitGroup;peer.Add(1);var requests,responses [][]byte
  go func(){defer peer.Done();reader:=bufio.NewReader(right)
   for i:=0;i<2;i++{raw,err:=readWireLine(reader);if err!=nil{return};requests=append(requests,raw);var request struct{ID int64 `json:"id"`};if json.Unmarshal(raw,&request)!=nil{return}
    result:=map[string]any{"content":[]any{map[string]any{"type":"text","text":"{\"file\":{\"id\":\"main.go\"},\"defines\":[]}"}}}
    if i==0 {body,_:=json.Marshal(map[string]any{"error_code":code,"message":"transient selected read unavailable","retriable":true,"data":map[string]any{"retry_scope":"read_only","delivery_unknown":false}});result=map[string]any{"isError":true,"content":[]any{map[string]any{"type":"text","text":string(body)}}}}
    frame,_:=json.Marshal(map[string]any{"jsonrpc":"2.0","id":request.ID,"result":result});frame=append(frame,'\n');responses=append(responses,frame);if _,err:=right.Write(frame);err!=nil{return}
   }
  }()
  closed:=false;closeAndJoin:=func(){if !closed{closed=true;_ =left.Close();_ =right.Close();<-client.done;peer.Wait()}};t.Cleanup(closeAndJoin)
  args:=map[string]any{"operation":"editing_context","target":map[string]any{"file":"main.go"},"output":map[string]any{"format":"json","max_bytes":0}}
  r:=request{Tool:"read",Operation:"editing_context",Arguments:args}
  started:=time.Now();result,attempts,err:=callTool(t.Context(),client,r,args,"workload",3,0,1,time.Second,waitContext)
  closeAndJoin()
  t.Logf("ACTUAL_FRAMED_TRANSIENT_WITNESS code=%s requests=%d responses=%d original_errors_retained=%+v",code,len(requests),len(responses),attempts)
  require.NoError(t,err,"explicit read-only contract must allow a bounded fresh RPC within the original logical deadline")
  require.Len(t,requests,2);require.Len(t,responses,2);require.Len(t,attempts,2)
  require.Equal(t,"retryable",attempts[0].Outcome);require.Equal(t,-32000,attempts[0].Code);require.NotEmpty(t,attempts[0].Error)
  require.Equal(t,"success",attempts[1].Outcome);require.Equal(t,1,attempts[1].Retry);require.Equal(t,attempts[0].Request,attempts[1].Request)
  require.GreaterOrEqual(t,attempts[1].LogicalMS,attempts[0].DurationMS);require.Less(t,time.Since(started),time.Second)
  require.Contains(t,string(responses[0]),"isError");require.NotEmpty(t,result)
  metric:=summarize(attempts,1,"workload")["read.editing_context"];require.Equal(t,1,metric.RetryableRefusals);require.Equal(t,1,metric.Successes)
 })}
}
